#!/usr/bin/env python3
"""Validate source-backed exposure evidence and emit conservative TeX macros."""

from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path


PAPER_DIR = Path(__file__).resolve().parent
ROOT = PAPER_DIR.parent.parent
RESULT = ROOT / "evaluation/exposure/results.json"
CORPUS = ROOT / "evaluation/exposure/corpus.json"
RQ1_ORACLE = ROOT / "evaluation/exposureoracle/oracle.go"
PERFORMANCE = ROOT / "evaluation/exposure-performance/results.json"
PERFORMANCE_ENVIRONMENT = ROOT / "evaluation/exposure-performance/environment.json"
AGENT_CORPUS = ROOT / "evaluation/agenttasks/corpus.json"
FORMAL = ROOT / "formal/results/exposure_ledger.json"
OUTPUT = PAPER_DIR / "generated/evidence.tex"


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_json(path: Path) -> dict:
    with path.open("r", encoding="utf-8") as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ValueError(f"{path.relative_to(ROOT)} must contain a JSON object")
    return value


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def comma(value: int) -> str:
    return f"{value:,}"


def decimal(value: float, digits: int = 1) -> str:
    return f"{value:.{digits}f}"


def string_set_sha256(domain: str, values: list[str]) -> str:
    digest = hashlib.sha256()
    digest.update(domain.encode("utf-8"))
    for value in sorted(values):
        digest.update(value.encode("utf-8"))
        digest.update(b"\x00")
    return digest.hexdigest()


