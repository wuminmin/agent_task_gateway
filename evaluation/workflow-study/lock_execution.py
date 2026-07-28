#!/usr/bin/env python3
"""Create the immutable DeepSeek execution lock before budget collection."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

import deepseek_agent_adapter as adapter
import validate


HERE = Path(__file__).resolve().parent


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True, help="DeepSeek API model identifier, e.g. deepseek-chat")
    parser.add_argument("--model-version", required=True, help="Provider release/version recorded on the collection date")
    parser.add_argument("--temperature", type=float, default=0)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    tasks_doc, protocol = validate.validate_design()
    payload = {
        "schema_version": 1,
        "study_id": protocol["study_id"],
        "provider": "deepseek",
        "model": args.model,
        "model_version": args.model_version,
        "temperature": args.temperature,
        "system_prompt_sha256": hashlib.sha256((HERE / "system-prompt.txt").read_bytes()).hexdigest(),
        "tool_surface_sha256": hashlib.sha256((HERE / "agent-tool-surface.json").read_bytes()).hexdigest(),
        "agent_adapter_sha256": hashlib.sha256((HERE / "deepseek_agent_adapter.py").read_bytes()).hexdigest(),
        "answer_schema_sha256": validate.canonical_sha256(adapter.answer_contract(tasks_doc)),
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"wrote execution lock: {args.output}")


if __name__ == "__main__":
    main()
