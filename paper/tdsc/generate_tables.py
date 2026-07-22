#!/usr/bin/env python3
"""Generate paper tables without inventing experimental results.

The generator accepts a source-backed summary at
``evaluation/generated/paper-results.json``.  That file must itself name the
raw inputs from which it was generated.  Missing, malformed, or incomplete data
is rendered as ``not measured``.  No numeric fallback is embedded here.
"""

from __future__ import annotations

import json
import hashlib
import math
import re
from pathlib import Path
from typing import Any


PAPER_DIR = Path(__file__).resolve().parent
ROOT = PAPER_DIR.parents[1]
OUT = PAPER_DIR / "generated"
SUMMARY = ROOT / "evaluation" / "generated" / "paper-results.json"
PERFORMANCE_BASELINES = {
    "direct_postgresql": "Direct PostgreSQL",
    "native_view": "Native View",
    "ast_only_gateway": "AST-only Gateway",
    "full_taskgate": "Full TaskGate",
}
FUZZ_TARGETS = {
    "FuzzAuthorizeNeverPanics",
    "FuzzFormattingMetamorphic",
    "FuzzQueryPlanCompileNeverPanics",
}
FORMAL_RESULT_FILES = (
    ("TaskGate core", ROOT / "formal" / "results" / "tlc.json"),
    ("Vector budget", ROOT / "formal" / "results" / "vector_budget.json"),
    ("SQL authorization", ROOT / "formal" / "results" / "sql_authorization.json"),
    ("Multi-task audit", ROOT / "formal" / "results" / "multi_task_audit.json"),
    ("Receipt/audit", ROOT / "formal" / "results" / "receipt_audit.json"),
    ("Recovery liveness", ROOT / "formal" / "results" / "recovery_liveness.json"),
)


def latex_escape(value: Any) -> str:
    text = str(value)
    replacements = {
        "\\": r"\textbackslash{}",
        "&": r"\&",
        "%": r"\%",
        "$": r"\$",
        "#": r"\#",
        "_": r"\_",
        "{": r"\{",
        "}": r"\}",
        "~": r"\textasciitilde{}",
        "^": r"\textasciicircum{}",
    }
    return "".join(replacements.get(char, char) for char in text)


def write(name: str, body: str) -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    (OUT / name).write_text(body.rstrip() + "\n", encoding="utf-8")


def is_finite_number(value: Any) -> bool:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    try:
        return math.isfinite(value)
    except (OverflowError, TypeError, ValueError):
        return False


def verify_repository_input(raw_path: Any, expected: Any) -> tuple[Path | None, str]:
    if not isinstance(raw_path, str) or not isinstance(expected, str):
        return None, "raw-input provenance malformed"
    if re.fullmatch(r"[0-9a-f]{64}", expected) is None:
        return None, "raw-input digest malformed"
    candidate = Path(raw_path)
    if candidate.is_absolute():
        return None, "raw-input path must be repository-relative"
    resolved = (ROOT / candidate).resolve()
    try:
        resolved.relative_to(ROOT.resolve())
    except ValueError:
        return None, "raw-input path escapes repository"
    if not resolved.is_file():
        return None, "raw input absent"
    digest = hashlib.sha256()
    try:
        with resolved.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError:
        return None, "raw input unreadable"
    if digest.hexdigest() != expected:
        return None, "raw-input digest mismatch"
    return resolved, "verified"