def validate_exposure() -> dict:
    report = load_json(RESULT)
    require(report.get("schema_version") == 3, "unsupported exposure report schema")
    require(report.get("corpus_sha256") == sha256(CORPUS), "exposure corpus digest is stale")
    rq1 = report.get("rq1_ground_truth", {})
    rq2 = report.get("rq2_rewrite_invariance", {})
    rq3 = report.get("rq3_anti_arbitrage", {})
    rq5 = report.get("rq5_budget_aware_planning", {})
    agent = report.get("rq5_agent_tasks", {})
    corpus = load_json(CORPUS)
    ground_truth = corpus.get("ground_truth", [])
    relation_rows = sum(len(relation.get("rows", [])) for relation in corpus.get("relations", []))
    require(
        rq1.get("cases") == len(ground_truth) >= 10
        and rq1.get("passed") == rq1.get("cases")
        and rq1.get("dataset_relations") == len(corpus.get("relations", []))
        and rq1.get("dataset_rows") == relation_rows >= 16
        and rq1.get("release_fact_comparisons", 0) > 0
        and rq1.get("influence_fact_comparisons", 0) > 0
        and rq1.get("oracle") == "independent-rq1-relational-oracle-v1"
        and rq1.get("oracle_source_sha256") == sha256(RQ1_ORACLE)
        and len(rq1.get("results", [])) == len(ground_truth)
        and all(
            re.fullmatch(r"[0-9a-f]{64}", item.get("release_set_sha256", ""))
            and re.fullmatch(r"[0-9a-f]{64}", item.get("influence_set_sha256", ""))
            for item in rq1.get("results", [])
        ),
        "RQ1 independent-oracle evidence is incomplete",
    )
    pair_signatures = rq2.get("normalized_pair_signatures", [])
    require(
        rq2.get("generated_attempts") == 1024
        and rq2.get("unique_normalized_pairs") == 1024
        and rq2.get("executed_unique_pairs") == 1024
        and rq2.get("duplicate_attempts") == 0
        and rq2.get("rewrite_templates") == 8
        and rq2.get("scenarios") == 128
        and rq2.get("fixture_rows") == 12
        and rq2.get("pair_normalization")
        == "collapse-sql-whitespace+ordered-statement-framing+sha256-v1"
        and re.fullmatch(r"[0-9a-f]{64}", rq2.get("pair_set_sha256", "")) is not None
        and len(pair_signatures) == 1024
        and len(set(pair_signatures)) == 1024
        and pair_signatures == sorted(pair_signatures)
        and all(re.fullmatch(r"[0-9a-f]{64}", signature) for signature in pair_signatures)
        and rq2.get("pair_set_sha256")
        == string_set_sha256("TASKGATE-POSTGRES-REWRITE-PAIR-SET-V1\x00", pair_signatures)
        and rq2.get("differential_checks") == 1152
        and rq2.get("metamorphic_checks") == 1024
        and rq2.get("postgres_statements") == 2176
        and rq2.get("oracle") == "independent-go-fixture-oracle-v2"
        and re.fullmatch(r"[0-9a-f]{64}", rq2.get("oracle_fixture_sha256", "")) is not None
        and rq2.get("postgres_major") == 16
        and isinstance(rq2.get("postgres_version"), str)
        and rq2.get("postgres_version")
        and rq2.get("mismatches") == 0,
        "RQ2 independent PostgreSQL campaign is incomplete",
    )
    require(
        rq3.get("deterministic_cases", 0) > 0
        and rq3.get("deterministic_passed") == rq3.get("deterministic_cases"),
        "RQ3 deterministic cases are incomplete",
    )
    manifest = rq3.get("postgres_integration_manifest", [])
    expected_manifest = {
        item.get("test"): item.get("id")
        for item in corpus.get("adversarial_cases", [])
        if item.get("execution") == "postgres_integration"
    }
    require(
        {item.get("test"): item.get("id") for item in manifest} == expected_manifest,
        "RQ3 integration-case manifest is inconsistent",
    )
    integration = rq3.get("postgres_integration", {})
    artifact_relative = integration.get("artifact", "")
    artifact_path = ROOT / artifact_relative
    require(
        integration.get("status") == "complete"
        and integration.get("executed") == len(expected_manifest)
        and integration.get("passed") == len(expected_manifest)
        and integration.get("failed") == 0
        and artifact_relative
        and artifact_path.is_file()
        and integration.get("artifact_sha256") == sha256(artifact_path),
        "RQ3 PostgreSQL integration summary is incomplete",
    )
    artifact = load_json(artifact_path)
    raw_relative = artifact.get("raw_log", "")
    raw_path = ROOT / raw_relative
    require(
        artifact.get("schema_version") == 1
        and artifact.get("status") == "complete"
        and artifact.get("command_exit_code") == 0
        and artifact.get("race_enabled") is True
        and artifact.get("package") == "taskbound.local/agent-data-gateway/internal/control"
        and artifact.get("executed") == len(expected_manifest)
        and artifact.get("passed") == len(expected_manifest)
        and artifact.get("failed") == 0
        and raw_relative
        and raw_path.is_file()
        and artifact.get("raw_log_sha256") == sha256(raw_path),
        "RQ3 integration artifact or raw-log digest is invalid",
    )
    terminal: dict[str, str] = {}
    package_pass = False
    for line in raw_path.read_text(encoding="utf-8").splitlines():
        event = json.loads(line)
        if event.get("Package") != artifact["package"]:
            continue
        if event.get("Test") in expected_manifest and event.get("Action") in {"pass", "fail", "skip"}:
            terminal[event["Test"]] = event["Action"]
        if not event.get("Test") and event.get("Action") == "pass":
            package_pass = True
    artifact_tests = {item.get("test"): item.get("id") for item in artifact.get("tests", []) if item.get("status") == "pass"}
    require(
        package_pass
        and terminal == {test: "pass" for test in expected_manifest}
        and artifact_tests == expected_manifest,
        "RQ3 raw go-test events do not prove every declared test passed",
    )
    require(rq5.get("scenarios", 0) > 0 and rq5.get("passed") == rq5.get("scenarios"), "RQ5 planner oracle is incomplete")
    require(
        report.get("rq4_runtime_overhead_status")
        == "measured_controlled_local_postgresql_campaign",
        "RQ4 status is stale",
    )
    require(
        agent.get("status") == "complete"
        and agent.get("corpus_sha256") == sha256(AGENT_CORPUS)
        and agent.get("seed") == 20260725
        and agent.get("tasks") == 120
        and agent.get("objectives") == 24
        and agent.get("budget_profiles") == 5,
        "RQ5 agent-task campaign is incomplete",
    )
    policies = {item.get("policy"): item for item in agent.get("policies", [])}
    require(
        set(policies) == {"taskgate_exact", "utility_greedy", "taskgate_exact_no_history"}
        and all(item.get("tasks") == 120 and item.get("budget_violations") == 0 for item in policies.values())
        and policies["taskgate_exact"].get("task_successes", 0) > policies["utility_greedy"].get("task_successes", 0)
        and policies["taskgate_exact"].get("mean_answer_completeness", 0)
        > policies["utility_greedy"].get("mean_answer_completeness", 0),
        "RQ5 agent-task policy results are invalid",
    )
    return report


