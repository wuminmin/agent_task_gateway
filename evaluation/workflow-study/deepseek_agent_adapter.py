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
from pathlib import Path

import controller
import study_risk
import validate


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
COMPOSE_FILES = (ROOT / "compose.yaml", HERE / "compose.yaml")
COMMON_ERROR = {"error": {"code": "STUDY_BUDGET_EXHAUSTED", "envelope": "taskgate-study-budget-rejection-v1", "retryable": False}}
CSRF = re.compile(rb'name="csrf" value="([^"]+)"')
AUDIT_BUDGET = {
    "max_queries": 100, "max_rows": 100000, "max_release_facts": 1000000,
    "max_influence_facts": 10000000, "max_outcome_facts": 1000,
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


def answer_contract(tasks_doc: dict, calibration_doc: dict | None = None) -> dict:
    """Return the frozen, task-specific top-level answer fields.

    Evaluation fields are induced by the deterministic rubric.  Held-out
    calibration tasks are not scored, but still declare a strict answer shape
    so that their unlimited traces exercise the same Agent protocol.
    """

    contract = {
        task["id"]: sorted({item["answer_path"].split(".")[0] for item in task["rubric"] if item.get("answer_path")})
        for task in tasks_doc["tasks"]
    }
    if calibration_doc is not None:
        for task in calibration_doc["tasks"]:
            contract[task["id"]] = sorted(task["required_answer_fields"])
    return contract


def validate_lock(path: Path, invocation: dict, tasks_doc: dict, calibration_doc: dict) -> dict:
    lock = validate.validate_execution_lock(path, invocation["study_id"])
    if hashlib.sha256(path.read_bytes()).hexdigest() != invocation["execution_lock_sha256"]:
        raise ValueError("execution-lock file does not match the registered invocation")
    if lock["provider"] != "deepseek":
        raise ValueError("execution lock provider must be deepseek")
    expected = {
        "system_prompt_sha256": hashlib.sha256((HERE / "system-prompt.txt").read_bytes()).hexdigest(),
        "tool_surface_sha256": hashlib.sha256((HERE / "agent-tool-surface.json").read_bytes()).hexdigest(),
        "agent_adapter_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "answer_schema_sha256": validate.canonical_sha256(answer_contract(tasks_doc, calibration_doc)),
    }
    for field, digest in expected.items():
        if lock[field] != digest:
            raise ValueError(f"execution lock {field} does not match source")
    return lock


def requested_budget(invocation: dict) -> dict:
    if invocation["arm"] == "taskgate_v3":
        budget = invocation["budget"]
        return {
            "max_queries": AUDIT_BUDGET["max_queries"], "max_rows": AUDIT_BUDGET["max_rows"],
            "max_release_facts": budget["release_facts"],
            "max_influence_facts": budget["influence_facts"],
            "max_outcome_facts": budget["outcome_facts"],
        }
    return dict(AUDIT_BUDGET)


def visible_query_result(result: dict) -> dict:
    return {key: result.get(key) for key in ("columns", "rows", "row_count", "limited")}


def parse_final(content: str, required_fields: list[str] | None = None) -> tuple[dict, str]:
    stripped = content.strip()
    if stripped.startswith("```"):
        stripped = re.sub(r"^```(?:json)?\s*|\s*```$", "", stripped, flags=re.IGNORECASE)
    value = json.loads(stripped)
    if set(value) != {"answer", "narrative"} or not isinstance(value["answer"], dict) or not isinstance(value["narrative"], str):
        raise ValueError("final model output violates the answer envelope")
    if required_fields is not None and set(value["answer"]) != set(required_fields):
        raise ValueError("final answer fields differ from the frozen task contract")
    return value["answer"], value["narrative"]


def call_deepseek(messages: list[dict], tools: list[dict], lock: dict, api_key: str) -> tuple[dict, str]:
    base = lock["api_base_url"].rstrip("/")
    response = http_json(
        base + "/chat/completions",
        {
            "model": lock["model"], "messages": messages, "tools": tools,
            "tool_choice": "auto", "temperature": lock["temperature"],
            "top_p": lock["top_p"], "max_tokens": lock["max_tokens"],
        },
        {"Authorization": "Bearer " + api_key},
        timeout=lock["request_timeout_seconds"],
    )
    choices = response.get("choices", [])
    if not choices or not isinstance(choices[0].get("message"), dict):
        raise RuntimeError("DeepSeek response omitted an assistant message")
    response_model = response.get("model")
    if response_model != lock["model"]:
        raise RuntimeError("DeepSeek response model differs from the execution lock")
    return choices[0]["message"], response_model


def sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def audit_facts(project: str, admitted_query_ids: list[str], env: dict[str, str]) -> list[dict]:
    if not admitted_query_ids:
        return []
    ids = ",".join(sql_literal(value) for value in admitted_query_ids)
    sql = f"""
WITH observed AS (
  SELECT DISTINCT root_task_id, query_id, ledger_kind, fact_sha256
  FROM query_exposure_facts WHERE query_id IN ({ids})
), linked AS (
  SELECT root_task_id, ledger_kind, fact_sha256,
         json_agg(query_id ORDER BY query_id) AS query_ids
  FROM observed GROUP BY root_task_id, ledger_kind, fact_sha256
)
SELECT json_build_object('ledger_kind', linked.ledger_kind, 'fact_sha256', linked.fact_sha256,
                         'identity', facts.identity_json, 'query_ids', linked.query_ids)::text
FROM linked
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
    return study_risk.measure(facts, task)


def empty_run_state() -> dict:
    return {
        "queries": [],
        "admitted_query_ids": [],
        "native_usage": {"successful_queries": 0, "returned_rows": 0, "serialized_bytes": 0},
        "runtime_budget_rejections": 0,
        "gateway_latency_ms": 0,
        "accounting_latency_ms": 0,
        "provider_response_models": [],
    }


def run_agent(
    invocation: dict,
    mcp: MCP,
    task: dict,
    lock: dict,
    api_key: str,
    state: dict,
) -> tuple[dict, str]:
    tools = validate.load(HERE / "agent-tool-surface.json")
    if "rubric" in task:
        required_fields = answer_contract({"tasks": [task]})[task["id"]]
    else:
        required_fields = sorted(task["required_answer_fields"])
    system = (HERE / "system-prompt.txt").read_text(encoding="utf-8")
    user = {
        "evaluation_replicate": invocation["replicate"], "task_id": task["id"], "business_request": task["prompt"],
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
    queries = state["queries"]
    admitted_query_ids = state["admitted_query_ids"]
    usage = state["native_usage"]
    task_id = invocation["root_task_id"]
    for turn in range(lock["max_tool_turns"]):
        message, response_model = call_deepseek(messages, tools, lock, api_key)
        if response_model not in state["provider_response_models"]:
            state["provider_response_models"].append(response_model)
        messages.append(message)
        calls = message.get("tool_calls") or []
        if not calls:
            answer, narrative = parse_final(message.get("content") or "", required_fields)
            return answer, narrative
        for call in calls:
            name = call.get("function", {}).get("name")
            tool_content: str | None = None
            try:
                arguments = json.loads(call.get("function", {}).get("arguments") or "{}")
            except json.JSONDecodeError:
                result_for_model = {"error": {"code": "INVALID_TOOL_ARGUMENTS", "retryable": True}}
            else:
                if name == "get_budget":
                    if baseline is not None:
                        unit = next(iter(invocation["budget"]))
                        result_for_model = {
                            "policy": invocation["arm"],
                            "units": [unit],
                            "limits": {unit: baseline.ceiling},
                            "used": {unit: baseline.used},
                            "remaining": {unit: baseline.ceiling - baseline.used},
                        }
                    elif invocation["arm"] == "unlimited":
                        result_for_model = {
                            "policy": "unlimited", "units": [],
                            "limits": {}, "used": {}, "remaining": {},
                        }
                    else:
                        try:
                            raw, elapsed = mcp.call("get_budget", {"task_id": task_id})
                            state["gateway_latency_ms"] += elapsed
                            exposure = raw["exposure_budget"]
                            limits = exposure["limits"]
                            used = exposure["used"]
                            result_for_model = {
                                "policy": "taskgate_v3",
                                "units": ["release_facts", "influence_facts", "outcome_facts"],
                                "limits": limits,
                                "used": used,
                                "remaining": {
                                    unit: max(0, int(limits[unit]) - int(used[unit]))
                                    for unit in ("release_facts", "influence_facts", "outcome_facts")
                                },
                            }
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
                        state["gateway_latency_ms"] += elapsed
                        visible = visible_query_result(raw)
                        payload = controller.canonical_response_bytes(visible)
                        rows = int(raw.get("row_count", 0))
                        component = raw.get("component_ms", {})
                        state["accounting_latency_ms"] += sum(int(component.get(key, 0)) for key in (
                            "exposure_derivation", "exposure_reservation_lock", "exposure_ledger_lock", "exposure_fact_store",
                        ))
                        admitted = True
                        if baseline is not None:
                            admitted = baseline.admit(request_id, rows, payload).released
                        if admitted:
                            usage["successful_queries"] += 1
                            usage["returned_rows"] += rows
                            usage["serialized_bytes"] += len(payload)
                            entry["admitted"] = True
                            entry["query_id"] = raw.get("query_id")
                            entry["admitted_response_canonical"] = payload.decode("utf-8")
                            entry["admitted_response_sha256"] = hashlib.sha256(payload).hexdigest()
                            admitted_query_ids.append(raw["query_id"])
                            result_for_model = visible
                            tool_content = payload.decode("utf-8")
                        else:
                            state["runtime_budget_rejections"] += 1
                            result_for_model = COMMON_ERROR
                            entry["budget_rejected"] = True
                        entry.update({"row_count": rows, "serialized_bytes": len(payload)})
                    except ToolFailure as error:
                        message_text = str(error)
                        if "EXPOSURE_BUDGET_EXHAUSTED" in message_text or "暴露预算" in message_text or "预算" in message_text:
                            state["runtime_budget_rejections"] += 1
                            result_for_model = COMMON_ERROR
                            entry["budget_rejected"] = True
                        else:
                            result_for_model = {"error": {"code": "TOOL_ERROR", "message": message_text}}
                            entry["tool_error"] = True
                    queries.append(entry)
                else:
                    result_for_model = {"error": {"code": "UNKNOWN_TOOL", "retryable": False}}
            messages.append({
                "role": "tool",
                "tool_call_id": call.get("id", ""),
                "content": tool_content if tool_content is not None else json.dumps(result_for_model, ensure_ascii=False),
            })
    raise RuntimeError(f"DeepSeek exceeded the {lock['max_tool_turns']}-turn tool limit")


def metric_sections(facts: list[dict], task: dict) -> tuple[dict, dict]:
    measured = risk_metrics(facts, task)
    neutral_names = set(validate.NEUTRAL_DISCLOSURE_FIELDS)
    neutral = {name: measured.pop(name) for name in sorted(neutral_names)}
    return measured, neutral


def main() -> None:
    invocation = json.load(sys.stdin)
    tasks_doc, calibration_doc, protocol = validate.validate_design()
    tasks = {task["id"]: task for doc in (tasks_doc, calibration_doc) for task in doc["tasks"]}
    if invocation.get("study_id") != protocol["study_id"] or invocation.get("task_id") not in tasks:
        raise SystemExit("invalid workflow-study invocation")
    dotenv = read_dotenv(ROOT / ".env")
    api_key = secret("DEEPSEEK_API_KEY", dotenv)
    lock_path = Path(os.getenv("WORKFLOW_EXECUTION_LOCK", ""))
    if not lock_path.is_file():
        raise SystemExit("WORKFLOW_EXECUTION_LOCK must name the frozen execution-lock JSON")
    lock = validate_lock(lock_path, invocation, tasks_doc, calibration_doc)
    project = re.sub(r"[^a-z0-9-]", "-", invocation["run_id"].lower())[:55]
    ports = {name: free_port() for name in ("BUSINESS", "CONTROL", "GATEWAY", "OA")}
    # Match secret(): explicit process environment wins over the ignored .env.
    env = {**dotenv, **os.environ}
    env.update({f"WORKFLOW_STUDY_{name}_PORT": str(port) for name, port in ports.items()})
    started_at = now()
    wall_started = time.monotonic()
    state = empty_run_state()
    task = tasks[invocation["task_id"]]
    mcp: MCP | None = None
    created: dict | None = None
    stack_started = False
    agent_started = False
    answer: dict = {}
    narrative = ""
    failure: dict | None = None
    status = "completed"
    try:
        try:
            run_command(compose_command(project, "up", "-d", "--wait"), env)
            stack_started = True
            mcp = MCP(f"http://127.0.0.1:{ports['GATEWAY']}/mcp", secret("TASKBOUND_ALICE_TOKEN", dotenv))
            created, _ = mcp.call("request_data_task", {
                "objective": task["prompt"],
                "data_products": task["products"],
                "columns": task["approved_columns"],
                "scopes": task["scope"],
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
                task_status, _ = mcp.call("get_task_status", {"task_id": created["task_id"]})
                if task_status.get("state") == "ACTIVE":
                    break
                time.sleep(0.25)
            else:
                raise RuntimeError("approved task did not become ACTIVE")
            agent_started = True
            answer, narrative = run_agent(invocation, mcp, task, lock, api_key, state)
        except Exception as error:  # Preserve completed query evidence before teardown.
            status = "agent_error" if agent_started else "tool_error"
            failure = {
                "category": type(error).__name__,
                "stage": "agent" if agent_started else "setup",
                "message_sha256": hashlib.sha256(str(error).encode("utf-8", "replace")).hexdigest(),
            }

        admitted = state["admitted_query_ids"]
        facts = audit_facts(project, admitted, env) if admitted else []
        if created is None:
            gateway_audit = {"available": False, "reason": "task_not_created"}
        else:
            if mcp is None:
                raise RuntimeError("created task has no MCP client for the post-run budget audit")
            budget_snapshot, _ = mcp.call("get_budget", {"task_id": created["task_id"]})
            gateway_audit = {"available": True, "snapshot": budget_snapshot}
        storage_bytes = exposure_storage_bytes(project, env) if stack_started else 0
        common_risk, neutral = metric_sections(facts, task)
        record = {
            "schema_version": 3, "study_id": invocation["study_id"], "run_id": invocation["run_id"],
            "task_id": invocation["task_id"], "domain": task["domain"],
            "arm": invocation["arm"], "replicate": invocation["replicate"],
            "phase": invocation["phase"], "budget_level": invocation["budget_level"],
            "model": {
                "provider": lock["provider"], "model": lock["model"],
                "version": lock["model_version"], "temperature": lock["temperature"],
                "top_p": lock["top_p"], "max_tokens": lock["max_tokens"],
                "api_base_url": lock["api_base_url"],
            },
            "provider_response_models": sorted(state["provider_response_models"]),
            "database_snapshot": "workflow-study-2026-v1",
            "database_instance_id": project if stack_started else f"not-created-{invocation['run_id']}",
            "root_task_id": created["task_id"] if created is not None else f"not-created-{invocation['run_id']}",
            "cache_namespace": invocation["isolation_namespace"],
            "algorithmic_budget_freeze_sha256": invocation["algorithmic_budget_freeze_sha256"],
            "execution_lock_sha256": invocation["execution_lock_sha256"],
            "budget_rejection_envelope": invocation["budget_rejection_envelope"],
            "started_at": started_at, "finished_at": now(), "status": status, "budget": invocation["budget"],
            "queries": state["queries"], "final_answer": answer if status == "completed" else {},
            "final_answer_text": narrative if status == "completed" else "",
            "fact_evidence": facts,
            "fact_evidence_sha256": validate.canonical_sha256(facts),
            "gateway_budget_audit": gateway_audit,
            "gateway_budget_audit_sha256": validate.canonical_sha256(gateway_audit),
            "common_v3_risk": common_risk,
            "neutral_disclosure": neutral,
            "native_usage": state["native_usage"],
            "runtime_budget_rejections": state["runtime_budget_rejections"],
            "performance": {
                "wall_clock_ms": round((time.monotonic() - wall_started) * 1000),
                "gateway_latency_ms": state["gateway_latency_ms"],
                "accounting_latency_ms": state["accounting_latency_ms"],
                "exposure_storage_bytes": storage_bytes,
            },
        }
        if failure is not None:
            record["failure"] = failure
        json.dump(record, sys.stdout, ensure_ascii=False, separators=(",", ":"))
        sys.stdout.write("\n")
    finally:
        if os.getenv("WORKFLOW_STUDY_KEEP_STACK") != "1":
            subprocess.run(
                compose_command(project, "down", "-v", "--remove-orphans", "--rmi", "local"),
                cwd=ROOT, env=env, capture_output=True, check=False,
            )


if __name__ == "__main__":
    main()
