#!/usr/bin/env python3
"""Run one isolated workflow-study cell with DeepSeek tool calling.

The adapter reads one registered cell from stdin and emits exactly one run
record on stdout. Secrets are read from the process environment or the
repository's ignored .env file and are never included in the run record.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import http.cookiejar
import json
import os
import re
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

import controller
import validate


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
COMPOSE_FILES = (ROOT / "compose.yaml", HERE / "compose.yaml")
COMMON_ERROR = {"error": {"code": "STUDY_BUDGET_EXHAUSTED", "envelope": "taskgate-study-budget-rejection-v1", "retryable": False}}
CSRF = re.compile(rb'name="csrf" value="([^"]+)"')
AUDIT_BUDGET = {
    "max_queries": 100, "max_rows": 5000, "max_release_facts": 100000,
    "max_influence_facts": 1000000, "max_outcome_facts": 100,
}


class ToolFailure(RuntimeError):
    pass


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def read_dotenv(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    if not path.is_file():
        return values
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[key.strip()] = value
    return values


def secret(name: str, dotenv: dict[str, str]) -> str:
    value = os.getenv(name) or dotenv.get(name, "")
    if not value:
        raise ValueError(f"required secret {name} is not configured")
    return value


def free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def compose_command(project: str, *arguments: str) -> list[str]:
    return ["docker", "compose", "-p", project, "-f", str(COMPOSE_FILES[0]), "-f", str(COMPOSE_FILES[1]), *arguments]


def run_command(argv: list[str], env: dict[str, str], timeout: int = 900) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(argv, cwd=ROOT, env=env, text=True, capture_output=True, timeout=timeout, check=False)
    if completed.returncode:
        message = completed.stderr.strip().splitlines()[-1:] or completed.stdout.strip().splitlines()[-1:]
        raise RuntimeError(f"command failed ({argv[0]} {argv[1]}): {' '.join(message)}")
    return completed


def http_json(url: str, payload: dict, headers: dict[str, str], timeout: int = 180) -> dict:
    request = urllib.request.Request(
        url, data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json", **headers}, method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as error:
        body = error.read(4096).decode("utf-8", "replace")
        raise RuntimeError(f"HTTP {error.code} from {urllib.parse.urlparse(url).netloc}: {body}") from error


class MCP:
    def __init__(self, url: str, token: str) -> None:
        self.url = url
        self.token = token
        self.counter = 0

    def call(self, name: str, arguments: dict) -> tuple[dict, int]:
        self.counter += 1
        started = time.monotonic()
        rpc = http_json(
            self.url,
            {"jsonrpc": "2.0", "id": self.counter, "method": "tools/call", "params": {"name": name, "arguments": arguments}},
            {"Authorization": "Bearer " + self.token, "Accept": "application/json"},
        )
        elapsed = round((time.monotonic() - started) * 1000)
        if rpc.get("error"):
            raise ToolFailure(json.dumps(rpc["error"], ensure_ascii=False))
        result = rpc.get("result", {})
        if result.get("isError"):
            content = result.get("content", [])
            message = content[0].get("text", "tool returned isError=true") if content else "tool returned isError=true"
            raise ToolFailure(message)
        structured = result.get("structuredContent")
        if not isinstance(structured, dict):
            raise ToolFailure("tool omitted structuredContent")
        return structured, elapsed


def oa_client(base_url: str, username: str, password: str) -> urllib.request.OpenerDirector:
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    with opener.open(base_url + "/login", timeout=30) as response:
        match = CSRF.search(response.read())
    if not match:
        raise RuntimeError("OA login page omitted CSRF token")
    body = urllib.parse.urlencode({"csrf": match.group(1).decode(), "username": username, "password": password}).encode()
    with opener.open(urllib.request.Request(base_url + "/login", data=body, method="POST"), timeout=30):
        pass
    return opener


def oa_action(opener: urllib.request.OpenerDirector, base_url: str, draft_id: str, action: str, decision: str = "") -> None:
    task_url = base_url + "/tasks/" + urllib.parse.quote(draft_id, safe="")
    with opener.open(task_url, timeout=30) as response:
        match = CSRF.search(response.read())
    if not match:
        raise RuntimeError("OA task page omitted CSRF token")
    values = {"csrf": match.group(1).decode()}
    if decision:
        values["decision"] = decision
    body = urllib.parse.urlencode(values).encode()
    with opener.open(urllib.request.Request(task_url + "/" + action, data=body, method="POST"), timeout=30):
        pass


def answer_contract(tasks_doc: dict) -> dict:
    return {
        task["id"]: sorted({item["answer_path"].split(".")[0] for item in task["rubric"] if item.get("answer_path")})
        for task in tasks_doc["tasks"]
    }


def validate_lock(path: Path, invocation: dict, tasks_doc: dict) -> dict:
    lock = validate.validate_execution_lock(path, invocation["study_id"])
    if hashlib.sha256(path.read_bytes()).hexdigest() != invocation["execution_lock_sha256"]:
        raise ValueError("execution-lock file does not match the registered invocation")
    if lock["provider"] != "deepseek":
        raise ValueError("execution lock provider must be deepseek")
    expected = {
        "system_prompt_sha256": hashlib.sha256((HERE / "system-prompt.txt").read_bytes()).hexdigest(),
        "tool_surface_sha256": hashlib.sha256((HERE / "agent-tool-surface.json").read_bytes()).hexdigest(),
        "agent_adapter_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "answer_schema_sha256": validate.canonical_sha256(answer_contract(tasks_doc)),
    }
    for field, digest in expected.items():
        if lock[field] != digest:
            raise ValueError(f"execution lock {field} does not match source")
    return lock


def requested_budget(invocation: dict) -> dict:
    if invocation["arm"] == "taskgate_v3":
        budget = invocation["budget"]
        return {
            "max_queries": 100, "max_rows": 5000,
            "max_release_facts": budget["release_facts"],
            "max_influence_facts": budget["influence_facts"],
            "max_outcome_facts": budget["outcome_facts"],
        }
    return dict(AUDIT_BUDGET)


def visible_query_result(result: dict) -> dict:
    return {key: result.get(key) for key in ("columns", "rows", "row_count", "limited")}


def parse_final(content: str) -> tuple[dict, str]:
    stripped = content.strip()
    if stripped.startswith("```"):
        stripped = re.sub(r"^```(?:json)?\s*|\s*```$", "", stripped, flags=re.IGNORECASE)
    value = json.loads(stripped)
    if set(value) != {"answer", "narrative"} or not isinstance(value["answer"], dict) or not isinstance(value["narrative"], str):
        raise ValueError("final model output violates the answer envelope")
    return value["answer"], value["narrative"]


def call_deepseek(messages: list[dict], tools: list[dict], lock: dict, api_key: str) -> dict:
    base = os.getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com").rstrip("/")
    response = http_json(
        base + "/chat/completions",
        {"model": lock["model"], "messages": messages, "tools": tools, "tool_choice": "auto", "temperature": lock["temperature"]},
        {"Authorization": "Bearer " + api_key},
        timeout=int(os.getenv("DEEPSEEK_TIMEOUT_SECONDS", "300")),
    )
    choices = response.get("choices", [])
    if not choices or not isinstance(choices[0].get("message"), dict):
        raise RuntimeError("DeepSeek response omitted an assistant message")
    return choices[0]["message"]


def sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def audit_facts(project: str, admitted_query_ids: list[str], env: dict[str, str]) -> list[dict]:
    if not admitted_query_ids:
        return []
    ids = ",".join(sql_literal(value) for value in admitted_query_ids)
    sql = f"""
