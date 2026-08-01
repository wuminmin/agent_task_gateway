#!/usr/bin/env python3
"""Validate source-backed exposure evidence and emit conservative TeX macros."""

from __future__ import annotations

import hashlib
import io
import json
import re
import runpy
import subprocess
import tarfile
from pathlib import Path

from v4_evidence import validate_v4_evidence
from v4_supplemental_evidence import validate_v4_supplemental_evidence
from rq5_evidence import validate_rq5_evidence


PAPER_DIR = Path(__file__).resolve().parent
ROOT = PAPER_DIR.parent.parent
RESULT = ROOT / "evaluation/exposure/results.json"
CORPUS = ROOT / "evaluation/exposure/corpus.json"
RQ1_ORACLE = ROOT / "evaluation/exposureoracle/oracle.go"
PERFORMANCE = ROOT / "evaluation/exposure-performance/results.json"
PERFORMANCE_ENVIRONMENT = ROOT / "evaluation/exposure-performance/environment.json"
PERFORMANCE_SUMMARIZER = ROOT / "evaluation/exposure-performance/summarize_campaign.py"
PERFORMANCE_SOURCE_PROVENANCE = ROOT / "evaluation/exposure-performance/evidence/legacy-rq4-source.json"
PATH_ANALYSIS = ROOT / "evaluation/exposure-performance/path_analysis.json"
STORAGE_SCALING = ROOT / "evaluation/exposure-storage/results.json"
STORAGE_SOURCE_PROVENANCE = ROOT / "evaluation/exposure-performance/evidence/legacy-storage-source.json"
SCALE = ROOT / "evaluation/exposure-scale/results.json"
SCALE_SOURCE_PROVENANCE = ROOT / "evaluation/exposure-performance/evidence/legacy-scale-source.json"
FORMAL = ROOT / "formal/results/exposure_ledger.json"
FORMAL_BITMAP = ROOT / "formal/results/exposure_bitmap_refinement.json"
FORMAL_OUTCOME = ROOT / "formal/results/outcome_set_abstract_refinement.json"
FORMAL_ARTIFACT = ROOT / "formal/results/artifact_publication.json"
V5_OUTCOME = ROOT / "evaluation/v5-outcome/evidence.json"
V5_COMPOSE_RECEIPT = ROOT / "evaluation/v5-outcome/compose-receipt.json"
OUTPUT = PAPER_DIR / "generated/evidence.tex"

V5_SOURCE_PATHS = (
    "config/catalog.yaml",
    "go.mod",
    "go.sum",
    "internal/control/migrations/018_predicate_footprint_v5.sql",
    "internal/control/ordinal_exposure_v5.go",
    "internal/control/ordinal_materialization_artifact_test.go",
    "internal/control/outcome_hashset_v5.go",
    "internal/control/outcome_hashset_v5_test.go",
    "internal/control/result.go",
    "internal/exposure/canonical_validation_test.go",
    "internal/exposure/fact.go",
    "internal/exposure/outcome_v5_test.go",
    "internal/gateway/exposure.go",
    "internal/gateway/result_artifact.go",
    "internal/gateway/result_artifact_test.go",
    "internal/gateway/service.go",
    "internal/queryplan/normalform.go",
    "internal/queryplan/predicate_footprint.go",
    "internal/queryreceipt/queryreceipt.go",
    "scripts/integration-test.sh",
    "scripts/record-compose-e2e.sh",
)

V5_MEASURED_PATHS = (
    "Dockerfile", "compose.yaml", "go.mod", "go.sum", "cmd", "internal",
    "config", "db", "scripts/compose-test.sh", "scripts/integration-test.sh",
)

V5_RAW_TEST_COMMAND = [
    "go", "test", "-json", "-count=1", "./internal/exposure", "./internal/control",
    "-run",
    "Test(ValidateCanonicalSQLValueEncodingCoversAdmissibleDomain|"
    "ValidateCanonicalSQLValueEncodingSpecialValues|"
    "ValidateCanonicalJSONBEncodingRejectsMalformedTrees|"
    "PredicateAtomsAcceptCanonicalTimeAndJSONB|"
    "CanonicalTimeValidatorMatchesEncoderAtMicrosecondBoundaries|"
    "V5SettlementAndSemanticReplayPostgres|"
    "OutcomeHashSetV5PostgresLoadsOnlyTouchedBranches|"
    "OutcomeHashSetV5SamePrefixMultiChunkBoundaries|"
    "NormalizeV5OutcomeFactsValidatesAtomCompositeBinding|"
    "OutcomeHashSetV5ExactDifferenceUnionAndReplay|"
    "OutcomeHashSetV5DeterministicAndTamperEvident)$",
]

V5_EXPECTED_TESTS = {
    "TestValidateCanonicalSQLValueEncodingCoversAdmissibleDomain",
    "TestValidateCanonicalSQLValueEncodingSpecialValues",
    "TestValidateCanonicalJSONBEncodingRejectsMalformedTrees",
    "TestPredicateAtomsAcceptCanonicalTimeAndJSONB",
    "TestCanonicalTimeValidatorMatchesEncoderAtMicrosecondBoundaries",
    "TestV5SettlementAndSemanticReplayPostgres",
    "TestOutcomeHashSetV5PostgresLoadsOnlyTouchedBranches",
    "TestOutcomeHashSetV5SamePrefixMultiChunkBoundaries",
    "TestNormalizeV5OutcomeFactsValidatesAtomCompositeBinding",
    "TestOutcomeHashSetV5ExactDifferenceUnionAndReplay",
    "TestOutcomeHashSetV5DeterministicAndTamperEvident",
}

PERFORMANCE_SOURCE_DIRS = (
    "internal",
    "cmd/gateway",
    "evaluation/cmd/exposure-bench",
)
PERFORMANCE_SOURCE_FILES = (
    "go.mod",
    "go.sum",
    "compose.yaml",
    "evaluation/Dockerfile",
    "evaluation/run-exposure-performance.sh",
    "evaluation/exposure-performance/compose.yaml",
    "evaluation/exposure-performance/catalog.yaml",
    "evaluation/exposure-performance/merge_memory.py",
    "evaluation/exposure-performance/summarize_campaign.py",
)

SCALE_SOURCE_DIRS = (
    "evaluation/cmd/exposure-bench",
    "internal/control",
    "internal/exposure",
    "internal/gateway",
    "internal/queryplan",
)
SCALE_SOURCE_FILES = (
    "go.mod",
    "go.sum",
    "cmd/gateway/main.go",
    "evaluation/exposure-scale/05-scale-data.sql",
    "evaluation/exposure-scale/15-scale-reader.sql",
    "evaluation/exposure-scale/catalog.yaml",
    "evaluation/exposure-scale/compose.yaml",
    "evaluation/exposure-scale/finalize.py",
    "evaluation/run-exposure-scale.sh",
)

