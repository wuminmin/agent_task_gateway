#!/usr/bin/env python3
"""Build and verify security summaries only from checksummed raw evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import re
import sys
from typing import Any


EXPECTED_FUZZ_TARGETS = (
    "FuzzAuthorizeNeverPanics",
    "FuzzFormattingMetamorphic",
    "FuzzQueryPlanCompileNeverPanics",
)
FUZZ_PUBLICATION_CPU_SECONDS = 24 * 60 * 60
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
USER_TIME_RE = re.compile(r"^\s*User time \(seconds\):\s*([0-9]+(?:\.[0-9]+)?)\s*$", re.MULTILINE)
SYSTEM_TIME_RE = re.compile(r"^\s*System time \(seconds\):\s*([0-9]+(?:\.[0-9]+)?)\s*$", re.MULTILINE)
EXIT_STATUS_RE = re.compile(r"^\s*Exit status:\s*([0-9]+)\s*$", re.MULTILINE)
PACKAGE = "taskbound.local/agent-data-gateway/evaluation/security"


class VerificationError(ValueError):
    """Raised when raw evidence is missing, inconsistent, or malformed."""


def sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError as exc:
        raise VerificationError(f"cannot hash {path}: {exc}") from exc
    return digest.hexdigest()


def load_json(path: pathlib.Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise VerificationError(f"cannot read JSON {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise VerificationError(f"JSON root is not an object: {path}")
    return value


def repository_path(root: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise VerificationError(f"{label} must be a non-empty repository-relative path")
    pure = pathlib.PurePosixPath(value)
    if pure.is_absolute() or ".." in pure.parts:
        raise VerificationError(f"{label} escapes the repository: {value}")
    resolved = (root / pathlib.Path(*pure.parts)).resolve()
    try:
        resolved.relative_to(root)
    except ValueError as exc:
        raise VerificationError(f"{label} escapes the repository: {value}") from exc
    if not resolved.is_file():
        raise VerificationError(f"{label} does not exist: {value}")
    return resolved


def relative(path: pathlib.Path, root: pathlib.Path) -> str:
    return path.resolve().relative_to(root).as_posix()


def require_int(value: Any, label: str, minimum: int = 0) -> int:
    if type(value) is not int or value < minimum:
        raise VerificationError(f"{label} must be an integer >= {minimum}")
    return value


def require_number(value: Any, label: str, minimum: float = 0.0) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise VerificationError(f"{label} must be numeric")
    result = float(value)
    if not math.isfinite(result) or result < minimum:
        raise VerificationError(f"{label} must be finite and >= {minimum}")
    return result


def check_declared_sha(path: pathlib.Path, declared: Any, label: str) -> None:
    if not isinstance(declared, str) or not SHA256_RE.fullmatch(declared):
        raise VerificationError(f"{label} is not a SHA-256 digest")
    actual = sha256(path)
    if actual != declared:
        raise VerificationError(f"{label} mismatch for {path}: declared {declared}, actual {actual}")


def verify_inputs(
    root: pathlib.Path,
    declared: Any,
    expected: set[str],
    label: str,
) -> list[pathlib.Path]:
    if not isinstance(declared, list):
        raise VerificationError(f"{label} inputs must be an array")
    seen: dict[str, pathlib.Path] = {}
    for index, item in enumerate(declared):
        if not isinstance(item, dict) or set(item) != {"path", "sha256"}:
            raise VerificationError(f"{label} inputs[{index}] must contain only path and sha256")
        path = repository_path(root, item["path"], f"{label} inputs[{index}].path")
        path_key = relative(path, root)
        if path_key in seen:
            raise VerificationError(f"duplicate {label} input: {path_key}")
        check_declared_sha(path, item["sha256"], f"{label} inputs[{index}].sha256")
        seen[path_key] = path
    if set(seen) != expected:
        missing = sorted(expected - set(seen))
        extra = sorted(set(seen) - expected)
        raise VerificationError(f"{label} input set mismatch; missing={missing}, extra={extra}")
    return [seen[key] for key in sorted(seen)]


def expected_fuzz_inputs(root: pathlib.Path) -> set[str]:
    fixed = {
        "evaluation/fuzz/policy_fuzz_test.go",
        "evaluation/fuzz/campaign.sh",
        "evaluation/Dockerfile",
        "go.mod",
        "go.sum",
    }
    discovered = {
        relative(path, root)
        for directory in (root / "internal/queryplan", root / "internal/sqlpolicy")
        for path in directory.glob("*.go")
        if path.is_file()
    }
    return fixed | discovered


def expected_attack_inputs(root: pathlib.Path) -> set[str]:
    fixed = {
        "evaluation/attacks/corpus.json",
        "evaluation/attacks/prompt-injection.json",
        "evaluation/ast-gateway/tpch.json",
        "evaluation/security/security_test.go",
        "evaluation/security/run-corpus.sh",
        "evaluation/Dockerfile",
    }
    sql_inputs = {
        relative(path, root)
        for path in (root / "evaluation/attacks/sql").glob("*.sql")
        if path.is_file()
    }
    return fixed | sql_inputs


def verify_fuzz_campaign(root: pathlib.Path, manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest = load_json(manifest_path)
    if manifest.get("schema_version") != 1 or manifest.get("status") != "complete":
        raise VerificationError("fuzz campaign is not a completed schema-v1 campaign")
    if not isinstance(manifest.get("run_id"), str) or not manifest["run_id"]:
        raise VerificationError("fuzz campaign omits run_id")
    if not isinstance(manifest.get("go_version"), str) or not manifest["go_version"].startswith("go version go"):
        raise VerificationError("fuzz campaign omits a recognizable Go version")
    requested_hours = require_int(manifest.get("requested_cpu_hours"), "requested_cpu_hours", 1)
    workers = require_int(manifest.get("workers"), "workers", 1)
    wall_seconds = require_int(manifest.get("wall_seconds_per_target"), "wall_seconds_per_target", 1)
    image_id = manifest.get("fuzz_image_id")
    if not isinstance(image_id, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
        raise VerificationError("fuzz campaign omits a content-addressed image ID")

    input_paths = verify_inputs(root, manifest.get("inputs"), expected_fuzz_inputs(root), "fuzz")
    input_by_relative = {relative(path, root): path for path in input_paths}
    for path_field, sha_field, expected_path in (
        ("fuzz_source", "fuzz_source_sha256", "evaluation/fuzz/policy_fuzz_test.go"),
        ("campaign_source", "campaign_source_sha256", "evaluation/fuzz/campaign.sh"),
        ("dockerfile", "dockerfile_sha256", "evaluation/Dockerfile"),
    ):
        if manifest.get(path_field) != expected_path:
            raise VerificationError(f"fuzz campaign {path_field} must be {expected_path}")
        check_declared_sha(input_by_relative[expected_path], manifest.get(sha_field), sha_field)

    targets = manifest.get("targets")
    if not isinstance(targets, list) or len(targets) != len(EXPECTED_FUZZ_TARGETS):
        raise VerificationError("fuzz campaign must contain exactly three targets")
    names = [target.get("name") if isinstance(target, dict) else None for target in targets]
    if tuple(names) != EXPECTED_FUZZ_TARGETS:
        raise VerificationError(f"unexpected fuzz target sequence: {names}")

    corpus = manifest.get("corpus")
    if not isinstance(corpus, list):
        raise VerificationError("fuzz campaign corpus must be an array")
    declared_corpus: dict[str, pathlib.Path] = {}
    for index, item in enumerate(corpus):
        if not isinstance(item, dict) or set(item) != {"path", "sha256"}:
            raise VerificationError(f"fuzz corpus[{index}] must contain only path and sha256")
        corpus_name = item["path"]
        if not isinstance(corpus_name, str):
            raise VerificationError(f"fuzz corpus[{index}].path must be a string")
        pure = pathlib.PurePosixPath(corpus_name)
        if pure.is_absolute() or ".." in pure.parts or len(pure.parts) < 2:
            raise VerificationError(f"unsafe fuzz corpus path: {corpus_name}")
        if pure.parts[0] not in {f"cache-{name}" for name in EXPECTED_FUZZ_TARGETS}:
            raise VerificationError(f"fuzz corpus path has an unexpected cache: {corpus_name}")
        corpus_path = (manifest_path.parent / pathlib.Path(*pure.parts)).resolve()
        try:
            corpus_path.relative_to(manifest_path.parent.resolve())
        except ValueError as exc:
            raise VerificationError(f"fuzz corpus escapes campaign directory: {corpus_name}") from exc
        if not corpus_path.is_file() or corpus_name in declared_corpus:
            raise VerificationError(f"missing or duplicate fuzz corpus file: {corpus_name}")
        check_declared_sha(corpus_path, item["sha256"], f"fuzz corpus[{index}].sha256")
        declared_corpus[corpus_name] = corpus_path
    actual_corpus: dict[str, pathlib.Path] = {}
    for name in EXPECTED_FUZZ_TARGETS:
        cache_dir = manifest_path.parent / f"cache-{name}"
        if not cache_dir.is_dir():
            raise VerificationError(f"missing fuzz cache directory for {name}")
        for corpus_path in cache_dir.rglob("*"):
            if corpus_path.is_file():
                actual_corpus[corpus_path.relative_to(manifest_path.parent).as_posix()] = corpus_path
    if set(declared_corpus) != set(actual_corpus):
        missing = sorted(set(actual_corpus) - set(declared_corpus))
        extra = sorted(set(declared_corpus) - set(actual_corpus))
        raise VerificationError(f"fuzz corpus inventory mismatch; missing={missing}, extra={extra}")

    verified_targets: list[dict[str, Any]] = []
    target_logs: list[pathlib.Path] = []
    actual_total = 0.0
    for index, target in enumerate(targets):
        if not isinstance(target, dict):
            raise VerificationError(f"fuzz target {index} is not an object")
        log_name = target.get("log")
        if not isinstance(log_name, str) or pathlib.PurePosixPath(log_name).name != log_name:
            raise VerificationError(f"fuzz target {names[index]} has an unsafe log name")
        log_path = (manifest_path.parent / log_name).resolve()
        try:
            log_path.relative_to(root)
        except ValueError as exc:
            raise VerificationError(f"fuzz log escapes repository: {log_name}") from exc
        if not log_path.is_file():
            raise VerificationError(f"missing fuzz log: {log_path}")
        check_declared_sha(log_path, target.get("log_sha256"), f"{names[index]} log_sha256")
        try:
            log_text = log_path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as exc:
            raise VerificationError(f"cannot read fuzz log {log_path}: {exc}") from exc
        user_matches = USER_TIME_RE.findall(log_text)
        system_matches = SYSTEM_TIME_RE.findall(log_text)
        exit_matches = EXIT_STATUS_RE.findall(log_text)
        if len(user_matches) != 1 or len(system_matches) != 1 or exit_matches != ["0"]:
            raise VerificationError(f"fuzz log lacks one successful GNU-time record: {log_path}")
        expected_command = (
            'Command being timed: "/usr/local/bin/taskgate-fuzz.test -test.run ^$ '
            f'-test.fuzz ^{names[index]}$ -test.fuzztime {wall_seconds}s '
            f'-test.parallel {workers} -test.fuzzcachedir /results/cache-{names[index]}"'
        )
        if not re.search(r"(?m)^PASS$", log_text) or expected_command not in log_text:
            raise VerificationError(f"fuzz log lacks the successful immutable-binary command: {log_path}")
        if "without coverage guidance" in log_text or "not built with coverage instrumentation" in log_text:
            raise VerificationError(f"fuzz log shows coverage-guidance was disabled: {log_path}")
        if re.search(r"(?m)^panic:", log_text):
            raise VerificationError(f"fuzz log contains a panic: {log_path}")
        user_seconds = float(user_matches[0])
        system_seconds = float(system_matches[0])
        parsed_cpu = user_seconds + system_seconds
        declared_user = require_number(target.get("user_seconds"), f"{names[index]} user_seconds")
        declared_system = require_number(target.get("system_seconds"), f"{names[index]} system_seconds")
        declared_cpu = require_number(target.get("actual_cpu_seconds"), f"{names[index]} actual_cpu_seconds")
        if not math.isclose(user_seconds, declared_user, abs_tol=1e-6) or not math.isclose(system_seconds, declared_system, abs_tol=1e-6):
            raise VerificationError(f"fuzz target CPU fields do not match GNU time: {names[index]}")
        if not math.isclose(parsed_cpu, declared_cpu, abs_tol=1e-6):
            raise VerificationError(f"fuzz target aggregate CPU does not match GNU time: {names[index]}")
        actual_total += parsed_cpu
        target_logs.append(log_path)
        verified_targets.append(
            {
                "name": names[index],
                "actual_cpu_seconds": round(parsed_cpu, 6),
                "raw_log": relative(log_path, root),
                "raw_log_sha256": sha256(log_path),
            }
        )

    declared_total = require_number(manifest.get("actual_cpu_seconds"), "actual_cpu_seconds")
    if not math.isclose(actual_total, declared_total, abs_tol=1e-6):
        raise VerificationError("campaign actual_cpu_seconds does not match parsed GNU-time logs")
    campaign_requirement = actual_total >= requested_hours * 3600
    if manifest.get("cpu_requirement_met") is not campaign_requirement:
        raise VerificationError("campaign cpu_requirement_met does not match parsed CPU time")
    publication_requirement = requested_hours >= 24 and actual_total >= FUZZ_PUBLICATION_CPU_SECONDS
    return {
        "manifest": manifest_path,
        "inputs": input_paths,
        "logs": target_logs,
        "corpus": [declared_corpus[key] for key in sorted(declared_corpus)],
        "requested_hours": requested_hours,
        "actual_seconds": round(actual_total, 6),
        "publication_requirement_met": publication_requirement,
        "targets": verified_targets,
    }


def parse_attack_corpora(root: pathlib.Path) -> dict[str, Any]:
    corpus_path = root / "evaluation/attacks/corpus.json"
    corpus = load_json(corpus_path)
    if corpus.get("schema_version") != 1 or corpus.get("grant") != "evaluation/ast-gateway/tpch.json":
        raise VerificationError("attack corpus has unsupported schema or grant")
    cases = corpus.get("cases")
    if not isinstance(cases, list) or not cases:
        raise VerificationError("attack corpus has no cases")
    case_ids: list[str] = []
    referenced_sql: set[str] = set()
    for index, case in enumerate(cases):
        if not isinstance(case, dict):
            raise VerificationError(f"attack case {index} is not an object")
        case_id, filename, expected = case.get("id"), case.get("file"), case.get("expected")
        if not all(isinstance(value, str) and value for value in (case_id, filename, expected)):
            raise VerificationError(f"attack case {index} has incomplete fields")
        if case_id in case_ids:
            raise VerificationError(f"duplicate attack case ID: {case_id}")
        sql_path = pathlib.PurePosixPath(filename)
        if sql_path.is_absolute() or ".." in sql_path.parts or sql_path.parts[:1] != ("sql",):
            raise VerificationError(f"attack case has unsafe SQL path: {filename}")
        referenced_sql.add(f"evaluation/attacks/{filename}")
        case_ids.append(case_id)
    available_sql = {
        relative(path, root)
        for path in (root / "evaluation/attacks/sql").glob("*.sql")
        if path.is_file()
    }
    if referenced_sql != available_sql:
        raise VerificationError("attack corpus must reference every and only versioned SQL input")

    prompt_path = root / "evaluation/attacks/prompt-injection.json"
    prompts = load_json(prompt_path)
    prompt_cases = prompts.get("cases")
    if prompts.get("schema_version") != 1 or not isinstance(prompt_cases, list) or not prompt_cases:
        raise VerificationError("prompt-injection corpus is invalid or empty")
    prompt_ids: list[str] = []
    prompt_attempts: dict[str, list[str]] = {}
    for index, prompt in enumerate(prompt_cases):
        if not isinstance(prompt, dict):
            raise VerificationError(f"prompt case {index} is not an object")
        prompt_id = prompt.get("id")
        if not all(isinstance(prompt.get(field), str) and prompt[field] for field in ("id", "untrusted_text", "expected")):
            raise VerificationError(f"prompt case {index} has incomplete boundary metadata")
        if prompt_id in prompt_ids:
            raise VerificationError(f"duplicate prompt case ID: {prompt_id}")
        attempts = prompt.get("representative_attempts")
        if not isinstance(attempts, list) or not attempts:
            raise VerificationError(f"prompt case has no representative attempts: {prompt_id}")
        attempt_ids: list[str] = []
        for attempt in attempts:
            if not isinstance(attempt, dict) or not all(
                isinstance(attempt.get(field), str) and attempt[field]
                for field in ("id", "sql", "expected_code")
            ):
                raise VerificationError(f"prompt case has incomplete representative attempt: {prompt_id}")
            if attempt["id"] in attempt_ids:
                raise VerificationError(f"duplicate prompt attempt ID: {prompt_id}/{attempt['id']}")
            attempt_ids.append(attempt["id"])
        prompt_ids.append(prompt_id)
        prompt_attempts[prompt_id] = attempt_ids
    return {
        "case_ids": case_ids,
        "prompt_ids": prompt_ids,
        "prompt_attempts": prompt_attempts,
    }


def verify_attack_run(root: pathlib.Path, manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest = load_json(manifest_path)
    if manifest.get("schema_version") != 1 or manifest.get("status") != "passed" or manifest.get("exit_code") != 0:
        raise VerificationError("attack run is not a successful schema-v1 run")
    if not isinstance(manifest.get("go_version"), str) or not manifest["go_version"].startswith("go version go"):
        raise VerificationError("attack run omits a recognizable Go version")
    input_paths = verify_inputs(root, manifest.get("inputs"), expected_attack_inputs(root), "attack")
    input_by_relative = {relative(path, root): path for path in input_paths}
    for path_field, sha_field, expected_path in (
        ("corpus", "corpus_sha256", "evaluation/attacks/corpus.json"),
        ("test_source", "test_source_sha256", "evaluation/security/security_test.go"),
    ):
        if manifest.get(path_field) != expected_path:
            raise VerificationError(f"attack run {path_field} must be {expected_path}")
        check_declared_sha(input_by_relative[expected_path], manifest.get(sha_field), sha_field)
    log_path = repository_path(root, manifest.get("raw_log"), "attack raw_log")
    check_declared_sha(log_path, manifest.get("raw_log_sha256"), "attack raw_log_sha256")

    corpora = parse_attack_corpora(root)
    expected_tests = {"TestAttackCorpus", "TestPromptInjectionBoundaryCases"}
    expected_tests.update(f"TestAttackCorpus/{case_id}" for case_id in corpora["case_ids"])
    for prompt_id in corpora["prompt_ids"]:
        prefix = f"TestPromptInjectionBoundaryCases/{prompt_id}"
        expected_tests.add(prefix)
        expected_tests.update(f"{prefix}/{attempt_id}" for attempt_id in corpora["prompt_attempts"][prompt_id])

    passed_tests: list[str] = []
    package_passed = False
    try:
        with log_path.open(encoding="utf-8") as source:
            for line_number, line in enumerate(source, 1):
                if not line.strip():
                    continue
                try:
                    event = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise VerificationError(f"attack raw log line {line_number} is not go test JSON") from exc
                if not isinstance(event, dict) or event.get("Package") != PACKAGE:
                    raise VerificationError(f"attack raw log line {line_number} has an unexpected package")
                action, test = event.get("Action"), event.get("Test")
                if action == "fail":
                    raise VerificationError(f"attack raw log records failure: {test or PACKAGE}")
                if action == "skip" and test in expected_tests:
                    raise VerificationError(f"attack raw log skipped required test: {test}")
                if action == "pass" and isinstance(test, str):
                    passed_tests.append(test)
                if action == "pass" and test is None:
                    package_passed = True
    except (OSError, UnicodeDecodeError) as exc:
        raise VerificationError(f"cannot read attack raw log {log_path}: {exc}") from exc
    if not package_passed:
        raise VerificationError("attack raw log has no package-level pass event")
    if len(passed_tests) != len(set(passed_tests)) or set(passed_tests) != expected_tests:
        missing = sorted(expected_tests - set(passed_tests))
        extra = sorted(set(passed_tests) - expected_tests)
        raise VerificationError(f"attack test set mismatch; missing={missing}, extra={extra}")
    return {
        "manifest": manifest_path,
        "inputs": input_paths,
        "log": log_path,
        "cases": len(corpora["case_ids"]),
        "prompt_cases": len(corpora["prompt_ids"]),
    }


def manifest_argument(root: pathlib.Path, value: pathlib.Path, filename: str) -> pathlib.Path:
    path = value if value.is_absolute() else root / value
    path = path.resolve()
    if path.is_dir():
        path /= filename
    try:
        path.relative_to(root)
    except ValueError as exc:
        raise VerificationError(f"manifest is outside repository: {path}") from exc
    if not path.is_file():
        raise VerificationError(f"manifest does not exist: {path}")
    return path


def build_results(root: pathlib.Path, campaign_path: pathlib.Path, attack_path: pathlib.Path) -> dict[str, Any]:
    root = root.resolve()
    campaign_manifest = manifest_argument(root, campaign_path, "campaign.json")
    attack_manifest = manifest_argument(root, attack_path, "run.json")
    fuzz = verify_fuzz_campaign(root, campaign_manifest)
    attack = verify_attack_run(root, attack_manifest)
    verifier_path = pathlib.Path(__file__).resolve()
    evidence_paths = {
        campaign_manifest,
        attack_manifest,
        attack["log"],
        verifier_path,
        *fuzz["inputs"],
        *fuzz["logs"],
        *fuzz["corpus"],
        *attack["inputs"],
    }
    evidence = [
        {"path": relative(path, root), "sha256": sha256(path)}
        for path in sorted(evidence_paths, key=lambda item: relative(item, root))
    ]
    fuzz_status = "passed" if fuzz["publication_requirement_met"] else "completed_below_24_cpu_hours"
    return {
        "schema_version": 1,
        "status": "partial",
        "scope": "sql_policy_attack_and_prompt_boundaries_plus_three_target_fuzz",
        "security_acceptance_met": False,
        "component_status": {"attack_corpus": "passed", "fuzz": fuzz_status},
        "cases": attack["cases"],
        "corpus_passed": attack["cases"],
        "prompt_injection_cases": attack["prompt_cases"],
        "prompt_injection_passed": attack["prompt_cases"],
        "unauthorized_crossings": None,
        "budget_violations": None,
        "panics": 0,
        "requested_fuzz_cpu_hours": fuzz["requested_hours"],
        "actual_fuzz_cpu_seconds": fuzz["actual_seconds"],
        "fuzz_cpu_hours": round(fuzz["actual_seconds"] / 3600, 6),
        "fuzz_cpu_requirement_met": fuzz["publication_requirement_met"],
        "fuzz_targets": fuzz["targets"],
        "raw_log": relative(attack["log"], root),
        "attack_manifest": relative(attack_manifest, root),
        "fuzz_campaign": relative(campaign_manifest, root),
        "evidence": evidence,
        "note": (
            "Verified SQL-policy and prompt-boundary corpus plus three fuzz targets; "
            "connector-crossing and budget-fault metrics remain unmeasured, so full security acceptance is not claimed."
        ),
    }


def verify_results_file(root: pathlib.Path, result_path: pathlib.Path) -> tuple[dict[str, Any], list[pathlib.Path]]:
    root = root.resolve()
    result_path = result_path.resolve()
    value = load_json(result_path)
    campaign = repository_path(root, value.get("fuzz_campaign"), "security fuzz_campaign")
    attack = repository_path(root, value.get("attack_manifest"), "security attack_manifest")
    rebuilt = build_results(root, campaign, attack)
    if value != rebuilt:
        raise VerificationError("security results.json is not the deterministic summary of its raw evidence")
    paths = [repository_path(root, item["path"], "security evidence path") for item in value["evidence"]]
    return value, paths


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default="/workspace", type=pathlib.Path)
    parser.add_argument("--campaign", type=pathlib.Path)
    parser.add_argument("--attack-run", type=pathlib.Path)
    parser.add_argument("--output", type=pathlib.Path)
    parser.add_argument("--verify-result", type=pathlib.Path)
    args = parser.parse_args()
    try:
        if args.verify_result is not None:
            if any(value is not None for value in (args.campaign, args.attack_run, args.output)):
                parser.error("--verify-result cannot be combined with build arguments")
            value, _ = verify_results_file(args.root, args.verify_result)
            print(f"ok - verified {args.verify_result} ({value['status']})")
            return
        if args.campaign is None or args.attack_run is None or args.output is None:
            parser.error("building requires --campaign, --attack-run, and --output")
        value = build_results(args.root, args.campaign, args.attack_run)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        print(f"ok - wrote verified security summary to {args.output} ({value['status']})")
    except VerificationError as exc:
        raise SystemExit(f"security evidence verification failed: {exc}") from exc


if __name__ == "__main__":
    main()