SELECT json_build_object('ledger_kind', linked.ledger_kind, 'fact_sha256', linked.fact_sha256,
                         'identity', facts.identity_json)::text
FROM (SELECT DISTINCT root_task_id, ledger_kind, fact_sha256 FROM query_exposure_facts
      WHERE query_id IN ({ids})) linked
JOIN exposure_facts facts USING (root_task_id, ledger_kind, fact_sha256)
ORDER BY linked.ledger_kind, linked.fact_sha256;
"""
    output = run_command(
        compose_command(project, "exec", "-T", "control-postgres", "psql", "-U", "postgres", "-d", env.get("CONTROL_POSTGRES_DB", "taskbound_gateway"), "-At", "-c", sql),
        env,
    ).stdout
    return [json.loads(line) for line in output.splitlines() if line.strip()]


def exposure_storage_bytes(project: str, env: dict[str, str]) -> int:
    sql = "SELECT pg_total_relation_size('exposure_facts') + pg_total_relation_size('query_exposure_facts');"
    output = run_command(
        compose_command(project, "exec", "-T", "control-postgres", "psql", "-U", "postgres", "-d", env.get("CONTROL_POSTGRES_DB", "taskbound_gateway"), "-At", "-c", sql),
        env,
    ).stdout.strip()
    return int(output)


def risk_metrics(facts: list[dict], task: dict) -> dict:
    sensitivity = validate.load(HERE / "sensitivity-map.json")
    essentials = validate.load(HERE / "essential-columns.json")["tasks"][task["id"]]
    weights = sensitivity["weights"]
    namespaces = sensitivity["namespaces"]
    sensitive_records: set[str] = set()
    sensitive_fields: set[str] = set()
    unnecessary: set[str] = set()
    weighted = 0
    for row in facts:
        identity = row["identity"]
        kind = identity.get("kind")
        namespace = identity.get("source_namespace", "")
        definition = namespaces.get(namespace, {})
        product = definition.get("product", "")
        level = definition.get("default", task["sensitivity"])
        if kind == "base-cell":
            level = definition.get("fields", {}).get(identity.get("field"), level)
        elif kind == "derived":
            levels = [namespaces.get(item.get("source_namespace", ""), {}).get("default", "low") for item in identity.get("snapshot_bundle", [])]
            level = max(levels, key=lambda item: weights[item]) if levels else task["sensitivity"]
        elif kind == "outcome":
            level = task["sensitivity"]
        weight = weights[level]
        weighted += weight
        if weight > 1 and identity.get("entity_key"):
            sensitive_records.add(namespace + "\0" + identity["entity_key"])
        if weight > 1 and identity.get("field"):
            field_key = namespace + "\0" + identity["field"]
            sensitive_fields.add(field_key)
            if identity["field"] not in essentials.get(product, []):
                unnecessary.add(field_key)
    counts = {kind: sum(row["ledger_kind"] == kind for row in facts) for kind in ("RELEASE", "INFLUENCE", "OUTCOME")}
    return {
        "release_facts": counts["RELEASE"], "influence_facts": counts["INFLUENCE"],
        "outcome_facts": counts["OUTCOME"], "sensitivity_weighted_exposure": weighted,
        "distinct_sensitive_records": len(sensitive_records), "distinct_sensitive_fields": len(sensitive_fields),
        "unnecessary_sensitive_fields": len(unnecessary),
    }


def run_agent(invocation: dict, mcp: MCP, task: dict, lock: dict, api_key: str) -> tuple[dict, str, list[dict], list[str], dict, int, int]:
    tools = validate.load(HERE / "agent-tool-surface.json")
    required_fields = answer_contract({"tasks": [task]})[task["id"]]
    system = (HERE / "system-prompt.txt").read_text(encoding="utf-8")
    user = {
        "evaluation_replicate": invocation["seed"], "task_id": task["id"], "business_request": task["prompt"],
        "authorized_products": task["products"], "authorized_columns": task["approved_columns"],
        "mandatory_scope": task["scope"], "required_answer_fields": required_fields,
        "query_plan_guide": {
            "single_product": {"product": "authorized_product", "columns": ["field"], "filters": [{"column": "field", "op": "=", "value": "value"}], "limit": 100},
            "aggregate": {"product": "authorized_product", "columns": [], "aggregates": [{"function": "count", "column": "*", "alias": "n"}], "group_by": []},
            "join": {"from": {"join": {"left": {"product": "left_product", "role": "l", "filters": []}, "right": {"product": "right_product", "role": "r", "filters": []}, "on": [{"left": "l.key", "right": "r.key"}]}}, "columns": ["l.field", "r.field"]},
            "notes": "Use qualified role.column names for joins. Scope filters are injected by TaskGate. date_trunc and to_char are allowed only where the catalog permits them."
        },
    }
    messages = [{"role": "system", "content": system}, {"role": "user", "content": json.dumps(user, ensure_ascii=False)}]
    baseline = None
    if invocation["arm"] in controller.BASELINE_ARMS:
        unit = next(iter(invocation["budget"].values()))
        baseline = controller.BaselineController(invocation["arm"], unit)
    queries: list[dict] = []
    admitted_query_ids: list[str] = []
    successful_queries = returned_rows = serialized_bytes = 0
    gateway_ms = accounting_ms = 0
    runtime_rejections = 0
    task_id = invocation["root_task_id"]
    for turn in range(16):
        message = call_deepseek(messages, tools, lock, api_key)
        messages.append(message)
        calls = message.get("tool_calls") or []
        if not calls:
            answer, narrative = parse_final(message.get("content") or "")
            return answer, narrative, queries, admitted_query_ids, {
                "successful_queries": successful_queries, "returned_rows": returned_rows,
                "serialized_bytes": serialized_bytes, "runtime_budget_rejections": runtime_rejections,
            }, gateway_ms, accounting_ms
        for call in calls:
            name = call.get("function", {}).get("name")
            try:
                arguments = json.loads(call.get("function", {}).get("arguments") or "{}")
            except json.JSONDecodeError:
                result_for_model = {"error": {"code": "INVALID_TOOL_ARGUMENTS", "retryable": True}}
            else:
                if name == "get_budget":
                    try:
                        raw, elapsed = mcp.call("get_budget", {"task_id": task_id})
                        gateway_ms += elapsed
                        result_for_model = raw
                    except ToolFailure as error:
                        result_for_model = {"error": {"code": "TOOL_ERROR", "message": str(error)}}
                elif name == "execute_plan":
                    request_id = f"study-{invocation['run_id']}-{len(queries) + 1}"
                    tool_arguments = {"task_id": task_id, "request_id": request_id, "plan": arguments.get("plan")}
                    if arguments.get("output_format"):
                        tool_arguments["output_format"] = arguments["output_format"]
                    entry = {"request_id": request_id, "plan": arguments.get("plan"), "admitted": False}
                    try:
                        raw, elapsed = mcp.call("execute_plan", tool_arguments)
                        gateway_ms += elapsed
                        successful_queries += 1
                        visible = visible_query_result(raw)
                        payload = controller.canonical_response_bytes(visible)
                        rows = int(raw.get("row_count", 0))
                        returned_rows += rows
                        serialized_bytes += len(payload)
                        component = raw.get("component_ms", {})
                        accounting_ms += sum(int(component.get(key, 0)) for key in (
                            "exposure_derivation", "exposure_reservation_lock", "exposure_ledger_lock", "exposure_fact_store",
                        ))
                        admitted = True
                        if baseline is not None:
                            admitted = baseline.admit(request_id, rows, payload).released
                        if admitted:
                            entry["admitted"] = True
                            entry["query_id"] = raw.get("query_id")
                            admitted_query_ids.append(raw["query_id"])
                            result_for_model = visible
                        else:
                            runtime_rejections += 1
                            result_for_model = COMMON_ERROR
                        entry.update({"row_count": rows, "serialized_bytes": len(payload)})
                    except ToolFailure as error:
                        message_text = str(error)
                        if "EXPOSURE_BUDGET_EXHAUSTED" in message_text or "暴露预算" in message_text or "预算" in message_text:
                            runtime_rejections += 1
                            result_for_model = COMMON_ERROR
                            entry["budget_rejected"] = True
                        else:
                            result_for_model = {"error": {"code": "TOOL_ERROR", "message": message_text}}
                            entry["tool_error"] = True
                    queries.append(entry)
                else:
                    result_for_model = {"error": {"code": "UNKNOWN_TOOL", "retryable": False}}
            messages.append({"role": "tool", "tool_call_id": call.get("id", ""), "content": json.dumps(result_for_model, ensure_ascii=False)})
    raise RuntimeError("DeepSeek exceeded the 16-turn tool limit")


def main() -> None:
    invocation = json.load(sys.stdin)
    tasks_doc, protocol = validate.validate_design()
    tasks = {task["id"]: task for task in tasks_doc["tasks"]}
    if invocation.get("study_id") != protocol["study_id"] or invocation.get("task_id") not in tasks:
        raise SystemExit("invalid workflow-study invocation")
    dotenv = read_dotenv(ROOT / ".env")
    api_key = secret("DEEPSEEK_API_KEY", dotenv)
    lock_path = Path(os.getenv("WORKFLOW_EXECUTION_LOCK", ""))
    if not lock_path.is_file():
        raise SystemExit("WORKFLOW_EXECUTION_LOCK must name the frozen execution-lock JSON")
    lock = validate_lock(lock_path, invocation, tasks_doc)
    project = re.sub(r"[^a-z0-9-]", "-", invocation["run_id"].lower())[:55]
    ports = {name: free_port() for name in ("BUSINESS", "CONTROL", "GATEWAY", "OA")}
    env = dict(os.environ)
    env.update(dotenv)
    env.update({f"WORKFLOW_STUDY_{name}_PORT": str(port) for name, port in ports.items()})
    started_at = now()
    wall_started = time.monotonic()
    queries: list[dict] = []
    try:
        run_command(compose_command(project, "up", "-d", "--wait"), env)
        mcp = MCP(f"http://127.0.0.1:{ports['GATEWAY']}/mcp", secret("TASKBOUND_ALICE_TOKEN", dotenv))
        created, _ = mcp.call("request_data_task", {
            "objective": tasks[invocation["task_id"]]["prompt"],
            "data_products": tasks[invocation["task_id"]]["products"],
            "columns": tasks[invocation["task_id"]]["approved_columns"],
            "scopes": tasks[invocation["task_id"]]["scope"],
            "requested_budget": requested_budget(invocation),
        })
        invocation["root_task_id"] = created["task_id"]
        draft_id = created["oa_url"].rstrip("/").split("/")[-1]
        oa_url = f"http://127.0.0.1:{ports['OA']}"
        alice = oa_client(oa_url, "alice", secret("OA_ALICE_PASSWORD", dotenv))
        bob = oa_client(oa_url, "bob", secret("OA_BOB_PASSWORD", dotenv))
        oa_action(alice, oa_url, draft_id, "submit")
        for _ in range(40):
            pending, _ = mcp.call("get_task_status", {"task_id": created["task_id"]})
            if pending.get("state") == "AWAITING_APPROVAL":
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("submitted task did not become AWAITING_APPROVAL")
        oa_action(bob, oa_url, draft_id, "decision", "approved")
        for _ in range(40):
            status, _ = mcp.call("get_task_status", {"task_id": created["task_id"]})
            if status.get("state") == "ACTIVE":
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("approved task did not become ACTIVE")
        answer, narrative, queries, admitted, usage, gateway_ms, accounting_ms = run_agent(
            invocation, mcp, tasks[invocation["task_id"]], lock, api_key,
        )
        facts = audit_facts(project, admitted, env)
        risk = risk_metrics(facts, tasks[invocation["task_id"]])
        record = {
            "schema_version": 2, "study_id": invocation["study_id"], "run_id": invocation["run_id"],
            "task_id": invocation["task_id"], "arm": invocation["arm"], "seed": invocation["seed"],
            "phase": invocation["phase"], "budget_multiplier": invocation["budget_multiplier"],
            "model": {"provider": lock["provider"], "model": lock["model"], "version": lock["model_version"], "temperature": lock["temperature"]},
            "database_snapshot": "workflow-study-2026-v1", "database_instance_id": project,
            "root_task_id": created["task_id"], "cache_namespace": invocation["isolation_namespace"],
            "budget_freeze_sha256": invocation["budget_freeze_sha256"],
            "execution_lock_sha256": invocation["execution_lock_sha256"],
            "budget_rejection_envelope": invocation["budget_rejection_envelope"],
            "started_at": started_at, "finished_at": now(), "status": "completed", "budget": invocation["budget"],
            "queries": queries, "final_answer": answer, "final_answer_text": narrative,
            "common_v3_risk": risk,
            "native_usage": {key: usage[key] for key in ("successful_queries", "returned_rows", "serialized_bytes")},
            "runtime_budget_rejections": usage["runtime_budget_rejections"],
            "performance": {
                "wall_clock_ms": round((time.monotonic() - wall_started) * 1000), "gateway_latency_ms": gateway_ms,
                "accounting_latency_ms": accounting_ms, "exposure_storage_bytes": exposure_storage_bytes(project, env),
            },
        }
        json.dump(record, sys.stdout, ensure_ascii=False, separators=(",", ":"))
        sys.stdout.write("\n")
    finally:
        if os.getenv("WORKFLOW_STUDY_KEEP_STACK") != "1":
            subprocess.run(compose_command(project, "down", "-v", "--remove-orphans"), cwd=ROOT, env=env, capture_output=True, check=False)


if __name__ == "__main__":
    main()
