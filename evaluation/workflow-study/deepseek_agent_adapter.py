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


class CampaignAbort(RuntimeError):
    """Abort collection without converting provider infrastructure failure into task failure."""


class HTTPResponseError(RuntimeError):
    def __init__(self, status: int, host: str, body_sha256: str, retry_after: float | None) -> None:
        super().__init__(f"HTTP {status} from {host}; body_sha256={body_sha256}")
        self.status = status
        self.retry_after = retry_after


class HTTPTransportError(RuntimeError):
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


def free_ports(names: tuple[str, ...]) -> dict[str, int]:
    """Allocate distinct candidate host ports from simultaneously held sockets."""
    listeners: list[socket.socket] = []
    try:
        for _ in names:
            listener = socket.socket()
            listener.bind(("127.0.0.1", 0))
            listeners.append(listener)
        return {
            name: int(listener.getsockname()[1])
            for name, listener in zip(names, listeners)
        }
    finally:
        for listener in listeners:
            listener.close()


def compose_command(project: str, *arguments: str) -> list[str]:
    return ["docker", "compose", "-p", project, "-f", str(COMPOSE_FILES[0]), "-f", str(COMPOSE_FILES[1]), *arguments]


def run_command(argv: list[str], env: dict[str, str], timeout: int = 900) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(argv, cwd=ROOT, env=env, text=True, capture_output=True, timeout=timeout, check=False)
    if completed.returncode:
        message = completed.stderr.strip().splitlines()[-1:] or completed.stdout.strip().splitlines()[-1:]
        raise RuntimeError(f"command failed ({argv[0]} {argv[1]}): {' '.join(message)}")
    return completed


def command_output(argv: list[str], label: str, env: dict[str, str]) -> str:
    completed = subprocess.run(
        argv, cwd=ROOT, env=env, text=True, capture_output=True, timeout=60, check=False,
    )
    if completed.returncode != 0 or not completed.stdout.strip():
        raise CampaignAbort(f"locked container environment check failed for {label}")
    return completed.stdout.strip()


def configure_locked_container_environment(lock: dict, env: dict[str, str]) -> None:
    observed_runtime = {
        "docker_server_version": command_output(
            ["docker", "version", "--format", "{{.Server.Version}}"], "Docker server version", env,
        ),
        "docker_compose_version": command_output(
            ["docker", "compose", "version", "--short"], "Docker Compose version", env,
        ),
    }
    if observed_runtime != lock["container_runtime"]:
        raise CampaignAbort("container runtime version differs from the execution lock")
    environment_fields = {
        "gateway": "WORKFLOW_STUDY_GATEWAY_IMAGE",
        "oa_demo": "WORKFLOW_STUDY_OA_IMAGE",
        "postgres": "WORKFLOW_STUDY_POSTGRES_IMAGE",
    }
    for name, variable in environment_fields.items():
        image_id = lock["container_images"][name]["image_id"]
        observed_id = command_output(
            ["docker", "image", "inspect", "--format", "{{.Id}}", image_id],
            f"{name} image", env,
        )
        if observed_id != image_id:
            raise CampaignAbort(f"{name} image differs from the execution lock")
        env[variable] = image_id


def cleanup_compose_project(project: str, env: dict[str, str]) -> None:
    try:
        completed = subprocess.run(
            compose_command(project, "down", "-v", "--remove-orphans"),
            cwd=ROOT, env=env, text=True, capture_output=True, timeout=180, check=False,
        )
    except subprocess.TimeoutExpired as error:
        raise RuntimeError("Compose cleanup timed out after a failed start") from error
    if completed.returncode != 0:
        raise RuntimeError("Compose cleanup failed after a failed start")


def start_compose_project(project: str, env: dict[str, str], lock: dict) -> tuple[dict[str, int], int]:
    retry = lock["infrastructure_retry"]
    for attempt in range(1, retry["compose_start_max_attempts"] + 1):
        ports = free_ports(("GATEWAY", "OA"))
        env.update({f"WORKFLOW_STUDY_{name}_PORT": str(port) for name, port in ports.items()})
        try:
            run_command(compose_command(project, "up", "-d", "--wait", "--no-build"), env)
            return ports, attempt
        except (RuntimeError, subprocess.TimeoutExpired):
            cleanup_compose_project(project, env)
            if attempt == retry["compose_start_max_attempts"]:
                raise RuntimeError("Compose start exhausted its locked retry policy")
            time.sleep(retry["compose_start_backoff_seconds"] * attempt)
    raise RuntimeError("Compose start retry loop terminated unexpectedly")


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
        raw_retry_after = error.headers.get("Retry-After") if error.headers is not None else None
        try:
            retry_after = float(raw_retry_after) if raw_retry_after is not None else None
        except ValueError:
            retry_after = None
        raise HTTPResponseError(
            error.code,
            urllib.parse.urlparse(url).netloc,
            hashlib.sha256(body.encode("utf-8", "replace")).hexdigest(),
            retry_after,
        ) from error
    except (urllib.error.URLError, TimeoutError, socket.timeout) as error:
        host = urllib.parse.urlparse(url).netloc
        raise HTTPTransportError(f"transport failure from {host}: {type(error).__name__}") from error


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


