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
PATH_ANALYSIS = ROOT / "evaluation/exposure-performance/path_analysis.json"
STORAGE_SCALING = ROOT / "evaluation/exposure-storage/results.json"
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


def storage_source_sha256() -> str:
    paths = [ROOT / "go.mod", ROOT / "go.sum"]
    for directory in (
        ROOT / "evaluation/cmd/exposure-storage",
        ROOT / "internal/control",
        ROOT / "internal/exposure",
    ):
        paths.extend(path for path in directory.rglob("*") if path.suffix in {".go", ".sql"})
    digest = hashlib.sha256()
    for path in sorted(paths):
        digest.update(path.relative_to(ROOT).as_posix().encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def validate_exposure() -> dict:
    report = load_json(RESULT)
    require(report.get("schema_version") == 4, "unsupported exposure report schema")
    require(report.get("corpus_sha256") == sha256(CORPUS), "exposure corpus digest is stale")
    rq1 = report.get("rq1_ground_truth", {})
    rq2 = report.get("rq2_rewrite_invariance", {})
    rq3 = report.get("rq3_anti_arbitrage", {})
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
        (item.get("package"), item.get("test")): item.get("id")
        for item in corpus.get("adversarial_cases", [])
        if item.get("execution") == "postgres_integration"
    }
    require(
        {(item.get("package"), item.get("test")): item.get("id") for item in manifest} == expected_manifest,
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
        artifact.get("schema_version") == 2
        and artifact.get("status") == "complete"
        and artifact.get("command_exit_code") == 0
        and artifact.get("race_enabled") is True
        and set(artifact.get("packages", [])) == {package for package, _ in expected_manifest}
        and artifact.get("executed") == len(expected_manifest)
        and artifact.get("passed") == len(expected_manifest)
        and artifact.get("failed") == 0
        and raw_relative
        and raw_path.is_file()
        and artifact.get("raw_log_sha256") == sha256(raw_path),
        "RQ3 integration artifact or raw-log digest is invalid",
    )
    terminal: dict[tuple[str, str], str] = {}
    package_pass: set[str] = set()
    for line in raw_path.read_text(encoding="utf-8").splitlines():
        event = json.loads(line)
        key = (event.get("Package"), event.get("Test"))
        if key in expected_manifest and event.get("Action") in {"pass", "fail", "skip"}:
            terminal[key] = event["Action"]
        if not event.get("Test") and event.get("Action") == "pass":
            package_pass.add(event.get("Package"))
    artifact_tests = {(item.get("package"), item.get("test")): item.get("id") for item in artifact.get("tests", []) if item.get("status") == "pass"}
    require(
        package_pass == {package for package, _ in expected_manifest}
        and terminal == {key: "pass" for key in expected_manifest}
        and artifact_tests == expected_manifest,
        "RQ3 raw go-test events do not prove every declared test passed",
    )
    require(
        report.get("rq4_runtime_overhead_status")
        == "measured_controlled_local_postgresql_campaign",
        "RQ4 status is stale",
    )
    exposure_invariance = report.get("rq2_exposure_invariance", {})
    require(
        exposure_invariance.get("status") == "complete"
        and exposure_invariance.get("mismatches") == 0
        and exposure_invariance.get("cases", 0) >= exposure_invariance.get("rewrites", 0)
        and exposure_invariance.get("normal_form_checks", 0) > 0
        and exposure_invariance.get("effect_checks", 0) > 0
        and re.fullmatch(r"[0-9a-f]{64}", exposure_invariance.get("pair_set_sha256", "")) is not None,
        "RQ2 exposure-invariance evidence is incomplete",
    )
    scaling = report.get("rq4_scaling", {})
    require(
        scaling.get("status") == "complete"
        and {curve.get("dimension") for curve in scaling.get("curves", [])}
        == {"observe_rows", "normalizer_depth", "novel_vs_replay"}
        and all(len(curve.get("points", [])) >= 4 for curve in scaling.get("curves", [])),
        "RQ4 scaling evidence is incomplete",
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


def validate_path_analysis(performance: dict) -> dict:
    result = load_json(PATH_ANALYSIS)
    require(
        result.get("schema_version") == 1
        and result.get("status") == "complete_posthoc_path_analysis"
        and result.get("source_campaign_sha256") == sha256(PERFORMANCE)
        and result.get("trials") == performance.get("trials"),
        "RQ4 path analysis is stale",
    )
    paths = {item.get("path"): item for item in result.get("paths", [])}
    require(
        set(paths) == {"fresh_deployment_novel", "ramp_novel", "ramp_hit"}
        and paths["fresh_deployment_novel"].get("samples") == 3
        and paths["ramp_novel"].get("samples") == 12
        and paths["ramp_hit"].get("samples") == 84,
        "RQ4 novel/hit path partition is incomplete",
    )
    raw = {item.get("run_id"): item for item in result.get("raw_provenance", [])}
    require(
        len(raw) == 3
        and all(
            raw.get(item["run_id"], {}).get("samples_sha256") == item["samples_sha256"]
            for item in performance.get("raw_provenance", [])
        ),
        "RQ4 path analysis does not bind the campaign samples",
    )
    ramp = next(
        item for item in performance["cells"]
        if item["phase"] == "full_history_ramp" and item["concurrency"] == 1
    )
    storage = result.get("ledger_storage", {})
    require(
        storage.get("fact_rows") == ramp["ledger_growth"]["fact_rows"] == 28
        and storage.get("canonical_payload_bytes") == ramp["ledger_growth"]["fact_payload_bytes"]
        and storage.get("table_bytes") == ramp["ledger_growth"]["table_bytes"]
        and storage.get("index_bytes") == ramp["ledger_growth"]["indexes_bytes"]
        and storage.get("allocated_bytes") == storage["table_bytes"] + storage["index_bytes"],
        "RQ4 ledger storage analysis is inconsistent",
    )
    return result


def validate_storage_scaling() -> dict:
    result = load_json(STORAGE_SCALING)
    require(
        result.get("schema_version") == 1
        and result.get("status") == "complete_control_postgresql_storage_scaling"
        and result.get("trials") == 3
        and result.get("facts_per_ledger_sizes") == [10, 100, 1000, 10000]
        and "PostgreSQL 16.14" in result.get("postgres_version", "")
        and result.get("source_sha256") == storage_source_sha256(),
        "RQ4 Control PostgreSQL storage-scaling campaign is stale",
    )
    raw = {(item.get("trial"), item.get("facts_per_ledger"), item.get("operation")): item
           for item in result.get("raw_points", [])}
    require(len(raw) == 24, "RQ4 storage-scaling raw points are incomplete")
    previous = 0
    for size in result["facts_per_ledger_sizes"]:
        for trial in range(1, 4):
            novel = raw.get((trial, size, "novel"), {})
            replay = raw.get((trial, size, "replay"), {})
            require(
                novel.get("actual_release_facts") == size
                and novel.get("actual_dependency_facts") == size
                and novel.get("charged_release_facts") == size - previous
                and novel.get("charged_dependency_facts") == size - previous
                and novel.get("storage", {}).get("fact_rows") == 2 * size
                and replay.get("actual_release_facts") == size
                and replay.get("actual_dependency_facts") == size
                and replay.get("charged_release_facts") == 0
                and replay.get("charged_dependency_facts") == 0
                and replay.get("storage") == novel.get("storage"),
                f"RQ4 storage/replay evidence is inconsistent at trial={trial}, size={size}",
            )
        previous = size
    aggregates = {(item.get("facts_per_ledger"), item.get("operation")): item
                  for item in result.get("aggregates", [])}
    require(
        len(aggregates) == 8
        and all(item.get("trials") == 3 for item in aggregates.values()),
        "RQ4 storage-scaling aggregates are incomplete",
    )
    require(
        len(result.get("budget_boundaries", [])) == 3
        and all(
            item.get("budget_facts_per_ledger") == 10000
            and item.get("attempted_facts_per_ledger") == 10001
            and item.get("rejected") is True
            and item.get("fact_rows_before") == item.get("fact_rows_after") == 20000
            for item in result["budget_boundaries"]
        ),
        "RQ4 storage-scaling budget boundary is incomplete",
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
    path_analysis = validate_path_analysis(performance)
    storage_scaling = validate_storage_scaling()
    formal = validate_formal()
    rq1 = report["rq1_ground_truth"]
    rq2 = report["rq2_rewrite_invariance"]
    rq3 = report["rq3_anti_arbitrage"]
    exposure_invariance = report["rq2_exposure_invariance"]
    scaling = report["rq4_scaling"]
    scaling_curves = {curve["dimension"]: curve for curve in scaling["curves"]}
    performance_cells = {(item["phase"], item["concurrency"]): item for item in performance["cells"]}
    direct_one = performance_cells[("business_sql", 1)]
    paired_one = performance_cells[("paired_snapshot", 1)]
    algebra_one = performance_cells[("paired_plus_algebra", 1)]
    full_one = performance_cells[("full_history_hit", 1)]
    full_four = performance_cells[("full_history_hit", 4)]
    full_eight = performance_cells[("full_history_hit", 8)]
    ramp = performance_cells[("full_history_ramp", 1)]
    performance_overhead = {item["concurrency"]: item for item in performance["full_vs_direct"]}
    paths = {item["path"]: item for item in path_analysis["paths"]}
    fresh_novel = paths["fresh_deployment_novel"]
    ramp_novel = paths["ramp_novel"]
    ramp_hit = paths["ramp_hit"]
    storage = path_analysis["ledger_storage"]
    storage_points = {(item["facts_per_ledger"], item["operation"]): item for item in storage_scaling["aggregates"]}
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
        rf"\newcommand{{\RQFourTrials}}{{{performance['trials']}}}",
        rf"\newcommand{{\RQFourObservations}}{{{comma(performance['observations'])}}}",
        rf"\newcommand{{\RQFourDirectOneMedian}}{{{decimal(direct_one['latency_ms']['p50'], 2)}}}",
        rf"\newcommand{{\RQFourPairedOneMedian}}{{{decimal(paired_one['latency_ms']['p50'], 2)}}}",
        rf"\newcommand{{\RQFourAlgebraOneMedian}}{{{decimal(algebra_one['latency_ms']['p50'], 2)}}}",
        rf"\newcommand{{\RQFourFullOneMedian}}{{{decimal(full_one['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourFullOneTail}}{{{decimal(full_one['latency_ms']['p95'])}}}",
        rf"\newcommand{{\RQFourFullOneQPS}}{{{decimal(full_one['throughput_qps'])}}}",
        rf"\newcommand{{\RQFourFullOneRatio}}{{{decimal(performance_overhead[1]['p50_latency_ratio'])}}}",
        rf"\newcommand{{\RQFourFullOneTailLow}}{{{decimal(full_one['p95_trial_range_ms'][0])}}}",
        rf"\newcommand{{\RQFourFullOneTailHigh}}{{{decimal(full_one['p95_trial_range_ms'][1])}}}",
        rf"\newcommand{{\RQFourDirectOneTailLow}}{{{decimal(direct_one['p95_trial_range_ms'][0], 2)}}}",
        rf"\newcommand{{\RQFourDirectOneTailHigh}}{{{decimal(direct_one['p95_trial_range_ms'][1], 2)}}}",
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
        rf"\newcommand{{\RQFourRampHitRate}}{{{decimal(100 * ramp['query_history_hit_rate'])}}}",
        rf"\newcommand{{\RQFourFreshNovelSamples}}{{{fresh_novel['samples']}}}",
        rf"\newcommand{{\RQFourFreshNovelMedian}}{{{decimal(fresh_novel['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourFreshNovelLow}}{{{decimal(fresh_novel['latency_ms']['p50_trial_range'][0])}}}",
        rf"\newcommand{{\RQFourFreshNovelHigh}}{{{decimal(fresh_novel['latency_ms']['p50_trial_range'][1])}}}",
        rf"\newcommand{{\RQFourNovelSamples}}{{{ramp_novel['samples']}}}",
        rf"\newcommand{{\RQFourNovelMedian}}{{{decimal(ramp_novel['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourNovelLow}}{{{decimal(ramp_novel['latency_ms']['p50_trial_range'][0])}}}",
        rf"\newcommand{{\RQFourNovelHigh}}{{{decimal(ramp_novel['latency_ms']['p50_trial_range'][1])}}}",
        rf"\newcommand{{\RQFourRampHitSamples}}{{{ramp_hit['samples']}}}",
        rf"\newcommand{{\RQFourRampHitMedian}}{{{decimal(ramp_hit['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourRampHitLow}}{{{decimal(ramp_hit['latency_ms']['p50_trial_range'][0])}}}",
        rf"\newcommand{{\RQFourRampHitHigh}}{{{decimal(ramp_hit['latency_ms']['p50_trial_range'][1])}}}",
        rf"\newcommand{{\RQFourPayloadBytes}}{{{comma(storage['canonical_payload_bytes'])}}}",
        rf"\newcommand{{\RQFourTableBytes}}{{{comma(storage['table_bytes'])}}}",
        rf"\newcommand{{\RQFourIndexBytes}}{{{comma(storage['index_bytes'])}}}",
        rf"\newcommand{{\RQFourAllocatedBytes}}{{{comma(storage['allocated_bytes'])}}}",
        rf"\newcommand{{\RQFourPayloadPerFact}}{{{decimal(storage['canonical_payload_bytes_per_fact'])}}}",
        rf"\newcommand{{\RQFourStorageTrials}}{{{storage_scaling['trials']}}}",
        rf"\newcommand{{\RQFourStorageMaxFacts}}{{{comma(storage_scaling['facts_per_ledger_sizes'][-1])}}}",
        rf"\newcommand{{\RQFourStorageMaxRows}}{{{comma(storage_points[(10000, 'novel')]['storage']['fact_rows']['median'])}}}",
        rf"\newcommand{{\RQFourStorageTenPayload}}{{{decimal(storage_points[(10, 'novel')]['storage']['canonical_payload_bytes']['median'] / 1048576, 3)}}}",
        rf"\newcommand{{\RQFourStorageTenAllocated}}{{{decimal(storage_points[(10, 'novel')]['storage']['allocated_bytes']['median'] / 1048576, 3)}}}",
        rf"\newcommand{{\RQFourStorageTenNovel}}{{{decimal(storage_points[(10, 'novel')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageTenReplay}}{{{decimal(storage_points[(10, 'replay')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageHundredPayload}}{{{decimal(storage_points[(100, 'novel')]['storage']['canonical_payload_bytes']['median'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourStorageHundredAllocated}}{{{decimal(storage_points[(100, 'novel')]['storage']['allocated_bytes']['median'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourStorageHundredNovel}}{{{decimal(storage_points[(100, 'novel')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageHundredReplay}}{{{decimal(storage_points[(100, 'replay')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageThousandPayload}}{{{decimal(storage_points[(1000, 'novel')]['storage']['canonical_payload_bytes']['median'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourStorageThousandAllocated}}{{{decimal(storage_points[(1000, 'novel')]['storage']['allocated_bytes']['median'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourStorageThousandNovel}}{{{decimal(storage_points[(1000, 'novel')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageThousandReplay}}{{{decimal(storage_points[(1000, 'replay')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageTenThousandPayload}}{{{decimal(storage_points[(10000, 'novel')]['storage']['canonical_payload_bytes']['median'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourStorageTenThousandAllocated}}{{{decimal(storage_points[(10000, 'novel')]['storage']['allocated_bytes']['median'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourStorageTenThousandNovel}}{{{decimal(storage_points[(10000, 'novel')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageTenThousandNovelLow}}{{{decimal(storage_points[(10000, 'novel')]['settlement_ms']['trial_range'][0])}}}",
        rf"\newcommand{{\RQFourStorageTenThousandNovelHigh}}{{{decimal(storage_points[(10000, 'novel')]['settlement_ms']['trial_range'][1])}}}",
        rf"\newcommand{{\RQFourStorageTenThousandReplay}}{{{decimal(storage_points[(10000, 'replay')]['settlement_ms']['median'])}}}",
        rf"\newcommand{{\RQFourStorageTenThousandReplayLow}}{{{decimal(storage_points[(10000, 'replay')]['settlement_ms']['trial_range'][0])}}}",
        rf"\newcommand{{\RQFourStorageTenThousandReplayHigh}}{{{decimal(storage_points[(10000, 'replay')]['settlement_ms']['trial_range'][1])}}}",
        rf"\newcommand{{\RQFourBudgetBoundaryPassed}}{{{sum(1 for item in storage_scaling['budget_boundaries'] if item['rejected'])}}}",
        rf"\newcommand{{\RQFourScalingDims}}{{{len(scaling['curves'])}}}",
        rf"\newcommand{{\RQFourScalingMaxRows}}{{{comma(scaling_curves['observe_rows']['points'][-1]['size'])}}}",
        rf"\newcommand{{\RQFourScalingObserveMicros}}{{{decimal(scaling_curves['observe_rows']['points'][-1]['ns_per_op'] / 1000)}}}",
        rf"\newcommand{{\RQFourScalingNovelMax}}{{{comma(scaling_curves['novel_vs_replay']['points'][-1]['size'])}}}",
        rf"\newcommand{{\RQFourScalingReplayCharge}}{{{scaling_curves['novel_vs_replay']['points'][-1]['replay_charge']}}}",
        rf"\newcommand{{\RQTwoExposureRewrites}}{{{exposure_invariance['rewrites']}}}",
        rf"\newcommand{{\RQTwoExposureCases}}{{{exposure_invariance['cases']}}}",
        rf"\newcommand{{\RQTwoExposureDatasets}}{{{exposure_invariance['datasets']}}}",
        rf"\newcommand{{\RQTwoExposureNFCases}}{{{exposure_invariance['normal_form_checks']}}}",
        rf"\newcommand{{\RQTwoExposureEffectChecks}}{{{exposure_invariance['effect_checks']}}}",
        rf"\newcommand{{\RQTwoExposureMismatches}}{{{exposure_invariance['mismatches']}}}",
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
