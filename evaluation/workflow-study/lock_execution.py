#!/usr/bin/env python3
"""Create the immutable DeepSeek execution lock before calibration runs."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import re
import subprocess
from pathlib import Path

import deepseek_agent_adapter as adapter
import validate


HERE = Path(__file__).resolve().parent
PRICING_USD_PER_MILLION = {
    "deepseek-v4-flash": {"prompt_cache_hit": 0.0028, "prompt_cache_miss": 0.14, "completion": 0.28},
    "deepseek-v4-pro": {"prompt_cache_hit": 0.003625, "prompt_cache_miss": 0.435, "completion": 0.87},
}


def command_output(arguments: list[str], label: str) -> str:
    try:
        completed = subprocess.run(
            arguments, text=True, capture_output=True, timeout=60, check=False,
        )
    except subprocess.TimeoutExpired as error:
        raise SystemExit(f"cannot inspect {label}; command timed out") from error
    if completed.returncode != 0:
        raise SystemExit(f"cannot inspect {label}; prepare the locked container environment first")
    value = completed.stdout.strip()
    if not value:
        raise SystemExit(f"cannot inspect {label}; command returned no value")
    return value


def inspect_image(reference: str, label: str) -> dict:
    try:
        documents = json.loads(command_output(["docker", "image", "inspect", reference], label))
    except json.JSONDecodeError as error:
        raise SystemExit(f"cannot parse Docker metadata for {label}") from error
    if not isinstance(documents, list) or len(documents) != 1 or not isinstance(documents[0], dict):
        raise SystemExit(f"Docker returned ambiguous metadata for {label}")
    document = documents[0]
    image_id = document.get("Id")
    if not isinstance(image_id, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None:
        raise SystemExit(f"Docker returned an invalid immutable image ID for {label}")
    repo_digests = document.get("RepoDigests") or []
    if not isinstance(repo_digests, list) or not all(isinstance(value, str) for value in repo_digests):
        raise SystemExit(f"Docker returned invalid repository digests for {label}")
    return {
        "requested_reference": reference,
        "image_id": image_id,
        "repo_digests": sorted(set(repo_digests)),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True, help="DeepSeek API model identifier, e.g. deepseek-v4-flash")
    parser.add_argument("--model-version", required=True, help="Provider release/version recorded on the collection date")
    parser.add_argument(
        "--campaign-id",
        required=True,
        help="Unique immutable collection identifier (letters, digits, dot, underscore, or hyphen)",
    )
    parser.add_argument("--temperature", type=float, default=0)
    parser.add_argument("--top-p", type=float, default=1.0)
    parser.add_argument(
        "--thinking-mode", choices=("disabled",), default="disabled",
        help="Explicit DeepSeek thinking toggle; this benchmark freezes non-thinking mode",
    )
    parser.add_argument("--max-tokens", type=int, default=4096)
    parser.add_argument("--request-timeout-seconds", type=int, default=300)
    parser.add_argument("--max-tool-turns", type=int, default=16)
    parser.add_argument("--api-max-attempts", type=int, default=5)
    parser.add_argument("--api-backoff-initial-seconds", type=float, default=2.0)
    parser.add_argument("--api-backoff-max-seconds", type=float, default=30.0)
    parser.add_argument("--compose-start-max-attempts", type=int, default=3)
    parser.add_argument("--compose-start-backoff-seconds", type=float, default=2.0)
    parser.add_argument("--calibration-cost-limit-usd", type=float, default=2.0)
    parser.add_argument("--evaluation-cost-limit-usd", type=float, default=18.0)
    parser.add_argument("--adapter-timeout-seconds", type=int, default=1800)
    parser.add_argument(
        "--gateway-image", default="taskgate-workflow-gateway:workflow-study-v1",
        help="prebuilt gateway image reference to resolve to an immutable local image ID",
    )
    parser.add_argument(
        "--oa-image", default="taskgate-workflow-oa-demo:workflow-study-v1",
        help="prebuilt OA image reference to resolve to an immutable local image ID",
    )
    parser.add_argument(
        "--postgres-image", default="postgres:16-bookworm",
        help="PostgreSQL image reference to resolve to an immutable local image ID",
    )
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", args.campaign_id) is None:
        raise SystemExit("--campaign-id must be 1-64 safe identifier characters")
    if not 1 <= args.request_timeout_seconds <= 1800:
        raise SystemExit("--request-timeout-seconds must be between 1 and 1800")
    if not 1 <= args.max_tool_turns <= 64:
        raise SystemExit("--max-tool-turns must be between 1 and 64")
    if args.model not in PRICING_USD_PER_MILLION:
        raise SystemExit(f"--model must be one of {sorted(PRICING_USD_PER_MILLION)}")
    if not 1 <= args.api_max_attempts <= 10:
        raise SystemExit("--api-max-attempts must be between 1 and 10")
    if not 0 < args.api_backoff_initial_seconds <= args.api_backoff_max_seconds <= 300:
        raise SystemExit("API backoff must satisfy 0 < initial <= maximum <= 300 seconds")
    if not 1 <= args.compose_start_max_attempts <= 5:
        raise SystemExit("--compose-start-max-attempts must be between 1 and 5")
    if not 0 <= args.compose_start_backoff_seconds <= 30:
        raise SystemExit("--compose-start-backoff-seconds must be between 0 and 30")
    if not 0 < args.calibration_cost_limit_usd < args.evaluation_cost_limit_usd <= 100:
        raise SystemExit("cost limits must satisfy 0 < calibration < evaluation <= 100 USD")
    if not 1 <= args.adapter_timeout_seconds <= 86400:
        raise SystemExit("--adapter-timeout-seconds must be between 1 and 86400")
    if args.adapter_timeout_seconds < args.request_timeout_seconds:
        raise SystemExit("--adapter-timeout-seconds must not be shorter than one provider request timeout")
    tasks_doc, calibration_doc, protocol = validate.validate_design()
    container_images = {
        "gateway": inspect_image(args.gateway_image, "gateway image"),
        "oa_demo": inspect_image(args.oa_image, "OA image"),
        "postgres": inspect_image(args.postgres_image, "PostgreSQL image"),
    }
    container_runtime = {
        "docker_server_version": command_output(
            ["docker", "version", "--format", "{{.Server.Version}}"], "Docker server version",
        ),
        "docker_compose_version": command_output(
            ["docker", "compose", "version", "--short"], "Docker Compose version",
        ),
    }
    payload = {
        "schema_version": 2,
        "study_id": protocol["study_id"],
        "campaign_id": args.campaign_id,
        "locked_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z"),
        "provider": "deepseek",
        "model": args.model,
        "model_version": args.model_version,
        "thinking_mode": args.thinking_mode,
        "temperature": args.temperature,
        "top_p": args.top_p,
        "max_tokens": args.max_tokens,
        "request_timeout_seconds": args.request_timeout_seconds,
        "adapter_timeout_seconds": args.adapter_timeout_seconds,
        "max_tool_turns": args.max_tool_turns,
        "api_retry": {
            "max_attempts": args.api_max_attempts,
            "initial_backoff_seconds": args.api_backoff_initial_seconds,
            "max_backoff_seconds": args.api_backoff_max_seconds,
            "retryable_http_statuses": [429, 500, 502, 503, 504],
            "retry_insufficient_system_resource": True,
        },
        "infrastructure_retry": {
            "compose_start_max_attempts": args.compose_start_max_attempts,
            "compose_start_backoff_seconds": args.compose_start_backoff_seconds,
        },
        "pricing_usd_per_million_tokens": PRICING_USD_PER_MILLION[args.model],
        "pricing_source": "https://api-docs.deepseek.com/quick_start/pricing/",
        "phase_cost_limits_usd": {
            "calibration": args.calibration_cost_limit_usd,
            "evaluation": args.evaluation_cost_limit_usd,
        },
        "container_images": container_images,
        "container_runtime": container_runtime,
        "api_base_url": "https://api.deepseek.com",
        "system_prompt_sha256": hashlib.sha256((HERE / "system-prompt.txt").read_bytes()).hexdigest(),
        "tool_surface_sha256": hashlib.sha256((HERE / "agent-tool-surface.json").read_bytes()).hexdigest(),
        "agent_adapter_sha256": hashlib.sha256((HERE / "deepseek_agent_adapter.py").read_bytes()).hexdigest(),
        "answer_schema_sha256": validate.canonical_sha256(adapter.answer_contract(tasks_doc, calibration_doc)),
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"wrote execution lock: {args.output}")


if __name__ == "__main__":
    main()
