#!/usr/bin/env python3
"""Create the immutable DeepSeek execution lock before calibration runs."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import re
from pathlib import Path

import deepseek_agent_adapter as adapter
import validate


HERE = Path(__file__).resolve().parent


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True, help="DeepSeek API model identifier, e.g. deepseek-chat")
    parser.add_argument("--model-version", required=True, help="Provider release/version recorded on the collection date")
    parser.add_argument(
        "--campaign-id",
        required=True,
        help="Unique immutable collection identifier (letters, digits, dot, underscore, or hyphen)",
    )
    parser.add_argument("--temperature", type=float, default=0)
    parser.add_argument("--top-p", type=float, default=1.0)
    parser.add_argument("--max-tokens", type=int, default=4096)
    parser.add_argument("--request-timeout-seconds", type=int, default=300)
    parser.add_argument("--max-tool-turns", type=int, default=16)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", args.campaign_id) is None:
        raise SystemExit("--campaign-id must be 1-64 safe identifier characters")
    if not 1 <= args.request_timeout_seconds <= 1800:
        raise SystemExit("--request-timeout-seconds must be between 1 and 1800")
    if not 1 <= args.max_tool_turns <= 64:
        raise SystemExit("--max-tool-turns must be between 1 and 64")
    tasks_doc, calibration_doc, protocol = validate.validate_design()
    payload = {
        "schema_version": 2,
        "study_id": protocol["study_id"],
        "campaign_id": args.campaign_id,
        "locked_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z"),
        "provider": "deepseek",
        "model": args.model,
        "model_version": args.model_version,
        "temperature": args.temperature,
        "top_p": args.top_p,
        "max_tokens": args.max_tokens,
        "request_timeout_seconds": args.request_timeout_seconds,
        "max_tool_turns": args.max_tool_turns,
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