STORAGE_SOURCE_DIRS = (
    "evaluation/cmd/exposure-storage",
    "internal/control",
    "internal/exposure",
)
STORAGE_SOURCE_FILES = ("go.mod", "go.sum")


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


def path_set_sha256(paths: list[Path]) -> str:
    digest = hashlib.sha256()
    for path in sorted(set(paths)):
        digest.update(path.relative_to(ROOT).as_posix().encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def performance_source_sha256() -> str:
    paths = [ROOT / relative for relative in PERFORMANCE_SOURCE_FILES]
    for relative in PERFORMANCE_SOURCE_DIRS:
        paths.extend((ROOT / relative).rglob("*.go"))
    return path_set_sha256(paths)


def validate_historical_source(
    provenance_path: Path,
    expected_campaign: str,
    reported_source_sha256: str,
    source_dirs: tuple[str, ...],
    source_files: tuple[str, ...],
    source_suffixes: tuple[str, ...],
) -> None:
    provenance = load_json(provenance_path)
    expected_archives = {
        "rq4-local-postgresql-20260728":
            "evaluation/exposure-performance/evidence/legacy-rq4-source-38a35d7.tar.gz",
        "rq4-control-postgresql-storage-scaling":
            "evaluation/exposure-performance/evidence/legacy-storage-source-38a35d7.tar.gz",
        "rq4-postgresql16-multiscale-join-group":
            "evaluation/exposure-performance/evidence/legacy-scale-source-38a35d7.tar.gz",
    }
    require(
        set(provenance) == {
            "schema_version", "campaign_id", "git_commit", "archive",
            "archive_sha256", "source_sha256", "source_file_count",
            "source_paths_sha256", "source_digest_algorithm",
        }
        and provenance.get("schema_version") == 1
        and provenance.get("campaign_id") == expected_campaign
        and provenance.get("git_commit") == "38a35d7bb3baff5a6f731b40a42dd4a26f28e29d"
        and provenance.get("source_digest_algorithm")
        == "SHA-256 over sorted UTF-8 path, NUL, exact bytes, NUL frames",
        f"legacy {expected_campaign} source-snapshot manifest is invalid",
    )
    archive_relative = provenance.get("archive", "")
    archive = ROOT / archive_relative
    require(
        archive_relative == expected_archives.get(expected_campaign)
        and archive.is_file()
        and not archive.is_symlink()
        and 0 < archive.stat().st_size <= 4 * 1024 * 1024
        and sha256(archive) == provenance.get("archive_sha256"),
        "legacy RQ4 source archive is missing or stale",
    )
    files: dict[str, bytes] = {}
    archive_raw = archive.read_bytes()
    try:
        with tarfile.open(fileobj=io.BytesIO(archive_raw), mode="r:gz") as handle:
            members = handle.getmembers()
            require(len({member.name for member in members}) == len(members),
                    "legacy RQ4 source archive repeats a member")
            for member in members:
                path = Path(member.name)
                require(
                    member.name == path.as_posix()
                    and not path.is_absolute()
                    and ".." not in path.parts
                    and not member.issym()
                    and not member.islnk()
                    and (member.isdir() or member.isreg()),
                    f"unsafe legacy RQ4 source member {member.name!r}",
                )
                if member.isdir():
                    continue
                require(0 < member.size <= 32 * 1024 * 1024,
                        f"invalid legacy RQ4 source member size for {member.name}")
                extracted = handle.extractfile(member)
                require(extracted is not None, f"cannot read legacy RQ4 source member {member.name}")
                raw = extracted.read(member.size + 1)
                require(len(raw) == member.size,
                        f"legacy RQ4 source member {member.name} changed size")
                files[member.name] = raw
    except (tarfile.TarError, OSError) as error:
        raise ValueError(f"invalid legacy RQ4 source archive: {error}") from error

    require(
        len(files) == provenance.get("source_file_count")
        and all(
            relative in source_files
            or (
                Path(relative).suffix in source_suffixes
                and any(relative.startswith(directory + "/") for directory in source_dirs)
            )
            for relative in files
        ),
        "legacy RQ4 source archive file set is invalid",
    )
    path_digest = hashlib.sha256()
    source_digest = hashlib.sha256()
    for relative in sorted(files):
        encoded = relative.encode("utf-8")
        path_digest.update(encoded)
        path_digest.update(b"\0")
        source_digest.update(encoded)
        source_digest.update(b"\0")
        source_digest.update(files[relative])
        source_digest.update(b"\0")
    require(
        path_digest.hexdigest() == provenance.get("source_paths_sha256")
        and source_digest.hexdigest() == provenance.get("source_sha256")
        == reported_source_sha256,
        "legacy RQ4 historical source digest is not reproducible",
    )


def validate_exposure() -> dict:
    report = load_json(RESULT)
    require(
        report.get("schema_version") == 7
        and report.get("profile_version") == "taskgate-exposure-v3",
        "unsupported exposure report schema/profile",
    )
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
    outcome = rq3.get("outcome_probing", {})
    require(
        outcome.get("profile_version") == "taskgate-exposure-v3"
        and outcome.get("threshold_questions") == 3
        and outcome.get("distinct_plan_digests") == 3
        and outcome.get("identical_release_sets") is True
        and outcome.get("distinct_outcome_facts") == 3
        and outcome.get("novel_outcome_charges") == 3
        and outcome.get("replay_outcome_charge") == 0
        and outcome.get("equivalent_rewrite_charge") == 0
        and outcome.get("release_charge_after_first") == 0
        and outcome.get("influence_charge_after_first") == 0
        and outcome.get("passed") is True,
        "RQ3 outcome-probing evidence is incomplete",
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
    namespace = runpy.run_path(str(PERFORMANCE_SUMMARIZER))
    reproduced = namespace["build_summary"](namespace["DEFAULT_RUNS"])
    reproduced_source = reproduced.pop("gateway_benchmark_source_sha256", None)
    published_without_source = dict(result)
    published_without_source.pop("gateway_benchmark_source_sha256", None)
    require(
        reproduced_source == performance_source_sha256()
        and reproduced == published_without_source,
        "RQ4 raw campaign cannot reproduce the published non-source summary",
    )
    validate_historical_source(
        PERFORMANCE_SOURCE_PROVENANCE,
        result.get("campaign_id", ""),
        result.get("gateway_benchmark_source_sha256", ""),
        PERFORMANCE_SOURCE_DIRS,
        PERFORMANCE_SOURCE_FILES,
        (".go",),
    )
    require(result.get("schema_version") == 2, "unsupported RQ4 report schema")
    require(result.get("status") == "complete_controlled_local_campaign", "RQ4 campaign is incomplete")
    require(result.get("trials") == 3 and result.get("observations") == 31296, "RQ4 trial/sample count is incomplete")
    require(
        result.get("operation_partition") == {"full_path": 7896, "ablations": 23400, "total": 31296},
        "RQ4 full-path/ablation operation partition is incorrect",
    )
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
        and "PostgreSQL 16.14" in result.get("postgres_version", ""),
        "RQ4 Control PostgreSQL storage-scaling campaign is stale",
    )
    validate_historical_source(
        STORAGE_SOURCE_PROVENANCE,
        "rq4-control-postgresql-storage-scaling",
        result.get("source_sha256", ""),
        STORAGE_SOURCE_DIRS,
        STORAGE_SOURCE_FILES,
        (".go", ".sql"),
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


def validate_scale() -> dict:
    result = load_json(SCALE)
    require(
        result.get("schema_version") == 1
        and result.get("status") == "complete_postgresql16_multiscale_join_group_campaign"
        and re.match(r"^16\.", result.get("postgres_version", "")) is not None,
        "RQ4 multi-scale PostgreSQL campaign is stale",
    )
    validate_historical_source(
        SCALE_SOURCE_PROVENANCE,
        "rq4-postgresql16-multiscale-join-group",
        result.get("source_sha256", ""),
        SCALE_SOURCE_DIRS,
        SCALE_SOURCE_FILES,
        (".go", ".sql"),
    )
    config = result.get("configuration", {})
    sizes = config.get("orders_per_scale")
    trials = config.get("trials")
    require(
        sizes == [1000, 10000, 45000]
        and trials == 3
        and config.get("lineitems_per_order") == 5
        and sizes[-1] * 23 >= 1_000_000,
        "RQ4 multi-scale configuration is incomplete",
    )
    provenance = result.get("raw_provenance", {})
    artifact_relative = provenance.get("artifact", "")
    artifact_path = ROOT / artifact_relative
    require(
        artifact_relative
        and artifact_path.is_file()
        and provenance.get("artifact_sha256") == sha256(artifact_path),
        "RQ4 multi-scale raw artifact is missing or stale",
    )
    points = {
        (item.get("orders"), item.get("trial"), item.get("operation")): item
        for item in result.get("raw_points", [])
    }
    require(len(points) == len(sizes) * trials * 3 == 27, "RQ4 multi-scale raw points are incomplete")
    for size in sizes:
        for trial in range(1, trials + 1):
            direct = points.get((size, trial, "direct_sql"), {})
            novel = points.get((size, trial, "novel"), {})
            replay = points.get((size, trial, "replay"), {})
            expected = size * 23
            require(
                direct.get("rows") == novel.get("rows") == replay.get("rows") == 3
                and novel.get("expected_influence_facts") == expected
                and novel.get("actual_influence_facts") == expected
                and novel.get("charged_influence_facts") == expected
                and novel.get("actual_release_facts") == 12
                and novel.get("charged_release_facts") == 12
                and replay.get("actual_influence_facts") == expected
                and replay.get("actual_release_facts") == 12
                and replay.get("charged_influence_facts") == 0
                and replay.get("charged_release_facts") == 0
                and replay.get("observation_sha256") == novel.get("observation_sha256")
                and replay.get("ledger_before", {}).get("fact_rows")
                == replay.get("ledger_after", {}).get("fact_rows")
                == novel.get("ledger_after", {}).get("fact_rows"),
                f"RQ4 multi-scale accounting is invalid at scale={size}, trial={trial}",
            )
    aggregates = {
        (item.get("orders"), item.get("operation")): item
        for item in result.get("aggregates", [])
    }
    require(
        len(aggregates) == len(sizes) * 3
        and all(item.get("trials") == trials for item in aggregates.values()),
        "RQ4 multi-scale aggregates are incomplete",
    )
    peaks = result.get("service_peak_memory_bytes", {})
    require(
        set(peaks) == {"control-postgres", "business-postgres", "gateway"}
        and all(value > 0 for value in peaks.values()),
        "RQ4 multi-scale peak-memory evidence is incomplete",
    )
    return result


def validate_formal(path: Path, label: str) -> dict:
    result = load_json(path)
    require(result.get("schema_version") == 1 and result.get("status") == "passed", f"{label} TLC did not pass")
    for field, digest_field in (
        ("model", "model_sha256"),
        ("config", "config_sha256"),
        ("raw_log", "log_sha256"),
    ):
        relative = result.get(field, "")
        artifact_path = ROOT / relative
        require(relative and artifact_path.is_file(), f"missing {label} formal artifact {relative!r}")
        require(sha256(artifact_path) == result.get(digest_field), f"stale {label} formal digest for {relative}")
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


def v5_source_manifest_digest(files: list[dict[str, str]]) -> str:
    canonical = json.dumps(files, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def validate_v5_raw_execution(receipt: dict) -> None:
    require(
        set(receipt) == {
            "raw_log", "raw_log_sha256", "command", "go_version",
            "postgres_version", "exit_code", "tests_passed", "tests_skipped",
            "tests_failed", "packages_passed",
        },
        "V5 raw execution receipt schema is invalid",
    )
    raw_relative = receipt.get("raw_log", "")
    raw_path = ROOT / raw_relative
    require(
        raw_relative == "evaluation/v5-outcome/raw/go-test.jsonl"
        and raw_path.is_file()
        and receipt.get("raw_log_sha256") == sha256(raw_path)
        and receipt.get("command") == V5_RAW_TEST_COMMAND
        and re.fullmatch(r"go version go1\.[0-9]+(?:\.[0-9]+)? linux/amd64", receipt.get("go_version", "")) is not None
        and re.fullmatch(r"PostgreSQL 16\.[0-9]+ .*", receipt.get("postgres_version", "")) is not None
        and receipt.get("exit_code") == 0,
        "V5 raw execution identity, command, runtime, or digest is invalid",
    )
    events = []
    for line_number, line in enumerate(raw_path.read_text(encoding="utf-8").splitlines(), 1):
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"V5 raw go-test log line {line_number} is not JSON") from exc
        require(isinstance(event, dict) and "Action" in event and "Package" in event,
                f"V5 raw go-test event {line_number} is malformed")
        events.append(event)
    passed = {
        event["Test"] for event in events
        if event.get("Action") == "pass" and event.get("Test") and "/" not in event["Test"]
    }
    skipped = {event["Test"] for event in events if event.get("Action") == "skip" and event.get("Test")}
    failed = {event["Test"] for event in events if event.get("Action") == "fail" and event.get("Test")}
    package_passes = {
        event["Package"] for event in events
        if event.get("Action") == "pass" and not event.get("Test")
    }
    package_failures = {
        event["Package"] for event in events
        if event.get("Action") == "fail" and not event.get("Test")
    }
    require(
        passed == V5_EXPECTED_TESTS
        and not skipped
        and not failed
        and package_passes == {
            "taskbound.local/agent-data-gateway/internal/exposure",
            "taskbound.local/agent-data-gateway/internal/control",
        }
        and not package_failures
        and receipt.get("tests_passed") == len(passed)
        and receipt.get("tests_skipped") == len(skipped)
        and receipt.get("tests_failed") == len(failed)
        and receipt.get("packages_passed") == len(package_passes),
        "V5 raw go-test receipt does not prove the exact required passing test set",
    )


def validate_v5_compose_execution(binding: dict, submission_commit: str) -> None:
    require(
        set(binding) == {"receipt", "receipt_sha256"}
        and binding.get("receipt") == "evaluation/v5-outcome/compose-receipt.json"
        and V5_COMPOSE_RECEIPT.is_file()
        and binding.get("receipt_sha256") == sha256(V5_COMPOSE_RECEIPT),
        "V5 Compose execution receipt binding is missing or stale",
    )
    receipt = load_json(V5_COMPOSE_RECEIPT)
    require(
        set(receipt) == {
            "schema_version", "submission_commit", "executed_at", "command",
            "compose_images", "catalog_sha256", "exit_code", "assertions",
            "raw_log", "raw_log_sha256",
        }
        and receipt.get("schema_version") == 1
        and receipt.get("submission_commit") == submission_commit
        and re.fullmatch(
            r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z",
            receipt.get("executed_at", ""),
        ) is not None
        and receipt.get("command") == ["./scripts/integration-test.sh"]
        and receipt.get("catalog_sha256") == sha256(ROOT / "config/catalog.yaml")
        and receipt.get("exit_code") == 0
        and receipt.get("assertions") == {
            "caller_predicate": True,
            "parquet_available": True,
            "promotion_recovery": True,
            "semantic_replay": True,
        },
        "V5 Compose execution identity or required assertions are invalid",
    )
    images = receipt.get("compose_images")
    expected_services = sorted({
        "control-postgres", "business-postgres", "snapshot-index-detail",
        "snapshot-index-summary", "result-object-store",
        "result-object-store-init", "gateway", "oa-demo",
    })
    require(
        isinstance(images, list)
        and [item.get("service") for item in images] == expected_services
        and all(
            isinstance(item, dict) and set(item) == {"service", "reference", "image_id"}
            and isinstance(item["service"], str) and item["service"]
            and isinstance(item["reference"], str) and item["reference"]
            and re.fullmatch(r"sha256:[0-9a-f]{64}", item["image_id"]) is not None
            for item in images
        ),
        "V5 Compose image identities are incomplete or malformed",
    )
    raw_relative = receipt.get("raw_log", "")
    raw_path = ROOT / raw_relative
    require(
        raw_relative == "evaluation/v5-outcome/raw/compose-e2e.log"
        and raw_path.is_file()
        and receipt.get("raw_log_sha256") == sha256(raw_path),
        "V5 Compose raw log is missing or stale",
    )
    log = raw_path.read_text(encoding="utf-8", errors="replace")
    for marker in (
        "ok - approved query creates an AVAILABLE canonical Parquet",
        "ok - V5 semantic replay avoided Business PostgreSQL and repeated exposure charge",
        "ok - caller SQL lowers through V5 atomization",
        "ok - canonical-copy/AVAILABLE-commit crash-window recovery passed",
        "all Compose end-to-end checks passed",
    ):
        require(marker in log, f"V5 Compose raw log omitted {marker!r}")


def validate_v5_outcome_evidence() -> dict:
    result = load_json(V5_OUTCOME)
    schema_version = result.get("schema_version")
    expected_fields = {
        "schema_version", "implementation_base_commit", "source_manifest",
        "raw_execution", "deterministic_set", "postgres_committed_graph",
        "same_prefix_multichunk",
    }
    if schema_version == 3:
        expected_fields |= {"submission_commit", "compose_execution"}
    require(
        set(result) == expected_fields and schema_version in {2, 3},
        "V5 outcome evidence schema is invalid",
    )
    base_commit = result.get("implementation_base_commit", "")
    commit_check = subprocess.run(
        ["git", "cat-file", "-e", f"{base_commit}^{{commit}}"],
        cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
    )
    ancestry_check = subprocess.run(
        ["git", "merge-base", "--is-ancestor", base_commit, "HEAD"], cwd=ROOT,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
    )
    require(
        re.fullmatch(r"[0-9a-f]{40}", base_commit) is not None
        and commit_check.returncode == 0
        and ancestry_check.returncode == 0,
        "V5 implementation base commit is missing or is not an ancestor of HEAD",
    )
    if schema_version == 3:
        submission_commit = result.get("submission_commit", "")
        submission_check = subprocess.run(
            ["git", "cat-file", "-e", f"{submission_commit}^{{commit}}"],
            cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            check=False,
        )
        submission_ancestry = subprocess.run(
            ["git", "merge-base", "--is-ancestor", submission_commit, "HEAD"],
            cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            check=False,
        )
        require(
            re.fullmatch(r"[0-9a-f]{40}", submission_commit) is not None
            and submission_check.returncode == 0
            and submission_ancestry.returncode == 0,
            "V5 submission commit is missing or is not an ancestor of HEAD",
        )
        measured_diff = subprocess.run(
            ["git", "diff", "--quiet", submission_commit, "--", *V5_MEASURED_PATHS],
            cwd=ROOT, check=False,
        )
        measured_status = subprocess.run(
            ["git", "status", "--porcelain", "--untracked-files=all", "--", *V5_MEASURED_PATHS],
            cwd=ROOT, capture_output=True, text=True, check=False,
        )
        require(
            measured_diff.returncode == 0 and not measured_status.stdout.strip(),
            "V5 measured paths differ from the frozen submission commit",
        )
        validate_v5_compose_execution(result.get("compose_execution", {}), submission_commit)
    manifest = result.get("source_manifest", {})
    require(
        set(manifest) == {"algorithm", "files", "sha256"}
        and manifest.get("algorithm") == "sha256(canonical-json(sorted-path-sha256-list))"
        and isinstance(manifest.get("files"), list),
        "V5 implementation source manifest schema is invalid",
    )
    files = manifest["files"]
    require(
        [item.get("path") for item in files if isinstance(item, dict)] == list(V5_SOURCE_PATHS)
        and all(set(item) == {"path", "sha256"} for item in files),
        "V5 implementation source manifest paths are incomplete or unordered",
    )
    for item in files:
        source = ROOT / item["path"]
        require(
            source.is_file() and re.fullmatch(r"[0-9a-f]{64}", item["sha256"]) is not None
            and sha256(source) == item["sha256"],
            f"V5 implementation source binding is stale for {item['path']}",
        )
    require(
        manifest.get("sha256") == v5_source_manifest_digest(files),
        "V5 implementation source-set digest is stale",
    )
    validate_v5_raw_execution(result.get("raw_execution", {}))
    require(
        result.get("deterministic_set") == {
            "members": 10000, "permutation_deterministic": True,
            "duplicate_idempotent": True, "tamper_rejected": True,
        }
        and result.get("postgres_committed_graph") == {
            "root_cardinality": 100000, "root_block_count": 256,
            "candidate_cardinality": 1, "blocks_loaded": 1,
            "leaves_loaded": 1, "hashes_loaded": 2,
            "blocks_reused": 255, "leaves_changed": 1,
            "replay_changed_objects": 0,
            "transaction_boundary": "commit_then_independent_merge",
        }
        and result.get("same_prefix_multichunk") == {
            "members": 8193, "prefix16": "4224", "chunk_size": 4096,
            "chunks_before": 3, "insert_positions": ["first", "middle", "last"],
            "full_rebuild_reference_match": True, "contiguous_rechunking": True,
            "missing_chunk_rejected": True, "replay_changed_objects": 0,
        },
        "V5 outcome evidence counters or asserted properties are invalid",
    )
    return result


def main() -> None:
    report = validate_exposure()
    performance = validate_performance()
    path_analysis = validate_path_analysis(performance)
    storage_scaling = validate_storage_scaling()
    scale = validate_scale()
    v4 = validate_v4_evidence(ROOT)
    v4_supplemental = validate_v4_supplemental_evidence(ROOT)
    rq5 = validate_rq5_evidence()
    formal = validate_formal(FORMAL, "exposure ledger")
    bitmap_formal = validate_formal(FORMAL_BITMAP, "bitmap refinement")
    outcome_formal = validate_formal(FORMAL_OUTCOME, "abstract outcome-set settlement")
    artifact_formal = validate_formal(FORMAL_ARTIFACT, "artifact publication")
    v5_outcome = validate_v5_outcome_evidence()
    rq1 = report["rq1_ground_truth"]
    rq2 = report["rq2_rewrite_invariance"]
    rq3 = report["rq3_anti_arbitrage"]
    outcome = rq3["outcome_probing"]
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
    scale_points = {(item["orders"], item["operation"]): item for item in scale["aggregates"]}
    scale_low = scale_points[(1000, "novel")]
    scale_mid = scale_points[(10000, "novel")]
    scale_high = scale_points[(45000, "novel")]
    scale_low_replay = scale_points[(1000, "replay")]
    scale_mid_replay = scale_points[(10000, "replay")]
    scale_high_replay = scale_points[(45000, "replay")]
    scale_low_direct = scale_points[(1000, "direct_sql")]
    scale_mid_direct = scale_points[(10000, "direct_sql")]
    scale_high_direct = scale_points[(45000, "direct_sql")]
    v4_stats = v4["stats"]
    v4_distribution = v4_supplemental["distribution"]
    v4_concurrency = v4_supplemental["concurrency"]
    v4_oracle = v4_supplemental["oracle"]
    v4_activation_ms = v4_stats["activation_ms"]
    v4_storage = {
        item["roots"]: item for item in v4_stats["storage_amortized_1_10_100_roots"]
    }
    baseline = report["charge_baselines"]
    first = baseline["full_first"]
    replay = baseline["full_replay"]
    lines = [
        "% Generated by paper/tkde/generate_evidence.py. Do not edit.",
        rf"\newcommand{{\ArchivedExposureProfile}}{{\texttt{{{report['profile_version']}}}}}",
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
        rf"\newcommand{{\RQThreeThresholdQuestions}}{{{outcome['threshold_questions']}}}",
        rf"\newcommand{{\RQThreeDistinctOutcomes}}{{{outcome['distinct_outcome_facts']}}}",
        rf"\newcommand{{\RQThreeOutcomeCharges}}{{{outcome['novel_outcome_charges']}}}",
        rf"\newcommand{{\RQThreeReplayOutcomeCharge}}{{{outcome['replay_outcome_charge']}}}",
        rf"\newcommand{{\RQThreeRewriteOutcomeCharge}}{{{outcome['equivalent_rewrite_charge']}}}",
        rf"\newcommand{{\RQFourTrials}}{{{performance['trials']}}}",
        rf"\newcommand{{\RQFourObservations}}{{{comma(performance['observations'])}}}",
        rf"\newcommand{{\RQFourFullPathOperations}}{{{comma(performance['operation_partition']['full_path'])}}}",
        rf"\newcommand{{\RQFourAblationOperations}}{{{comma(performance['operation_partition']['ablations'])}}}",
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
        rf"\newcommand{{\RQFourScaleTrials}}{{{scale['configuration']['trials']}}}",
        rf"\newcommand{{\RQFourScalePoints}}{{{len(scale['configuration']['orders_per_scale'])}}}",
        rf"\newcommand{{\RQFourScaleMaxOrders}}{{{comma(45000)}}}",
        rf"\newcommand{{\RQFourScaleMaxJoined}}{{{comma(scale_high['joined_rows'])}}}",
        rf"\newcommand{{\RQFourScaleMaxFacts}}{{{comma(scale_high['expected_influence_facts'])}}}",
        rf"\newcommand{{\RQFourScaleLowFactsPlot}}{{{scale_low['expected_influence_facts']}}}",
        rf"\newcommand{{\RQFourScaleMidFactsPlot}}{{{scale_mid['expected_influence_facts']}}}",
        rf"\newcommand{{\RQFourScaleHighFactsPlot}}{{{scale_high['expected_influence_facts']}}}",
        rf"\newcommand{{\RQFourScaleReleaseFacts}}{{{12}}}",
        rf"\newcommand{{\RQFourScaleGatewayGiB}}{{{decimal(scale['service_peak_memory_bytes']['gateway'] / 1073741824)}}}",
        rf"\newcommand{{\RQFourScaleControlGiB}}{{{decimal(scale['service_peak_memory_bytes']['control-postgres'] / 1073741824)}}}",
        rf"\newcommand{{\RQFourScaleLowDirectMS}}{{{decimal(scale_low_direct['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourScaleMidDirectMS}}{{{decimal(scale_mid_direct['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourScaleHighDirectMS}}{{{decimal(scale_high_direct['latency_ms']['p50'])}}}",
        rf"\newcommand{{\RQFourScaleLowDirectS}}{{{decimal(scale_low_direct['latency_ms']['p50'] / 1000, 4)}}}",
        rf"\newcommand{{\RQFourScaleMidDirectS}}{{{decimal(scale_mid_direct['latency_ms']['p50'] / 1000, 4)}}}",
        rf"\newcommand{{\RQFourScaleHighDirectS}}{{{decimal(scale_high_direct['latency_ms']['p50'] / 1000, 4)}}}",
        rf"\newcommand{{\RQFourScaleLowNovelS}}{{{decimal(scale_low['latency_ms']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleMidNovelS}}{{{decimal(scale_mid['latency_ms']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighNovelS}}{{{decimal(scale_high['latency_ms']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleLowReplayS}}{{{decimal(scale_low_replay['latency_ms']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleMidReplayS}}{{{decimal(scale_mid_replay['latency_ms']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighReplayS}}{{{decimal(scale_high_replay['latency_ms']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighNovelLowS}}{{{decimal(scale_high['latency_ms']['min'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighNovelHighS}}{{{decimal(scale_high['latency_ms']['max'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighReplayLowS}}{{{decimal(scale_high_replay['latency_ms']['min'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighReplayHighS}}{{{decimal(scale_high_replay['latency_ms']['max'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighNovelDeriveS}}{{{decimal(scale_high['component_ms']['exposure_derivation']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighReplayDeriveS}}{{{decimal(scale_high_replay['component_ms']['exposure_derivation']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighNovelStoreS}}{{{decimal(scale_high['component_ms']['exposure_fact_store']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourScaleHighReplayStoreS}}{{{decimal(scale_high_replay['component_ms']['exposure_fact_store']['p50'] / 1000)}}}",
        rf"\newcommand{{\RQFourVFourProfile}}{{\texttt{{{v4_stats['profile_version']}}}}}",
        rf"\newcommand{{\RQFourVFourSourceHash}}{{\texttt{{{v4_stats['source_sha256'][:12]}}}}}",
        rf"\newcommand{{\RQFourVFourResultHash}}{{\texttt{{{v4_stats['results_sha256'][:12]}}}}}",
        rf"\newcommand{{\RQFourVFourEnvironmentHash}}{{\texttt{{{v4_stats['environment_sha256'][:12]}}}}}",
        rf"\newcommand{{\RQFourVFourGatesPassed}}{{{v4_stats['gate_count']}}}",
        rf"\newcommand{{\RQFourVFourGatesTotal}}{{{v4_stats['gate_count']}}}",
        rf"\newcommand{{\RQFourVFourSamples}}{{{v4_stats['sample_count']}}}",
        rf"\newcommand{{\RQFourVFourCases}}{{{v4_stats['case_count']}}}",
        rf"\newcommand{{\RQFourVFourRoots}}{{{v4_stats['root_count']}}}",
        rf"\newcommand{{\RQFourVFourShapes}}{{{v4_stats['shape_count']}}}",
        rf"\newcommand{{\RQFourVFourOverlaps}}{{{v4_stats['overlap_count']}}}",
        rf"\newcommand{{\RQFourVFourMaxSamples}}{{{v4_stats['max_point_samples']}}}",
        rf"\newcommand{{\RQFourVFourReleaseFacts}}{{{v4_stats['max_release_facts']}}}",
        rf"\newcommand{{\RQFourVFourInfluenceFacts}}{{{comma(v4_stats['max_influence_facts'])}}}",
        rf"\newcommand{{\RQFourVFourOutcomeFacts}}{{{v4_stats['max_outcome_facts']}}}",
        rf"\newcommand{{\RQFourVFourDirectMedianMS}}{{{decimal(v4_stats['direct_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourDirectTailMS}}{{{decimal(v4_stats['direct_p95_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourNovelMedianS}}{{{decimal(v4_stats['novel_p50_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFourVFourNovelTailS}}{{{decimal(v4_stats['novel_p95_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFourVFourReplayMedianMS}}{{{decimal(v4_stats['replay_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourReplayTailMS}}{{{decimal(v4_stats['replay_p95_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourLegacyNovelSpeedup}}{{{decimal(scale_high['latency_ms']['p50'] / v4_stats['novel_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourLegacyReplaySpeedup}}{{{decimal(scale_high_replay['latency_ms']['p50'] / v4_stats['replay_p50_ms'], 1)}}}",
        rf"\newcommand{{\RQFourVFourNovelReductionPct}}{{{decimal(100 * (1 - v4_stats['novel_p50_ms'] / scale_high['latency_ms']['p50']), 2)}}}",
        rf"\newcommand{{\RQFourVFourNovelDirectRatio}}{{{decimal(v4_stats['novel_over_direct_ratio'], 2)}}}",
        rf"\newcommand{{\RQFourVFourReplayNoSQLSamples}}{{{v4_stats['replay_no_business_sql_samples']}}}",
        rf"\newcommand{{\RQFourVFourGatewayPeakMiB}}{{{decimal(v4_stats['gateway_peak_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourBuildRuns}}{{{len(v4['report']['index_build']['runs'])}}}",
        rf"\newcommand{{\RQFourVFourIndexBuildS}}{{{decimal(v4_stats['index_build_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFourVFourBuilderPeakGiB}}{{{decimal(v4_stats['index_builder_peak_rss_bytes'] / 1073741824, 2)}}}",
        rf"\newcommand{{\RQFourVFourArtifactGiB}}{{{decimal(v4_stats['artifact_total_bytes'] / 1073741824, 2)}}}",
        rf"\newcommand{{\RQFourVFourHotArtifactMiB}}{{{decimal(v4_stats['artifact_hot_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourStrictVerifyS}}{{{decimal(v4_stats['activation_verification_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFourVFourActivationRuns}}{{{len(v4_activation_ms)}}}",
        rf"\newcommand{{\RQFourVFourActivationMinS}}{{{decimal(min(v4_activation_ms) / 1000, 3)}}}",
        rf"\newcommand{{\RQFourVFourActivationMaxS}}{{{decimal(max(v4_activation_ms) / 1000, 3)}}}",
        rf"\newcommand{{\RQFourVFourOrdinalStreamMedianMS}}{{{decimal(v4_stats['ordinal_stream_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourProvenancePGMedianMS}}{{{decimal(v4_stats['provenance_postgresql_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourBitmapDeriveMedianMS}}{{{decimal(v4_stats['bitmap_derivation_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourSettleMedianMS}}{{{decimal(v4_stats['settlement_p50_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourSmallRawSamples}}{{{v4_stats['small_query_raw_samples']}}}",
        rf"\newcommand{{\RQFourVFourSmallHitSamples}}{{{v4_stats['small_query_hit_samples']}}}",
        rf"\newcommand{{\RQFourVFourSmallLatencyImprovementPct}}{{{decimal(v4_stats['small_query_latency_improvement_percent'], 1)}}}",
        rf"\newcommand{{\RQFourVFourSmallThroughputImprovementPct}}{{{decimal(v4_stats['small_query_throughput_improvement_percent'], 1)}}}",
        rf"\newcommand{{\RQFourVFourNetworkSamples}}{{{v4_stats['sample_count']}}}",
        rf"\newcommand{{\RQFourVFourNetworkRXGiB}}{{{decimal(v4_stats['network_rx_bytes'] / 1073741824, 2)}}}",
        rf"\newcommand{{\RQFourVFourNetworkTXMiB}}{{{decimal(v4_stats['network_tx_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourWALSamples}}{{{v4_stats['sample_count']}}}",
        rf"\newcommand{{\RQFourVFourBusinessWALKiB}}{{{decimal(v4_stats['wal_business_bytes'] / 1024, 2)}}}",
        rf"\newcommand{{\RQFourVFourControlWALMiB}}{{{decimal(v4_stats['wal_control_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourFixedControlKiB}}{{{decimal(v4_stats['storage_fixed_control_bytes'] / 1024, 0)}}}",
        rf"\newcommand{{\RQFourVFourRuntimeControlMiB}}{{{decimal(v4_stats['storage_runtime_control_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourStorageOneRootGiB}}{{{decimal(v4_storage[1]['estimated_bytes_per_root'] / 1073741824, 3)}}}",
        rf"\newcommand{{\RQFourVFourStorageTenRootsMiB}}{{{decimal(v4_storage[10]['estimated_bytes_per_root'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourStorageHundredRootsMiB}}{{{decimal(v4_storage[100]['estimated_bytes_per_root'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourSupplementSourceHash}}{{\texttt{{{v4_supplemental['stats']['source_scope_sha256'][:12]}}}}}",
        rf"\newcommand{{\RQFourVFourDistributionCells}}{{{v4_distribution['cell_count']}}}",
        rf"\newcommand{{\RQFourVFourDistributionRuns}}{{{v4_distribution['runs_per_cell']}}}",
        rf"\newcommand{{\RQFourVFourDistributionRoundTrips}}{{{v4_distribution['portable_round_trip_checks']}}}",
        rf"\newcommand{{\RQFourVFourDistributionWorstTailMS}}{{{decimal(v4_distribution['worst_andnot_or_p95_ms'], 2)}}}",
        rf"\newcommand{{\RQFourVFourDistributionPeakHeapMiB}}{{{decimal(v4_distribution['max_peak_heap_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourConcurrencyCases}}{{{v4_concurrency['case_count']}}}",
        rf"\newcommand{{\RQFourVFourConcurrencyGateways}}{{{v4_concurrency['gateway_count']}}}",
        rf"\newcommand{{\RQFourVFourConcurrencyWidths}}{{{'/'.join(str(value) for value in v4_concurrency['concurrency_levels'])}}}",
        rf"\newcommand{{\RQFourVFourConcurrencyZeroNovelty}}{{{v4_concurrency['total_zero_novelty_settlements']}}}",
        rf"\newcommand{{\RQFourVFourRootLockWaiters}}{{{v4_concurrency['total_root_lock_waiters']}}}",
        rf"\newcommand{{\RQFourVFourOracleFacts}}{{{comma(v4_oracle['total_compared'])}}}",
        rf"\newcommand{{\RQFourVFourOracleMismatches}}{{{v4_oracle['total_mismatches']}}}",
        rf"\newcommand{{\RQFourVFourOracleWitnesses}}{{{v4_oracle['witnesses']}}}",
        rf"\newcommand{{\RQFourVFourOracleMultiplicity}}{{{comma(v4_oracle['witness_multiplicity'])}}}",
        rf"\newcommand{{\RQFourVFourOracleDurationS}}{{{decimal(v4_oracle['duration_seconds'], 2)}}}",
        rf"\newcommand{{\RQFourVFourOraclePeakRSSMiB}}{{{decimal(v4_oracle['peak_rss_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourOracleSpoolMiB}}{{{decimal(v4_oracle['spool_bytes'] / 1048576, 2)}}}",
        rf"\newcommand{{\RQFourVFourOracleColdFacts}}{{{comma(v4_oracle['cold_facts_scanned'])}}}",
        rf"\newcommand{{\RQFourVFourOracleSortRuns}}{{{v4_oracle['sort_runs']}}}",
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
        rf"\newcommand{{\BitmapFormalStates}}{{{comma(bitmap_formal['states_generated'])}}}",
        rf"\newcommand{{\BitmapFormalDistinct}}{{{comma(bitmap_formal['distinct_states'])}}}",
        rf"\newcommand{{\BitmapFormalDepth}}{{{bitmap_formal['search_depth']}}}",
        rf"\newcommand{{\OutcomeFormalStates}}{{{comma(outcome_formal['states_generated'])}}}",
        rf"\newcommand{{\OutcomeFormalDistinct}}{{{comma(outcome_formal['distinct_states'])}}}",
        rf"\newcommand{{\OutcomeFormalDepth}}{{{outcome_formal['search_depth']}}}",
        rf"\newcommand{{\ArtifactFormalStates}}{{{comma(artifact_formal['states_generated'])}}}",
        rf"\newcommand{{\ArtifactFormalDistinct}}{{{comma(artifact_formal['distinct_states'])}}}",
        rf"\newcommand{{\ArtifactFormalDepth}}{{{artifact_formal['search_depth']}}}",
        rf"\newcommand{{\VFiveImplementationCommit}}{{\texttt{{{v5_outcome['implementation_base_commit']}}}}}",
        rf"\newcommand{{\VFiveImplementationSourceHash}}{{\nolinkurl{{{v5_outcome['source_manifest']['sha256']}}}}}",
        rf"\newcommand{{\VFiveRawLogHash}}{{\nolinkurl{{{v5_outcome['raw_execution']['raw_log_sha256']}}}}}",
        rf"\newcommand{{\VFiveRawTestsPassed}}{{{v5_outcome['raw_execution']['tests_passed']}}}",
        rf"\newcommand{{\VFiveDeterministicMembers}}{{{comma(v5_outcome['deterministic_set']['members'])}}}",
        rf"\newcommand{{\VFiveRootCardinality}}{{{comma(v5_outcome['postgres_committed_graph']['root_cardinality'])}}}",
        rf"\newcommand{{\VFiveRootBlocks}}{{{v5_outcome['postgres_committed_graph']['root_block_count']}}}",
        rf"\newcommand{{\VFiveBlocksLoaded}}{{{v5_outcome['postgres_committed_graph']['blocks_loaded']}}}",
        rf"\newcommand{{\VFiveLeavesLoaded}}{{{v5_outcome['postgres_committed_graph']['leaves_loaded']}}}",
        rf"\newcommand{{\VFiveHashesLoaded}}{{{v5_outcome['postgres_committed_graph']['hashes_loaded']}}}",
        rf"\newcommand{{\VFiveBlocksReused}}{{{v5_outcome['postgres_committed_graph']['blocks_reused']}}}",
        rf"\newcommand{{\VFiveLeavesChanged}}{{{v5_outcome['postgres_committed_graph']['leaves_changed']}}}",
        rf"\newcommand{{\VFiveReplayChangedObjects}}{{{v5_outcome['postgres_committed_graph']['replay_changed_objects']}}}",
        rf"\newcommand{{\VFiveSamePrefixMembers}}{{{comma(v5_outcome['same_prefix_multichunk']['members'])}}}",
    ]
    rq5_offline = rq5["offline"]
    rq5_online = rq5["online"]
    rq5_metrics = rq5_offline["metrics"]
    rq5_days = rq5_offline["days"]
    rq5_times = rq5_online["timing_ms"]
    lines.extend([
        rf"\newcommand{{\RQFiveRows}}{{{comma(rq5_offline['rows_per_publication'])}}}",
        rf"\newcommand{{\RQFiveFacts}}{{{comma(rq5_offline['facts_per_publication'])}}}",
        rf"\newcommand{{\RQFiveBuildRunsPerDay}}{{{rq5_offline['measured_builds_per_publication']}}}",
        rf"\newcommand{{\RQFiveTotalBuildRuns}}{{{rq5_offline['publication_count'] * rq5_offline['measured_builds_per_publication']}}}",
        rf"\newcommand{{\RQFiveMaxBuildS}}{{{decimal(rq5_metrics['maximum_build_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFiveMaxVerifyS}}{{{decimal(rq5_metrics['maximum_strict_verify_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFiveMaxActivateS}}{{{decimal(rq5_metrics['maximum_activation_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFiveMaxCycleS}}{{{decimal(rq5_metrics['maximum_cycle_ms'] / 1000, 3)}}}",
        rf"\newcommand{{\RQFiveBuilderPeakGiB}}{{{decimal(rq5_metrics['maximum_builder_peak_rss_bytes'] / 1073741824, 2)}}}",
        rf"\newcommand{{\RQFiveArtifactGiB}}{{{decimal(max(rq5_metrics['artifact_bytes_by_day'].values()) / 1073741824, 2)}}}",
        rf"\newcommand{{\RQFiveHotArtifactMiB}}{{{decimal(max(rq5_metrics['hot_artifact_bytes_by_day'].values()) / 1048576, 2)}}}",
        rf"\newcommand{{\RQFiveTransitions}}{{{rq5_online['transition_count']}}}",
        rf"\newcommand{{\RQFiveChecksPassed}}{{{sum(value['status'] == 'pass' for value in rq5_online['conditions'].values())}}}",
        rf"\newcommand{{\RQFiveChecksTotal}}{{{len(rq5_online['conditions'])}}}",
        rf"\newcommand{{\RQFiveSwitchMedianMS}}{{{decimal(rq5_times['switch']['p50'], 6)}}}",
        rf"\newcommand{{\RQFiveSwitchTailMS}}{{{decimal(rq5_times['switch']['p95'], 6)}}}",
        rf"\newcommand{{\RQFiveFirstQueryMedianMS}}{{{decimal(rq5_times['first_query']['p50'], 3)}}}",
        rf"\newcommand{{\RQFiveFirstQueryTailMS}}{{{decimal(rq5_times['first_query']['p95'], 3)}}}",
        rf"\newcommand{{\RQFiveReplayMedianMS}}{{{decimal(rq5_times['replay']['p50'], 3)}}}",
        rf"\newcommand{{\RQFiveReplayTailMS}}{{{decimal(rq5_times['replay']['p95'], 3)}}}",
    ])
    for day, label in (
        ("day0", "DayZero"),
        ("day1", "DayOne"),
        ("day2", "DayTwo"),
        ("day3", "DayThree"),
    ):
        value = rq5_days[day]
        lines.extend([
            rf"\newcommand{{\RQFive{label}BuildMedianS}}{{{decimal(value['build_ms']['p50'] / 1000, 3)}}}",
            rf"\newcommand{{\RQFive{label}BuildTailS}}{{{decimal(value['build_ms']['p95'] / 1000, 3)}}}",
            rf"\newcommand{{\RQFive{label}VerifyMedianS}}{{{decimal(value['strict_verify_ms']['p50'] / 1000, 3)}}}",
            rf"\newcommand{{\RQFive{label}ActivateMedianS}}{{{decimal(value['activation_ms']['p50'] / 1000, 3)}}}",
            rf"\newcommand{{\RQFive{label}CycleMaxS}}{{{decimal(value['cycle_ms']['max'] / 1000, 3)}}}",
            rf"\newcommand{{\RQFive{label}ArtifactGiB}}{{{decimal(value['artifact_bytes'] / 1073741824, 2)}}}",
        ])
    for value, label in zip(
        rq5_online["transitions"],
        ("DayZeroToOne", "DayOneToTwo", "DayTwoToThree"),
        strict=True,
    ):
        timing = value["timing_ms"]
        lines.extend([
            rf"\newcommand{{\RQFive{label}SwitchMS}}{{{decimal(timing['switch_ms'], 6)}}}",
            rf"\newcommand{{\RQFive{label}FirstQueryMS}}{{{decimal(timing['first_query_ms'], 3)}}}",
            rf"\newcommand{{\RQFive{label}ReplayMS}}{{{decimal(timing['replay_ms'], 3)}}}",
        ])
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    OUTPUT.write_text("\n".join(lines) + "\n", encoding="ascii")
    print(f"ok - generated {OUTPUT.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