def load_summary() -> tuple[dict[str, Any] | None, str]:
    if not SUMMARY.is_file():
        return None, "source-backed summary absent"
    try:
        data = json.loads(SUMMARY.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None, "source-backed summary malformed"
    if not isinstance(data, dict) or data.get("schema_version") != 1:
        return None, "unsupported summary schema"
    provenance = data.get("provenance")
    if not isinstance(provenance, dict):
        return None, "provenance absent"
    raw_inputs = provenance.get("raw_inputs")
    if not isinstance(raw_inputs, list) or not raw_inputs:
        return None, "raw-input provenance absent"
    for item in raw_inputs:
        if not isinstance(item, dict) or not item.get("path") or not item.get("sha256"):
            return None, "raw-input provenance incomplete"
        _, error = verify_repository_input(item["path"], item["sha256"])
        if error != "verified":
            return None, error
    return data, "source-backed summary loaded"


def scalar(record: dict[str, Any], key: str) -> str:
    value = record.get(key)
    if isinstance(value, bool):
        return "yes" if value else "no"
    if is_finite_number(value):
        return latex_escape(value)
    if isinstance(value, str) and value.strip():
        return latex_escape(value.strip())
    return r"\notmeasured"


def display_path(path: Path) -> str:
    try:
        return path.resolve().relative_to(ROOT.resolve()).as_posix()
    except ValueError:
        return path.resolve().as_posix()


def load_formal_result(result_path: Path) -> tuple[dict[str, Any] | None, str]:
    if not result_path.is_file():
        return None, f"TLC result absent: {display_path(result_path)}"
    try:
        formal = json.loads(result_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None, f"TLC result malformed: {display_path(result_path)}"
    if (
        not isinstance(formal, dict)
        or formal.get("schema_version") != 1
        or formal.get("status") not in {"passed", "failed"}
        or formal.get("tool") != "TLC"
        or not isinstance(formal.get("tool_version"), str)
        or not formal["tool_version"].strip()
    ):
        return None, f"TLC result schema invalid: {display_path(result_path)}"
    exit_code = formal.get("exit_code")
    if type(exit_code) is not int or (formal["status"] == "passed") != (exit_code == 0):
        return None, f"TLC status and exit code disagree: {display_path(result_path)}"

    verified: dict[str, Path] = {}
    for label, path_key, digest_key in (
        ("model", "model", "model_sha256"),
        ("config", "config", "config_sha256"),
        ("log", "raw_log", "log_sha256"),
    ):
        resolved, error = verify_repository_input(formal.get(path_key), formal.get(digest_key))
        if resolved is None:
            return None, f"{display_path(result_path)} TLC {label}: {error}"
        verified[label] = resolved

    try:
        log_text = verified["log"].read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return None, f"TLC log unreadable: {display_path(verified['log'])}"

    # Numeric fields in tlc.json are metadata, not primary evidence.  Remove
    # them before examining the log so a parse failure can never fall back to a
    # manually edited JSON count.
    declared_stats = {
        key: formal.pop(key, None)
        for key in ("states_generated", "distinct_states", "search_depth")
    }
    success = list(
        re.finditer(
            r"(?m)^Model checking completed\. No error has been found\.$",
            log_text,
        )
    )
    counts = list(
        re.finditer(
            r"(?m)^([0-9][0-9,]*) states generated, "
            r"([0-9][0-9,]*) distinct states found, 0 states left on queue\.$",
            log_text,
        )
    )
    depths = list(
        re.finditer(
            r"(?m)^The depth of the complete state graph search is "
            r"([0-9][0-9,]*)\.$",
            log_text,
        )
    )

    if formal["status"] == "failed":
        if success:
            return None, f"TLC failed status conflicts with no-error completion marker: {display_path(result_path)}"
        return formal, "verified TLC failure; no passing statistics reported"

    if len(success) != 1:
        return None, f"TLC no-error completion marker absent or ambiguous: {display_path(result_path)}"
    if len(counts) != 1 or len(depths) != 1:
        return None, f"TLC final state/depth statistics absent or ambiguous: {display_path(result_path)}"
    if not success[0].start() < counts[0].start() < depths[0].start():
        return None, f"TLC completion and final-statistics order invalid: {display_path(result_path)}"

    parsed_stats = {
        "states_generated": int(counts[0].group(1).replace(",", "")),
        "distinct_states": int(counts[0].group(2).replace(",", "")),
        "search_depth": int(depths[0].group(1).replace(",", "")),
    }
    if not (
        parsed_stats["states_generated"] >= parsed_stats["distinct_states"] >= 1
        and parsed_stats["search_depth"] >= 1
    ):
        return None, f"TLC parsed statistics are outside valid ranges: {display_path(result_path)}"
    for key, parsed in parsed_stats.items():
        declared = declared_stats[key]
        if type(declared) is not int or declared != parsed:
            return None, f"TLC JSON {key} disagrees with verified log: {display_path(result_path)}"
    formal.update(parsed_stats)
    return formal, (
        "TLC result, no-error completion marker, final statistics, and "
        "referenced inputs SHA-256 verified"
    )


def load_direct_formal() -> tuple[dict[str, Any] | None, str]:
    return load_formal_result(FORMAL_RESULT_FILES[0][1])


def load_formal_results() -> list[tuple[str, dict[str, Any] | None, str]]:
    return [
        (label, *load_formal_result(path))
        for label, path in FORMAL_RESULT_FILES
    ]


def generate_formal(summary: dict[str, Any] | None, reason: str) -> None:
    del summary  # Formal evidence is verified independently of benchmark data.
    del reason
    rows = []
    for label, formal, formal_reason in load_formal_results():
        if not isinstance(formal, dict) or formal.get("status") not in {"passed", "failed"}:
            model_config = latex_escape(label)
            status = r"\notmeasured"
            checked_at = r"\notmeasured"
            states = r"\notmeasured"
            distinct = r"\notmeasured"
            depth = r"\notmeasured"
            evidence = latex_escape(formal_reason)
        else:
            model_config = latex_escape(f"{formal['model']} / {formal['config']}")
            status = latex_escape(formal["status"])
            checked_at = scalar(formal, "checked_at")
            states = scalar(formal, "states_generated")
            distinct = scalar(formal, "distinct_states")
            depth = scalar(formal, "search_depth")
            evidence = latex_escape(
                f"{formal['tool']} {formal['tool_version']}; "
                f"{formal.get('raw_log', 'verified TLC log')}; {formal_reason}"
            )
        rows.append(
            rf"\parbox[t]{{0.20\textwidth}}{{{model_config}}} & {status} & "
            rf"{checked_at} & {states} & {distinct} & {depth} & "
            rf"\parbox[t]{{0.30\textwidth}}{{{evidence}}} " + r"\\"
        )
    row_text = "\n".join(rows)
    write(
        "formal_status.tex",
        rf"""
\begin{{table*}}[t]
\centering
\caption{{Generated finite-model status.  A missing raw log is not a pass; each row is independently verified from its raw log.}}
\label{{tab:formal-status}}
\scriptsize
\begin{{tabular}}{{p{{0.20\textwidth}}lllllp{{0.30\textwidth}}}}
\toprule
Model/config & Outcome & Checked at & States & Distinct & Depth & Evidence \\
\midrule
{row_text}
\bottomrule
\end{{tabular}}
\end{{table*}}
""",
    )


def generate_security(summary: dict[str, Any] | None, reason: str) -> None:
    security = summary.get("security") if summary else None
    if summary is None:
        valid, security_reason = False, reason
    else:
        valid, security_reason = validate_security(summary, security)
    values = security if valid and isinstance(security, dict) else {}
    status = scalar(values, "status") if valid else r"\notmeasured"
    evidence = latex_escape(security_reason)
    corpus_count = (
        f"{scalar(values, 'corpus_passed')}/{scalar(values, 'cases')}"
        if valid else r"\notmeasured"
    )
    prompt_count = (
        f"{scalar(values, 'prompt_injection_passed')}/{scalar(values, 'prompt_injection_cases')}"
        if valid else r"\notmeasured"
    )
    rows = [
        ("SQL corpus passed/cases", corpus_count),
        ("Prompt boundaries passed/cases", prompt_count),
        ("Unauthorized connector crossings", scalar(values, "unauthorized_crossings") if valid else r"\notmeasured"),
        ("Budget invariant violations", scalar(values, "budget_violations") if valid else r"\notmeasured"),
        ("Panics", scalar(values, "panics") if valid else r"\notmeasured"),
        ("Aggregate fuzz CPU-hours", scalar(values, "fuzz_cpu_hours") if valid else r"\notmeasured"),
    ]
    row_text = "\n".join(f"{label} & {value} \\\\" for label, value in rows)
    write(
        "security_status.tex",
        rf"""
\begin{{table}}[t]
\centering
\caption{{Generated security-campaign status.  A partial harness-validation run is not an accepted full campaign.}}
\label{{tab:security-status}}
\footnotesize
\begin{{tabular}}{{lr}}
\toprule
Measure & Generated value \\
\midrule
Campaign outcome & {status} \\
{row_text}
\midrule
\multicolumn{{2}}{{>{{\raggedright\arraybackslash}}p{{0.86\columnwidth}}}}{{Evidence: {evidence}}} \\
\bottomrule
\end{{tabular}}
\end{{table}}
""",
    )


def nonnegative_integer(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def validate_security(
    summary: dict[str, Any] | None, security: Any
) -> tuple[bool, str]:
    """Validate paper-facing security fields and their raw-evidence linkage.

    The evaluation aggregator reconstructs these fields from its raw logs.  The
    paper generator independently requires every security evidence path/digest
    pair to occur in the already-rehashed top-level provenance.  This does not
    turn a partial campaign into a pass: nullable metrics remain visibly absent.
    """
    if not isinstance(summary, dict) or not isinstance(security, dict):
        return False, "source-backed security result absent"
    if security.get("status") in {"not_measured", "not_run"}:
        note = security.get("note")
        return False, note if isinstance(note, str) and note.strip() else "verified security campaign absent"
    if security.get("schema_version") != 1:
        return False, "security result schema invalid"
    outcome = security.get("status")
    if outcome not in {"passed", "failed", "partial"}:
        return False, "full or partial security campaign absent"
    accepted = security.get("security_acceptance_met")
    if not isinstance(accepted, bool):
        return False, "security acceptance flag absent"
    if accepted != (outcome == "passed"):
        return False, "security outcome and acceptance flag disagree"

    provenance = summary.get("provenance")
    raw_inputs = provenance.get("raw_inputs") if isinstance(provenance, dict) else None
    if not isinstance(raw_inputs, list):
        return False, "security provenance absent"
    top_pairs = {
        (item.get("path"), item.get("sha256"))
        for item in raw_inputs
        if isinstance(item, dict)
    }
    if not any(path == "evaluation/security/results.json" for path, _ in top_pairs):
        return False, "reconstructed security result is not checksummed provenance"

    security_evidence = security.get("evidence")
    if not isinstance(security_evidence, list) or not security_evidence:
        return False, "security evidence manifest absent"
    evidence_pairs: set[tuple[Any, Any]] = set()
    for item in security_evidence:
        if not isinstance(item, dict):
            return False, "security evidence manifest malformed"
        pair = (item.get("path"), item.get("sha256"))
        if pair not in top_pairs:
            return False, "security evidence omitted from verified provenance"
        evidence_pairs.add(pair)

    evidence_by_path = {path: digest for path, digest in evidence_pairs}
    for key in ("raw_log", "attack_manifest", "fuzz_campaign"):
        path = security.get(key)
        if not isinstance(path, str) or path not in evidence_by_path:
            return False, f"security {key} is not verified evidence"

    for key in ("cases", "corpus_passed", "prompt_injection_cases", "prompt_injection_passed", "panics"):
        if not nonnegative_integer(security.get(key)):
            return False, f"security {key} is invalid"
    if security["corpus_passed"] > security["cases"]:
        return False, "passed corpus count exceeds corpus cases"
    if security["prompt_injection_passed"] > security["prompt_injection_cases"]:
        return False, "passed prompt-injection count exceeds defined cases"

    components = security.get("component_status")
    if (
        not isinstance(components, dict)
        or not isinstance(components.get("attack_corpus"), str)
        or not components["attack_corpus"].strip()
        or not isinstance(components.get("fuzz"), str)
        or not components["fuzz"].strip()
    ):
        return False, "security component status invalid"

    requested_hours = security.get("requested_fuzz_cpu_hours")
    actual_seconds = security.get("actual_fuzz_cpu_seconds")
    reported_hours = security.get("fuzz_cpu_hours")
    cpu_met = security.get("fuzz_cpu_requirement_met")
    if not is_finite_number(requested_hours) or requested_hours <= 0:
        return False, "requested fuzz CPU-hours invalid"
    if not is_finite_number(actual_seconds) or actual_seconds < 0:
        return False, "actual fuzz CPU-seconds invalid"
    if not is_finite_number(reported_hours) or reported_hours < 0:
        return False, "reported fuzz CPU-hours invalid"
    if not math.isclose(reported_hours, actual_seconds / 3600.0, rel_tol=1e-9, abs_tol=1e-6):
        return False, "fuzz CPU-hour total disagrees with raw-second total"
    publication_cpu_met = requested_hours >= 24 and actual_seconds >= 24 * 3600.0
    if not isinstance(cpu_met, bool) or cpu_met != publication_cpu_met:
        return False, "24-CPU-hour publication flag disagrees with actual CPU time"

    targets = security.get("fuzz_targets")
    if not isinstance(targets, list) or len(targets) != len(FUZZ_TARGETS):
        return False, "fuzz target evidence incomplete"
    seen_targets: set[str] = set()
    target_seconds = 0.0
    for target in targets:
        if not isinstance(target, dict) or target.get("name") not in FUZZ_TARGETS:
            return False, "unexpected fuzz target evidence"
        name = target["name"]
        if name in seen_targets:
            return False, "duplicate fuzz target evidence"
        seen_targets.add(name)
        seconds = target.get("actual_cpu_seconds")
        log_path = target.get("raw_log")
        log_digest = target.get("raw_log_sha256")
        if not is_finite_number(seconds) or seconds < 0:
            return False, "fuzz target CPU time invalid"
        if (log_path, log_digest) not in evidence_pairs:
            return False, "fuzz target log omitted from verified evidence"
        target_seconds += float(seconds)
    if seen_targets != FUZZ_TARGETS:
        return False, "fuzz target set incomplete"
    if not math.isclose(target_seconds, float(actual_seconds), rel_tol=1e-9, abs_tol=5e-6):
        return False, "aggregate fuzz CPU time disagrees with per-target logs"

    nullable_counts = (security.get("unauthorized_crossings"), security.get("budget_violations"))
    if any(value is not None and not nonnegative_integer(value) for value in nullable_counts):
        return False, "nullable security violation count invalid"
    if outcome == "passed" and (
        security["corpus_passed"] != security["cases"]
        or security["prompt_injection_passed"] != security["prompt_injection_cases"]
        or security["unauthorized_crossings"] != 0
        or security["budget_violations"] != 0
        or security["panics"] != 0
        or not cpu_met
    ):
        return False, "security pass does not satisfy the declared acceptance criteria"

    note = (
        f"{outcome}; {len(evidence_pairs)} checksummed security inputs; "
        "deterministically reconstructed from evaluation/security/results.json"
    )
    return True, note


def generate_performance(summary: dict[str, Any] | None, reason: str) -> None:
    performance = summary.get("performance") if summary else None
    records = performance.get("summary") if isinstance(performance, dict) else None
    performance_reason = reason
    if isinstance(performance, dict):
        note = performance.get("note")
        if isinstance(note, str) and note.strip():
            performance_reason = note.strip()
    valid_records: list[dict[str, Any]] = []
    if isinstance(performance, dict) and performance.get("status") == "complete" and isinstance(records, list):
        required_text = {"baseline", "query_id"}
        required_numbers = {
            "concurrency", "measured_runs", "throughput_qps", "p50_ms",
            "p50_bootstrap_ci_low_ms", "p50_bootstrap_ci_high_ms", "p95_ms",
            "p50_ratio_vs_direct",
        }
        for record in records:
            if not isinstance(record, dict):
                continue
            label = record.get("experiment", record.get("workload"))
            text_ok = all(isinstance(record.get(key), str) and record[key].strip() for key in required_text)
            number_ok = all(
                is_finite_number(record.get(key))
                and record[key] >= 0
                for key in required_numbers
            )
            if (
                isinstance(label, str) and label.strip() and text_ok and number_ok
                and record["baseline"] in PERFORMANCE_BASELINES
                and isinstance(record["concurrency"], int)
                and not isinstance(record["concurrency"], bool)
                and record["concurrency"] in {1, 8, 32}
                and isinstance(record.get("scale_factor"), int)
                and not isinstance(record.get("scale_factor"), bool)
                and record.get("scale_factor") in {1, 10}
                and isinstance(record["measured_runs"], int)
                and not isinstance(record["measured_runs"], bool)
                and record["measured_runs"] >= 30
                and record["throughput_qps"] > 0
                and record["p50_ratio_vs_direct"] > 0
                and record["p50_bootstrap_ci_low_ms"] <= record["p50_bootstrap_ci_high_ms"]
                and record["p50_ms"] <= record["p95_ms"]
            ):
                p99 = record.get("p99_ms")
                p99_ok = (
                    is_finite_number(p99)
                    and record["p95_ms"] <= p99
                ) or (
                    p99 is None
                    and record.get("p99_reportable") is False
                    and record["measured_runs"] < 10000
                )
                if p99_ok:
                    valid_records.append(record)

    if not valid_records:
        body = rf"""
\begin{{table*}}[t]
\centering
\caption{{Generated performance status.  No complete source-backed record set is present; no latency or throughput conclusion is drawn.}}
\label{{tab:performance-status}}
\scriptsize
\begin{{tabular}}{{llllllllll}}
\toprule
Experiment & Query & SF & C & Baseline & Runs & Throughput & p50 [95\% CI] & p95/p99 & p50/direct \\
\midrule
\notmeasured & \notmeasured & \notmeasured & \notmeasured & \notmeasured & \notmeasured & \notmeasured & \notmeasured & \notmeasured & \notmeasured \\
\bottomrule
\end{{tabular}}
\\[1mm]\scriptsize Generated reason: {latex_escape(performance_reason)}.
\end{{table*}}
"""
    else:
        rows = []
        for record in valid_records:
            p50_ci = "{} [{}, {}]".format(
                scalar(record, "p50_ms"),
                scalar(record, "p50_bootstrap_ci_low_ms"),
                scalar(record, "p50_bootstrap_ci_high_ms"),
            )
            rows.append(
                "{} & {} & {} & {} & {} & {} & {} & {} & {}/{} & {} \\\\".format(
                    latex_escape(record.get("experiment", record.get("workload"))),
                    latex_escape(record.get("query_id", "")),
                    scalar(record, "scale_factor"),
                    scalar(record, "concurrency"),
                    latex_escape(PERFORMANCE_BASELINES[record["baseline"]]),
                    scalar(record, "measured_runs"),
                    scalar(record, "throughput_qps"),
                    p50_ci,
                    scalar(record, "p95_ms"),
                    scalar(record, "p99_ms"),
                    scalar(record, "p50_ratio_vs_direct"),
                )
            )
        body = r"""
\begin{table*}[t]
\centering
\caption{Generated performance measurements.  Every record is linked by the manifest to checksummed raw input.}
\label{tab:performance-status}
\scriptsize
\begin{tabular}{llllllllll}
\toprule
Experiment & Query & SF & C & Baseline & Runs & Throughput (q/s) & p50 [95\% CI] & p95/p99 (ms) & p50/direct \\
\midrule
""" + "\n".join(rows) + r"""
\bottomrule
\end{tabular}
\end{table*}
"""
    write("performance_status.tex", body)


def generate_approval_count() -> None:
    write(
        "approval_count.tex",
        r"""
\begin{table}[t]
\centering
\caption{Analytical approval-event count; these are formulas, not participant measurements.}
\label{tab:approval-count}
\footnotesize
\begin{tabular}{p{0.20\columnwidth}p{0.26\columnwidth}p{0.38\columnwidth}}
\toprule
Policy & Human approvals & Assumption \\
\midrule
Per query & $\sum_{t=1}^{T} n_t$ & one decision per query \\
TaskGate manual & $T$ & one accepted decision per task \\
Difference & $\sum_{t=1}^{T}(n_t-1)$ & same task partition \\
\bottomrule
\end{tabular}
\end{table}
""",
    )


def make_targets() -> set[str]:
    makefile = ROOT / "Makefile"
    if not makefile.is_file():
        return set()
    targets: set[str] = set()
    pattern = re.compile(r"^([A-Za-z0-9_.-]+)\s*:(?!=)")
    for line in makefile.read_text(encoding="utf-8").splitlines():
        if line.startswith((" ", "\t", ".")):
            continue
        match = pattern.match(line)
        if match:
            targets.add(match.group(1))
    return targets


def generate_artifact_status(summary: dict[str, Any] | None, reason: str) -> None:
    targets = make_targets()
    observed = summary.get("commands") if summary else None
    formal_results = load_formal_results()
    rows = []
    for command in ("verify", "formal", "eval-smoke", "eval-full", "paper"):
        present = "yes" if command in targets else "no"
        outcome = r"\notmeasured"
        evidence = "no source-backed outcome"
        if isinstance(observed, dict) and isinstance(observed.get(command), dict):
            record = observed[command]
            if record.get("status") in {"passed", "failed"} and record.get("raw_log"):
                outcome = latex_escape(record["status"])
                evidence = latex_escape(record["raw_log"])
        elif command == "formal":
            verified = [(label, formal) for label, formal, _ in formal_results if isinstance(formal, dict)]
            if verified:
                statuses = {str(formal["status"]) for _, formal in verified}
                if len(statuses) == 1:
                    outcome = latex_escape(f"{next(iter(statuses))} ({len(verified)} models)")
                else:
                    outcome = latex_escape(f"mixed ({len(verified)} models)")
                evidence = latex_escape(", ".join(str(formal["raw_log"]) for _, formal in verified))
            else:
                evidence = latex_escape("; ".join(f"{label}: {formal_reason}" for label, _, formal_reason in formal_results))
        rows.append(rf"\code{{make {command}}} & {present} & {outcome} & {evidence} " + r"\\")
    write(
        "artifact_status.tex",
        r"""
\begin{table*}[t]
\centering
\caption{Generated artifact-interface status. ``Present'' means a Make target was found, not that it passed.}
\label{tab:artifact-status}
\footnotesize
\begin{tabular}{llll}
\toprule
Command & Present & Recorded outcome & Source-backed evidence \\
\midrule
""" + "\n".join(rows) + rf"""
\bottomrule
\end{{tabular}}
\\[1mm]\scriptsize Summary state: {latex_escape(reason)}.
\end{{table*}}
""",
    )


def main() -> None:
    summary, reason = load_summary()
    generate_formal(summary, reason)
    generate_security(summary, reason)
    generate_performance(summary, reason)
    generate_approval_count()
    generate_artifact_status(summary, reason)


if __name__ == "__main__":
    main()