def empty_provider_usage() -> dict[str, int]:
    return {
        "prompt_tokens": 0,
        "prompt_cache_hit_tokens": 0,
        "prompt_cache_miss_tokens": 0,
        "completion_tokens": 0,
        "reasoning_tokens": 0,
        "total_tokens": 0,
    }


def normalize_provider_usage(response: dict) -> dict[str, int]:
    raw = response.get("usage")
    if not isinstance(raw, dict):
        raise CampaignAbort("DeepSeek response omitted auditable token usage")
    details = raw.get("completion_tokens_details") or {}
    values = {
        "prompt_tokens": raw.get("prompt_tokens"),
        "prompt_cache_hit_tokens": raw.get("prompt_cache_hit_tokens", 0),
        "prompt_cache_miss_tokens": raw.get("prompt_cache_miss_tokens", raw.get("prompt_tokens")),
        "completion_tokens": raw.get("completion_tokens"),
        "reasoning_tokens": details.get("reasoning_tokens", 0) if isinstance(details, dict) else 0,
        "total_tokens": raw.get("total_tokens"),
    }
    if not all(isinstance(value, int) and not isinstance(value, bool) and value >= 0 for value in values.values()):
        raise CampaignAbort("DeepSeek response contained invalid token usage")
    if values["prompt_tokens"] != values["prompt_cache_hit_tokens"] + values["prompt_cache_miss_tokens"]:
        raise CampaignAbort("DeepSeek prompt token accounting is inconsistent")
    if values["total_tokens"] != values["prompt_tokens"] + values["completion_tokens"]:
        raise CampaignAbort("DeepSeek total token accounting is inconsistent")
    if values["reasoning_tokens"] > values["completion_tokens"]:
        raise CampaignAbort("DeepSeek reasoning token accounting is inconsistent")
    return values


def merge_token_usage(target: dict[str, int], source: dict[str, int]) -> None:
    for field in target:
        target[field] += source[field]


def call_deepseek(messages: list[dict], tools: list[dict], lock: dict, api_key: str) -> tuple[dict, str, dict]:
    base = lock["api_base_url"].rstrip("/")
    retry = lock["api_retry"]
    usage = empty_provider_usage()
    fingerprints: list[str] = []
    finish_reasons: list[str] = []
    for attempt in range(1, retry["max_attempts"] + 1):
        try:
            response = http_json(
                base + "/chat/completions",
                {
                    "model": lock["model"], "messages": messages, "tools": tools,
                    "tool_choice": "auto", "temperature": lock["temperature"],
                    "top_p": lock["top_p"], "max_tokens": lock["max_tokens"],
                    "thinking": {"type": lock["thinking_mode"]},
                },
                {"Authorization": "Bearer " + api_key},
                timeout=lock["request_timeout_seconds"],
            )
        except HTTPResponseError as error:
            retryable = error.status in retry["retryable_http_statuses"]
            if not retryable or attempt == retry["max_attempts"]:
                raise CampaignAbort(str(error)) from error
            delay = min(
                retry["max_backoff_seconds"],
                retry["initial_backoff_seconds"] * (2 ** (attempt - 1)),
            )
            if error.retry_after is not None:
                delay = min(retry["max_backoff_seconds"], max(delay, error.retry_after))
            time.sleep(delay)
            continue
        except HTTPTransportError as error:
            if attempt == retry["max_attempts"]:
                raise CampaignAbort(str(error)) from error
            time.sleep(min(
                retry["max_backoff_seconds"],
                retry["initial_backoff_seconds"] * (2 ** (attempt - 1)),
            ))
            continue
        choices = response.get("choices", [])
        if not choices or not isinstance(choices[0].get("message"), dict):
            raise CampaignAbort("DeepSeek response omitted an assistant message")
        response_model = response.get("model")
        if response_model != lock["model"]:
            raise CampaignAbort("DeepSeek response model differs from the execution lock")
        fingerprint = response.get("system_fingerprint")
        if not isinstance(fingerprint, str) or not fingerprint:
            raise CampaignAbort("DeepSeek response omitted system_fingerprint")
        finish_reason = choices[0].get("finish_reason")
        if not isinstance(finish_reason, str) or not finish_reason:
            raise CampaignAbort("DeepSeek response omitted finish_reason")
        merge_token_usage(usage, normalize_provider_usage(response))
        fingerprints.append(fingerprint)
        finish_reasons.append(finish_reason)
        if finish_reason == "insufficient_system_resource":
            if not retry["retry_insufficient_system_resource"] or attempt == retry["max_attempts"]:
                raise CampaignAbort("DeepSeek exhausted retries after insufficient_system_resource")
            time.sleep(min(
                retry["max_backoff_seconds"],
                retry["initial_backoff_seconds"] * (2 ** (attempt - 1)),
            ))
            continue
        return choices[0]["message"], response_model, {
            "request_attempts": attempt,
            "successful_responses": len(finish_reasons),
            "retry_attempts": attempt - 1,
            "token_usage": usage,
            "system_fingerprints": fingerprints,
            "finish_reasons": finish_reasons,
        }
    raise CampaignAbort("DeepSeek retry loop terminated without a response")


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
        "provider_api": {
            "model_turns": 0,
            "request_attempts": 0,
            "successful_responses": 0,
            "retry_attempts": 0,
            "token_usage": empty_provider_usage(),
            "system_fingerprints": [],
            "finish_reasons": [],
        },
        "final_format_repair_attempts": 0,
    }


