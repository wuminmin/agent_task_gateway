#!/usr/bin/env python3
"""Low-cost tests for draft/final TKDE evidence validation modes."""

from __future__ import annotations

import copy
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


PAPER_DIR = Path(__file__).resolve().parent
REPOSITORY_ROOT = PAPER_DIR.parents[1]
for search_path in (PAPER_DIR, REPOSITORY_ROOT):
    if str(search_path) not in sys.path:
        sys.path.insert(0, str(search_path))

from paper.tkde import generate_evidence as evidence  # noqa: E402


def _git(root: Path, *arguments: str) -> str:
    completed = subprocess.run(
        ["git", *arguments], cwd=root, check=True, capture_output=True, text=True,
    )
    return completed.stdout.strip()


def _commit(root: Path, message: str) -> str:
    _git(root, "add", ".")
    _git(
        root, "-c", "user.name=Evidence Test", "-c", "user.email=evidence@example.invalid",
        "commit", "-q", "-m", message,
    )
    return _git(root, "rev-parse", "HEAD")


class EvidenceValidationModeTests(unittest.TestCase):
    def test_cli_defaults_to_draft_and_accepts_explicit_strict_modes(self) -> None:
        self.assertEqual(evidence.parse_args([]).evidence_mode, "draft")
        self.assertEqual(
            evidence.parse_args(["--evidence-mode", "strict"]).evidence_mode, "strict",
        )
        self.assertEqual(
            evidence.parse_args(["--evidence-mode", "final"]).evidence_mode, "final",
        )

    def test_checked_in_draft_accepts_head_evolution_without_skipping_evidence(self) -> None:
        with mock.patch.object(evidence, "validate_v5_measured_paths_frozen") as freeze:
            result = evidence.validate_v5_outcome_evidence()
        self.assertEqual(result["schema_version"], 3)
        freeze.assert_not_called()

    def test_schema3_draft_requires_lineage_but_not_a_source_freeze(self) -> None:
        checked_in = evidence.load_json(evidence.V5_OUTCOME)
        base_commit = checked_in["implementation_base_commit"]
        submission_commit = checked_in["submission_commit"]
        with (
            mock.patch.object(evidence, "validate_git_ancestry") as ancestry,
            mock.patch.object(evidence, "validate_v5_measured_paths_frozen") as freeze,
        ):
            evidence.validate_v5_outcome_evidence("draft")
        self.assertEqual(
            ancestry.call_args_list,
            [
                mock.call(
                    base_commit,
                    submission_commit,
                    "V5 implementation base commit to submission commit",
                ),
                mock.call(
                    submission_commit,
                    "HEAD",
                    "V5 submission commit to HEAD",
                ),
            ],
        )
        freeze.assert_not_called()

    def test_strict_and_final_modes_invoke_the_freeze_gate(self) -> None:
        checked_in = evidence.load_json(evidence.V5_OUTCOME)
        base_commit = checked_in["implementation_base_commit"]
        submission_commit = checked_in["submission_commit"]
        for mode in ("strict", "final"):
            with self.subTest(mode=mode):
                with (
                    mock.patch.object(evidence, "validate_v5_measured_paths_frozen") as freeze,
                    mock.patch.object(evidence, "validate_git_ancestry") as ancestry,
                ):
                    result = evidence.validate_v5_outcome_evidence(mode)
                self.assertEqual(result["schema_version"], 3)
                freeze.assert_called_once_with(submission_commit)
                self.assertEqual(
                    ancestry.call_args_list,
                    [
                        mock.call(
                            base_commit,
                            submission_commit,
                            "V5 implementation base commit to submission commit",
                        ),
                        mock.call(
                            submission_commit,
                            "HEAD",
                            "V5 submission commit to HEAD",
                        ),
                    ],
                )

    def test_schema2_draft_uses_base_to_head_and_strict_fails_closed(self) -> None:
        original_load = evidence.load_json
        changed = copy.deepcopy(original_load(evidence.V5_OUTCOME))
        changed["schema_version"] = 2
        changed.pop("submission_commit")
        changed.pop("compose_execution")
        changed["source_manifest"]["files"] = []
        changed["source_manifest"]["sha256"] = evidence.v5_source_manifest_digest([])

        def load(path: Path) -> dict:
            # A fresh copy per call: the validator annotates the document it
            # returns (family_property); a file loader never hands back a
            # previously annotated object.
            return copy.deepcopy(changed) if path == evidence.V5_OUTCOME else original_load(path)

        with (
            mock.patch.object(evidence, "load_json", side_effect=load),
            mock.patch.object(evidence, "V5_SOURCE_PATHS", ()),
            mock.patch.object(evidence, "validate_git_ancestry") as ancestry,
        ):
            result = evidence.validate_v5_outcome_evidence("draft")
        self.assertEqual(result["schema_version"], 2)
        ancestry.assert_called_once_with(
            changed["implementation_base_commit"],
            "HEAD",
            "V5 implementation base commit to HEAD",
        )

        with (
            mock.patch.object(evidence, "load_json", side_effect=load),
            mock.patch.object(evidence, "V5_SOURCE_PATHS", ()),
        ):
            for mode in ("strict", "final"):
                with self.subTest(mode=mode):
                    with self.assertRaisesRegex(ValueError, "requires schema version 3"):
                        evidence.validate_v5_outcome_evidence(mode)

    def test_draft_still_rejects_a_missing_submission_commit(self) -> None:
        original_load = evidence.load_json
        changed = copy.deepcopy(original_load(evidence.V5_OUTCOME))
        changed["submission_commit"] = "f" * 40

        def load(path: Path) -> dict:
            # A fresh copy per call: the validator annotates the document it
            # returns (family_property); a file loader never hands back a
            # previously annotated object.
            return copy.deepcopy(changed) if path == evidence.V5_OUTCOME else original_load(path)

        with mock.patch.object(evidence, "load_json", side_effect=load):
            with self.assertRaisesRegex(ValueError, "V5 submission commit"):
                evidence.validate_v5_outcome_evidence("draft")

    def test_draft_still_rejects_a_tampered_compose_receipt(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            tampered = Path(directory) / "compose-receipt.json"
            tampered.write_text("{}\n", encoding="utf-8")
            with mock.patch.object(evidence, "V5_COMPOSE_RECEIPT", tampered):
                with self.assertRaisesRegex(ValueError, "receipt binding is missing or stale"):
                    evidence.validate_v5_outcome_evidence("draft")

    def test_draft_still_rejects_a_tampered_source_hash(self) -> None:
        original_load = evidence.load_json
        changed = copy.deepcopy(original_load(evidence.V5_OUTCOME))
        changed["source_manifest"]["files"][0]["sha256"] = "0" * 64
        changed["source_manifest"]["sha256"] = evidence.v5_source_manifest_digest(
            changed["source_manifest"]["files"],
        )

        def load(path: Path) -> dict:
            # A fresh copy per call: the validator annotates the document it
            # returns (family_property); a file loader never hands back a
            # previously annotated object.
            return copy.deepcopy(changed) if path == evidence.V5_OUTCOME else original_load(path)

        with mock.patch.object(evidence, "load_json", side_effect=load):
            with self.assertRaisesRegex(ValueError, "source manifest binding is stale"):
                evidence.validate_v5_outcome_evidence("draft")

    def test_final_entry_points_request_final_validation(self) -> None:
        recorder = (evidence.ROOT / "scripts/record-compose-e2e.sh").read_text(encoding="utf-8")
        makefile = (evidence.ROOT / "Makefile").read_text(encoding="utf-8")
        compiler = (evidence.ROOT / "paper/tkde/compile.sh").read_text(encoding="utf-8")
        self.assertIn(
            'python3 "$root_dir/paper/tkde/generate_evidence.py" --evidence-mode final',
            recorder,
        )
        self.assertIn("set -euo pipefail", recorder)
        self.assertIn(
            'git -C "$root_dir" --no-replace-objects merge-base --is-ancestor '
            '"$submission_commit" HEAD',
            recorder,
        )
        self.assertIn('evidence.validate_v5_outcome_evidence("draft")', recorder)
        self.assertIn(
            "prepare V5 source-manifest and raw-execution evidence", recorder,
        )
        self.assertIn(
            "\tpython3 paper/tkde/generate_evidence.py --evidence-mode final\n",
            makefile,
        )
        self.assertIn("paper-final-check requires a clean worktree", makefile)
        self.assertIn("\t./paper/tkde/build-container.sh final\n", makefile)
        self.assertIn('python3 generate_evidence.py --evidence-mode "$evidence_mode"', compiler)


class HistoricalGitBindingTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        _git(self.root, "init", "-q")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_commit_file_hash_uses_the_historical_blob(self) -> None:
        source = self.root / "measured.txt"
        source.write_text("measured\n", encoding="utf-8")
        submission_commit = _commit(self.root, "measured")
        expected = hashlib.sha256(b"measured\n").hexdigest()
        source.write_text("draft evolution\n", encoding="utf-8")
        replacement_commit = _commit(self.root, "replacement tree")
        _git(self.root, "replace", submission_commit, replacement_commit)

        with mock.patch.object(evidence, "ROOT", self.root):
            self.assertEqual(
                evidence.git_commit_file_sha256(submission_commit, "measured.txt"), expected,
            )

    def test_tooling_manifest_is_checked_against_submission_not_head(self) -> None:
        paths = ("generator.py", "runner.sh")
        for path in paths:
            (self.root / path).write_text(f"historical {path}\n", encoding="utf-8")
        submission_commit = _commit(self.root, "tooling")
        files = [
            {
                "path": path,
                "sha256": hashlib.sha256(f"historical {path}\n".encode()).hexdigest(),
            }
            for path in paths
        ]
        tooling = {
            "algorithm": "sha256-canonical-json-v1",
            "files": files,
            "sha256": evidence.v5_source_manifest_digest(files),
        }
        (self.root / paths[0]).write_text("new draft generator\n", encoding="utf-8")

        with (
            mock.patch.object(evidence, "ROOT", self.root),
            mock.patch.object(evidence, "V5_EVIDENCE_TOOLING_PATHS", paths),
        ):
            evidence.validate_v5_evidence_tooling(tooling, submission_commit)
            changed = copy.deepcopy(tooling)
            changed["files"][0]["sha256"] = "0" * 64
            changed["sha256"] = evidence.v5_source_manifest_digest(changed["files"])
            with self.assertRaisesRegex(ValueError, "tooling manifest binding is stale"):
                evidence.validate_v5_evidence_tooling(changed, submission_commit)
            changed = copy.deepcopy(tooling)
            changed["sha256"] = "0" * 64
            with self.assertRaisesRegex(ValueError, "tooling manifest digest is stale"):
                evidence.validate_v5_evidence_tooling(changed, submission_commit)

    def test_freeze_gate_rejects_committed_or_untracked_measured_drift(self) -> None:
        measured = self.root / "measured"
        measured.mkdir()
        (measured / "source.go").write_text("frozen\n", encoding="utf-8")
        submission_commit = _commit(self.root, "frozen")
        (measured / "source.go").write_text("changed\n", encoding="utf-8")
        _commit(self.root, "head evolution")

        with (
            mock.patch.object(evidence, "ROOT", self.root),
            mock.patch.object(evidence, "V5_MEASURED_PATHS", ("measured",)),
        ):
            with self.assertRaisesRegex(ValueError, "differ from the frozen submission"):
                evidence.validate_v5_measured_paths_frozen(submission_commit)

        clean_submission = _git(self.root, "rev-parse", "HEAD")
        (measured / "untracked.go").write_text("untracked\n", encoding="utf-8")
        with (
            mock.patch.object(evidence, "ROOT", self.root),
            mock.patch.object(evidence, "V5_MEASURED_PATHS", ("measured",)),
        ):
            with self.assertRaisesRegex(ValueError, "differ from the frozen submission"):
                evidence.validate_v5_measured_paths_frozen(clean_submission)

    def test_freeze_gate_rejects_git_status_failure(self) -> None:
        diff = subprocess.CompletedProcess(args=[], returncode=0)
        status = subprocess.CompletedProcess(args=[], returncode=128, stdout="")
        with mock.patch.object(evidence.subprocess, "run", side_effect=[diff, status]):
            with self.assertRaisesRegex(ValueError, "differ from the frozen submission"):
                evidence.validate_v5_measured_paths_frozen("a" * 40)


if __name__ == "__main__":
    unittest.main()