def validate_performance() -> dict:
    result = load_json(PERFORMANCE)
    require(result.get("schema_version") == 1, "unsupported RQ4 report schema")
    require(result.get("status") == "complete_controlled_local_campaign", "RQ4 campaign is incomplete")
    require(result.get("trials") == 3 and result.get("observations") == 31296, "RQ4 trial/sample count is incomplete")
    require(result.get("environment_sha256") == sha256(PERFORMANCE_ENVIRONMENT), "RQ4 environment digest is stale")
    configuration = result.get("configuration", {})
    require(
        configuration.get("concurrency") == [1, 4, 8]
        and configuration.get("runs_per_worker") == 200
        and configuration.get("ramp_runs") == 32
        and configuration.get("task_concurrency_mode") == "delegated_tasks_shared_root",
        "RQ4 configuration is unexpected",
    )
    require(len(result.get("raw_provenance", [])) == 3, "RQ4 raw provenance is incomplete")
    cells = {(item.get("phase"), item.get("concurrency")): item for item in result.get("cells", [])}
    for key in (
        ("business_sql", 1),
        ("paired_snapshot", 1),
        ("paired_plus_algebra", 1),
        ("full_history_ramp", 1),
        ("full_history_hit", 1),
        ("full_history_hit", 4),
        ("full_history_hit", 8),
    ):
        require(key in cells, f"RQ4 omits cell {key}")
    for concurrency in (1, 4, 8):
        hit = cells[("full_history_hit", concurrency)]
        require(
            hit.get("fact_history_hit_rate") == 1
            and hit.get("query_history_hit_rate") == 1
            and hit.get("ledger_growth", {}).get("fact_rows") == 0,
            f"RQ4 history-hit cell {concurrency} is inconsistent",
        )
    return result


def validate_formal() -> dict:
    result = load_json(FORMAL)
    require(result.get("schema_version") == 1 and result.get("status") == "passed", "exposure TLC did not pass")
    for field, digest_field in (
        ("model", "model_sha256"),
        ("config", "config_sha256"),
        ("raw_log", "log_sha256"),
    ):
        relative = result.get(field, "")
        path = ROOT / relative
        require(relative and path.is_file(), f"missing formal artifact {relative!r}")
        require(sha256(path) == result.get(digest_field), f"stale formal digest for {relative}")
    log = (ROOT / result["raw_log"]).read_text(encoding="utf-8", errors="replace")
    require(log.count("Model checking completed. No error has been found.") == 1, "ambiguous TLC completion marker")
    match = re.search(
        r"(?m)^(\d[\d,]*) states generated, (\d[\d,]*) distinct states found, 0 states left on queue\.$",
        log,
    )
    depth = re.search(r"(?m)^The depth of the complete state graph search is (\d[\d,]*)\.$", log)
    require(match is not None and depth is not None, "TLC statistics are missing")
    parsed = tuple(int(value.replace(",", "")) for value in (*match.groups(), depth.group(1)))
    require(parsed == (result["states_generated"], result["distinct_states"], result["search_depth"]), "TLC statistics disagree with JSON")
    return result