def merge_provider_call(state: dict, observed: dict) -> None:
    target = state["provider_api"]
    target["model_turns"] += 1
    for field in ("request_attempts", "successful_responses", "retry_attempts"):
        target[field] += observed[field]
    merge_token_usage(target["token_usage"], observed["token_usage"])
    target["system_fingerprints"].extend(observed["system_fingerprints"])
    target["finish_reasons"].extend(observed["finish_reasons"])


def estimated_provider_cost_usd(provider_api: dict, lock: dict) -> float:
    usage = provider_api["token_usage"]
    prices = lock["pricing_usd_per_million_tokens"]
    cost = (
        usage["prompt_cache_hit_tokens"] * prices["prompt_cache_hit"]
        + usage["prompt_cache_miss_tokens"] * prices["prompt_cache_miss"]
        + usage["completion_tokens"] * prices["completion"]
    ) / 1_000_000
    return round(cost, 12)


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
        message, response_model, provider_call = call_deepseek(messages, tools, lock, api_key)
        merge_provider_call(state, provider_call)
        if response_model not in state["provider_response_models"]:
            state["provider_response_models"].append(response_model)
        messages.append(message)
        calls = message.get("tool_calls") or []
        if not calls:
            try:
                answer, narrative = parse_final(message.get("content") or "", required_fields)
            except ValueError:
                state["final_format_repair_attempts"] += 1
                messages.append({
                    "role": "user",
                    "content": json.dumps({
                        "error": "FINAL_RESPONSE_SCHEMA_INVALID",
                        "instruction": (
                            "Return only one valid JSON object with exactly the top-level keys "
                            "answer and narrative. answer must be an object with exactly the "
                            "required_answer_fields; narrative must be a string. Do not use a "
                            "Markdown fence and do not call another tool solely to repair formatting."
                        ),
                        "required_answer_fields": required_fields,
                    }, ensure_ascii=False),
                })
                continue
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
    # Match secret(): explicit process environment wins over the ignored .env.
    env = {**dotenv, **os.environ}
    # The provider credential is used only by this adapter process and must not
    # be inherited by Docker/Compose children that never call the provider.
    env.pop("DEEPSEEK_API_KEY", None)
    configure_locked_container_environment(lock, env)
    started_at = now()
    wall_started = time.monotonic()
    state = empty_run_state()
    task = tasks[invocation["task_id"]]
    mcp: MCP | None = None
    created: dict | None = None
    stack_started = False
    compose_start_attempts = 0
    agent_started = False
    answer: dict = {}
    narrative = ""
    failure: dict | None = None
    status = "completed"
    try:
        try:
            ports, compose_start_attempts = start_compose_project(project, env, lock)
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
        except CampaignAbort:
            raise
        except Exception as error:  # Preserve completed query evidence before teardown.
            if not agent_started:
                message_digest = hashlib.sha256(str(error).encode("utf-8", "replace")).hexdigest()
                raise CampaignAbort(
                    f"workflow setup failed ({type(error).__name__}); message_sha256={message_digest}"
                ) from error
            status = "agent_error"
            failure = {
                "category": type(error).__name__,
                "stage": "agent",
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
        provider_api = state["provider_api"]
        provider_api["system_fingerprints"] = sorted(set(provider_api["system_fingerprints"]))
        provider_api["estimated_cost_usd"] = estimated_provider_cost_usd(provider_api, lock)
        record = {
            "schema_version": 3, "study_id": invocation["study_id"], "run_id": invocation["run_id"],
            "task_id": invocation["task_id"], "domain": task["domain"],
            "arm": invocation["arm"], "replicate": invocation["replicate"],
            "phase": invocation["phase"], "budget_level": invocation["budget_level"],
            "model": {
                "provider": lock["provider"], "model": lock["model"],
                "version": lock["model_version"], "thinking_mode": lock["thinking_mode"],
                "temperature": lock["temperature"],
                "top_p": lock["top_p"], "max_tokens": lock["max_tokens"],
                "api_base_url": lock["api_base_url"],
            },
            "provider_response_models": sorted(state["provider_response_models"]),
            "provider_api": provider_api,
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
            "final_format_repair_attempts": state["final_format_repair_attempts"],
            "compose_start_attempts": compose_start_attempts,
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
                compose_command(project, "down", "-v", "--remove-orphans"),
                cwd=ROOT, env=env, capture_output=True, check=False,
            )


if __name__ == "__main__":
    main()
