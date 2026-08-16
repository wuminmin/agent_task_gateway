"""Tests for the retained Final-V5 pilot evidence the manuscript cites."""

from __future__ import annotations

import hashlib
import json
import shutil
from pathlib import Path

import pytest

from final_v5_pilot_evidence import (
    MANIFEST_RELATIVE_PATH,
    PilotEvidenceError,
    validate_final_v5_pilot_evidence,
)

ROOT = Path(__file__).resolve().parent.parent.parent


def test_repository_pilot_evidence_supports_every_cited_number():
    stats = validate_final_v5_pilot_evidence(ROOT)
    artifact, baseline = stats["artifact"], stats["baseline"]
    # The artifact claim the manuscript makes is deployment independence: more
    # than one deployment, all of a single commit.
    assert artifact["deployments"] > 1
    assert len(artifact["commit"]) == 40  # a full Git commit SHA-1
    # samples_per_cell already spans the deployments: each frozen cell is
    # measured in all three, so nine samples per cell is three per deployment.
    assert artifact["samples"] == artifact["cells"] * artifact["samples_per_cell"]
    assert artifact["samples_per_cell"] % artifact["deployments"] == 0
    # Baseline covers S1 and S2 at both frozen scale factors in all five modes.
    assert baseline["cells"] == 20
    assert baseline["samples"] == baseline["cells"] * baseline["samples_per_cell"]
    assert baseline["replay_zero_sql_samples"] == baseline["cells"] // 5 * 3 * baseline["samples_per_cell"]
    # The overhead range the paper prints must bracket every measured cell.
    per_cell = [value["novel_over_direct"] for value in baseline["cell_medians"].values()]
    assert baseline["overhead_min"] == pytest.approx(min(per_cell))
    assert baseline["overhead_max"] == pytest.approx(max(per_cell))
    assert baseline["overhead_min"] > 1
    # The dependency-drives-cost claim: S2/SF10 releases far fewer Facts than
    # S1/SF10 yet depends on far more, and costs more.
    s_one, s_two = baseline["exposure"]["S1/SF10"], baseline["exposure"]["S2/SF10"]
    assert s_two["release"] < s_one["release"]
    assert s_two["dependency"] > s_one["dependency"]
    assert baseline["cell_medians"]["S2/SF10"]["novel_ms"] > baseline["cell_medians"]["S1/SF10"]["novel_ms"]
    # The replay claim: idempotent replay of the joined aggregate beats direct
    # re-execution of the same query.
    assert baseline["cell_medians"]["S2/SF10"]["idempotent_ms"] < baseline["cell_medians"]["S2/SF10"]["direct_ms"]


def _clone(tmp_path: Path) -> Path:
    """Copy just the manifest and the runs it names into a writable tree."""
    manifest = json.loads((ROOT / MANIFEST_RELATIVE_PATH).read_text())
    target = tmp_path / "repo"
    (target / Path(MANIFEST_RELATIVE_PATH).parent).mkdir(parents=True, exist_ok=True)
    for entry in manifest["runs"]:
        shutil.copytree(ROOT / entry["path"], target / entry["path"])
    (target / MANIFEST_RELATIVE_PATH).write_text(json.dumps(manifest))
    return target


def test_edited_samples_are_refused(tmp_path):
    target = _clone(tmp_path)
    manifest = json.loads((target / MANIFEST_RELATIVE_PATH).read_text())
    victim = target / manifest["runs"][-1]["path"] / "raw" / "deployment-01.jsonl"
    lines = victim.read_text().splitlines()
    sample = json.loads(lines[0])
    sample["client_full_drain_ms"] = 0.001
    lines[0] = json.dumps(sample)
    victim.write_text("\n".join(lines) + "\n")
    with pytest.raises(PilotEvidenceError, match="digest"):
        validate_final_v5_pilot_evidence(target)


def test_a_manifest_repointed_at_the_edited_bytes_is_still_refused(tmp_path):
    """Re-digesting edited samples must not launder them into the paper."""
    target = _clone(tmp_path)
    manifest = json.loads((target / MANIFEST_RELATIVE_PATH).read_text())
    entry = manifest["runs"][-1]
    victim = target / entry["path"] / "raw" / "deployment-01.jsonl"
    lines = victim.read_text().splitlines()
    sample = json.loads(lines[0])
    sample["status"] = "invalid"
    lines[0] = json.dumps(sample)
    payload = ("\n".join(lines) + "\n").encode()
    victim.write_bytes(payload)
    entry["samples_sha256"] = hashlib.sha256(payload).hexdigest()
    (target / MANIFEST_RELATIVE_PATH).write_text(json.dumps(manifest))
    with pytest.raises(PilotEvidenceError, match="non-passing"):
        validate_final_v5_pilot_evidence(target)


def test_publication_eligible_evidence_is_refused(tmp_path):
    target = _clone(tmp_path)
    manifest = json.loads((target / MANIFEST_RELATIVE_PATH).read_text())
    manifest["publication_eligible"] = True
    (target / MANIFEST_RELATIVE_PATH).write_text(json.dumps(manifest))
    with pytest.raises(PilotEvidenceError, match="publication-ineligible"):
        validate_final_v5_pilot_evidence(target)