def main() -> None:
    report = validate_exposure()
    performance = validate_performance()
    formal = validate_formal()
    rq1 = report["rq1_ground_truth"]
    rq2 = report["rq2_rewrite_invariance"]
    rq3 = report["rq3_anti_arbitrage"]
    rq5 = report["rq5_budget_aware_planning"]
    agent = report["rq5_agent_tasks"]
    policies = {item["policy"]: item for item in agent["policies"]}
    performance_cells = {(item["phase"], item["concurrency"]): item for item in performance["cells"]}
    direct_one = performance_cells[("business_sql", 1)]
    paired_one = performance_cells[("paired_snapshot", 1)]
    algebra_one = performance_cells[("paired_plus_algebra", 1)]
    full_one = performance_cells[("full_history_hit", 1)]
    full_four = performance_cells[("full_history_hit", 4)]
    full_eight = performance_cells[("full_history_hit", 8)]
    ramp = performance_cells[("full_history_ramp", 1)]
    baseline = report["charge_baselines"]
    first = baseline["full_first"]
    replay = baseline["full_replay"]
    lines = [
        "% Generated by paper/tkde/generate_evidence.py. Do not edit.",
        rf"\newcommand{{\ExposureProfile}}{{\texttt{{{report['profile_version']}}}}}",
        rf"\newcommand{{\ExposureCorpusHash}}{{\texttt{{{report['corpus_sha256'][:12]}}}}}",
        rf"\newcommand{{\RQOneCases}}{{{rq1['cases']}}}",
        rf"\newcommand{{\RQOnePassed}}{{{rq1['passed']}}}",
        rf"\newcommand{{\RQOneRows}}{{{rq1['dataset_rows']}}}",
        rf"\newcommand{{\RQOneReleaseFacts}}{{{comma(rq1['release_fact_comparisons'])}}}",
        rf"\newcommand{{\RQOneInfluenceFacts}}{{{comma(rq1['influence_fact_comparisons'])}}}",
        rf"\newcommand{{\RQTwoPairs}}{{{comma(rq2['generated_attempts'])}}}",
        rf"\newcommand{{\RQTwoUnique}}{{{comma(rq2['unique_normalized_pairs'])}}}",
        rf"\newcommand{{\RQTwoTemplates}}{{{rq2['rewrite_templates']}}}",
        rf"\newcommand{{\RQTwoMismatches}}{{{rq2['mismatches']}}}",
        rf"\newcommand{{\RQThreeCases}}{{{rq3['deterministic_cases']}}}",
        rf"\newcommand{{\RQThreePassed}}{{{rq3['deterministic_passed']}}}",
        rf"\newcommand{{\RQThreeExecuted}}{{{rq3['postgres_integration']['executed']}}}",
        rf"\newcommand{{\RQThreeIntegration}}{{{rq3['postgres_integration']['passed']}}}",
        rf"\newcommand{{\RQFiveCases}}{{{rq5['scenarios']}}}",
        rf"\newcommand{{\RQFivePassed}}{{{rq5['passed']}}}",
        rf"\newcommand{{\RQFourTrials}}{{{performance['trials']}}}",
        rf"\newcommand{{\RQFourObservations}}{{{comma(performance['observations'])}}}",
        rf"\newcommand{{\RQFourDirectOneMedian}}{{{decimal(direct_one['latency_ms']['p50'], 2)}}}",
        rf"\newcommand{{\RQFourPairedOneMedian}}{{{decimal(paired_one['latency_ms']['p50'], 2)}}}",
        rf"\newcommand{{\RQFourAlgebraOneMedian}}{{{decimal(algebra_one['latency_ms']['p50'], 2)}}}",
        rf"\newcommand{{\RQFourFullOneMedian}}{{{decimal(full_one['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourFullOneTail}}{{{decimal(full_one['latency_ms']['p95'])}}}",
        rf"\newcommand{{\RQFourFullOneQPS}}{{{decimal(full_one['throughput_qps'])}}}",
        rf"\newcommand{{\RQFourLockOneTail}}{{{decimal(full_one['component_ms']['exposure_ledger_lock']['p95'])}}}",
        rf"\newcommand{{\RQFourFullFourMedian}}{{{decimal(full_four['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourFullFourTail}}{{{decimal(full_four['latency_ms']['p95'])}}}",
        rf"\newcommand{{\RQFourFullFourQPS}}{{{decimal(full_four['throughput_qps'])}}}",
        rf"\newcommand{{\RQFourLockFourTail}}{{{decimal(full_four['component_ms']['exposure_ledger_lock']['p95'])}}}",
        rf"\newcommand{{\RQFourFullEightMedian}}{{{decimal(full_eight['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourFullEightTail}}{{{decimal(full_eight['latency_ms']['p95'])}}}",
        rf"\newcommand{{\RQFourFullEightQPS}}{{{decimal(full_eight['throughput_qps'])}}}",
        rf"\newcommand{{\RQFourLockEightTail}}{{{decimal(full_eight['component_ms']['exposure_ledger_lock']['p95'])}}}",
        rf"\newcommand{{\RQFourRampFacts}}{{{int(ramp['ledger_growth']['fact_rows'])}}}",
        rf"\newcommand{{\RQFiveAgentTasks}}{{{agent['tasks']}}}",
        rf"\newcommand{{\RQFiveExactSuccess}}{{{policies['taskgate_exact']['task_successes']}}}",
        rf"\newcommand{{\RQFiveGreedySuccess}}{{{policies['utility_greedy']['task_successes']}}}",
        rf"\newcommand{{\RQFiveNoHistorySuccess}}{{{policies['taskgate_exact_no_history']['task_successes']}}}",
        rf"\newcommand{{\RQFiveExactCompleteness}}{{{decimal(100 * policies['taskgate_exact']['mean_answer_completeness'])}\%}}",
        rf"\newcommand{{\RQFiveGreedyCompleteness}}{{{decimal(100 * policies['utility_greedy']['mean_answer_completeness'])}\%}}",
        rf"\newcommand{{\BaseQueryCount}}{{{baseline['query_count']}}}",
        rf"\newcommand{{\BaseRows}}{{{baseline['returned_rows']}}}",
        rf"\newcommand{{\BaseBytes}}{{{baseline['serialized_bytes']}}}",
        rf"\newcommand{{\BaseCells}}{{{baseline['weighted_cells']}}}",
        rf"\newcommand{{\BaseNoHistory}}{{{baseline['provenance_no_history']}}}",
        rf"\newcommand{{\BaseFirst}}{{({first['release']},{first['influence']})}}",
        rf"\newcommand{{\BaseReplay}}{{({replay['release']},{replay['influence']})}}",
        rf"\newcommand{{\ExposureFormalStates}}{{{comma(formal['states_generated'])}}}",
        rf"\newcommand{{\ExposureFormalDistinct}}{{{comma(formal['distinct_states'])}}}",
        rf"\newcommand{{\ExposureFormalDepth}}{{{formal['search_depth']}}}",
    ]
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    OUTPUT.write_text("\n".join(lines) + "\n", encoding="ascii")
    print(f"ok - generated {OUTPUT.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
