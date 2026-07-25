#!/usr/bin/env python3
"""Bind RQ3 PostgreSQL tests to their raw go-test JSON evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PACKAGE = "taskbound.local/agent-data-gateway/internal/control"


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def relative(path: Path) -> str:
    return path.resolve().relative_to(ROOT).as_posix()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--artifact", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--exit-code", type=int, required=True)
    parser.add_argument("--command", required=True)
    parser.add_argument("--go-version", required=True)
    args = parser.parse_args()

    report = json.loads(args.report.read_text(encoding="utf-8"))
    rq3 = report["rq3_anti_arbitrage"]
    manifest = rq3["postgres_integration_manifest"]
    expected = {item["test"]: item["id"] for item in manifest}
    if len(expected) != len(manifest) or not expected:
        raise SystemExit("RQ3 integration manifest is empty or duplicated")

    final_events: dict[str, dict] = {}
    package_passed = False
    timestamps: list[str] = []
    for line_number, raw in enumerate(args.log.read_text(encoding="utf-8", errors="strict").splitlines(), 1):
        try:
            event = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise SystemExit(f"invalid go-test JSON at line {line_number}: {exc}") from exc
        if event.get("Package") != PACKAGE:
            continue
        if isinstance(event.get("Time"), str):
            timestamps.append(event["Time"])
        test = event.get("Test")
        action = event.get("Action")
        if test in expected and action in {"pass", "fail", "skip"}:
            final_events[test] = event
        if not test and action == "pass":
            package_passed = True

    tests = []
    for test, case_id in sorted(expected.items(), key=lambda item: item[1]):
        event = final_events.get(test, {})
        tests.append(
            {
                "id": case_id,
                "test": test,
                "status": event.get("Action", "missing"),
                "elapsed_seconds": event.get("Elapsed"),
            }
        )
    passed = sum(item["status"] == "pass" for item in tests)
    failed = len(tests) - passed
    complete = args.exit_code == 0 and package_passed and failed == 0
    artifact = {
        "schema_version": 1,
        "status": "complete" if complete else "failed",
        "command": args.command,
        "command_exit_code": args.exit_code,
        "race_enabled": "-race" in args.command.split(),
        "package": PACKAGE,
        "go_version": args.go_version.strip(),
        "postgres_version": report["rq2_rewrite_invariance"]["postgres_version"],
        "raw_log": relative(args.log),
        "raw_log_sha256": digest(args.log),
        "started_at": min(timestamps) if timestamps else None,
        "finished_at": max(timestamps) if timestamps else None,
        "executed": len(tests),
        "passed": passed,
        "failed": failed,
        "tests": tests,
    }
    args.artifact.write_text(json.dumps(artifact, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    rq3["postgres_integration"] = {
        "status": artifact["status"],
        "artifact": relative(args.artifact),
        "artifact_sha256": digest(args.artifact),
        "executed": len(tests),
        "passed": passed,
        "failed": failed,
    }
    args.output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    if not complete:
        raise SystemExit("RQ3 PostgreSQL integration evidence is incomplete; see the retained artifact and raw log")


if __name__ == "__main__":
    main()
