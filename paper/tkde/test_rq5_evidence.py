#!/usr/bin/env python3
"""Focused mutation tests for the combined RQ5 paper validator."""

from __future__ import annotations

import copy
import hashlib
import json
import pathlib
import shutil
import tempfile
import unittest

from paper.tkde import rq5_evidence as evidence


ROOT = pathlib.Path(__file__).resolve().parents[2]
OFFLINE_PACK = ROOT / "evaluation/daily-publication/evidence/scale-20260730-final3"


def _digest(label: str) -> str:
    return hashlib.sha256(label.encode("utf-8")).hexdigest()


def _write_json(path: pathlib.Path, value: object) -> bytes:
    path.parent.mkdir(parents=True, exist_ok=True)
    raw = (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    path.write_bytes(raw)
    return raw


def _source_bindings() -> dict[str, str]:
    source = json.loads((OFFLINE_PACK / "source-manifest.json").read_text(encoding="utf-8"))
    members = source["run_bound_members"]
    return {
        "generator_sha256": members[
            "evaluation/daily-publication/sql/05-generate-daily-data.sh"
        ]["sha256"],
        "config_sha256": members["evaluation/daily-publication/config.json"]["sha256"],
        "dataset_manifest_sha256": hashlib.sha256(
            (OFFLINE_PACK / "dataset-manifest.json").read_bytes()
        ).hexdigest(),
    }


def _bundle(day: str, publication: dict[str, object]) -> dict[str, object]:
    segments: list[dict[str, object]] = []
    for segment_id in evidence.EXPECTED_SEGMENTS:
        segment: dict[str, object] = {
            "id": segment_id,
            "kind": "base-row" if segment_id == "row" else "base-cell",
            "shard": 0,
            "fact_count": evidence.ROWS,
            "hashes_digest": _digest(f"{day}:{segment_id}:hashes"),
            "payloads_digest": _digest(f"{day}:{segment_id}:payloads"),
        }
        if segment_id != "row":
            segment["field"] = segment_id.removeprefix("cell:")
        segments.append(segment)
    name = publication["publication_name"]
    return {
        "version": "taskgate-snapshot-index-bundle-v1",
        "publication_name": name,
        "catalog_source": "daily_reporting",
        "ordinal_sidecar": f"taskgate_ordinal.daily_lineitem_{day}_r{evidence.ROWS}",
        "manifest_digest": publication["publication_manifest_digest"],
        "dictionary_manifest": {
            "version": "taskgate-ordinal-dictionary-v1",
            "source_id": "taskgate-eval-daily-publication",
            "source_namespace": "evaluation.daily_lineitem",
            "snapshot": f"rq5-daily-lineitem-{day}-rows-{evidence.ROWS}",
            "schema_digest": publication["schema_digest"],
            "dictionary_digest": publication["dictionary_digest"],
            "sidecar_digest": publication["sidecar_digest"],
            "cold_payload_digest": _digest(f"{day}:cold-payload"),
            "hot_index_digest": _digest(f"{day}:hot-index"),
            "segments": segments,
        },
        "row_count": evidence.ROWS,
        "hot": {
            "name": name + ".hot.tgord",
            "sha256": publication["hot_artifact_sha256"],
            "bytes": 101,
        },
        "cold": {
            "name": name + ".cold.tgord",
            "sha256": publication["cold_artifact_sha256"],
            "bytes": 202,
        },
        "sidecar": {
            "name": name + ".sidecar.ndjson",
            "sha256": publication["sidecar_artifact_sha256"],
            "bytes": 303,
        },
    }


def _approved(day: str, publication: dict[str, object], bundle: dict[str, object]) -> dict[str, object]:
    dictionary = bundle["dictionary_manifest"]
    assert isinstance(dictionary, dict)
    return {
        "version": "taskgate-snapshot-index-input-v1",
        "publication_name": publication["publication_name"],
        "catalog_source": "daily_reporting",
        "source_relation": f"reporting.daily_lineitem_{day}",
        "ordinal_sidecar": f"taskgate_ordinal.daily_lineitem_{day}_r{evidence.ROWS}",
        "entity_key_fields": ["l_orderkey", "l_linenumber"],
        "snapshot": {
            "source_id": "taskgate-eval-daily-publication",
            "source_namespace": "evaluation.daily_lineitem",
            "snapshot": f"rq5-daily-lineitem-{day}-rows-{evidence.ROWS}",
            "schema_digest": publication["schema_digest"],
            "fields": copy.deepcopy(evidence.EXPECTED_FIELDS),
            "rows": [],
        },
        "expected_digests": {
            "sidecar_digest": dictionary["sidecar_digest"],
            "dictionary_digest": dictionary["dictionary_digest"],
            "manifest_digest": bundle["manifest_digest"],
            "cold_payload_digest": dictionary["cold_payload_digest"],
            "hot_index_digest": dictionary["hot_index_digest"],
        },
    }


def _make_online_pack(root: pathlib.Path) -> pathlib.Path:
    bindings = _source_bindings()
    root.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(OFFLINE_PACK / "dataset-manifest.json", root / "dataset-manifest.json")
    publications: list[dict[str, object]] = []
    bundles: list[dict[str, object]] = []
    for index, day in enumerate(evidence.DAYS):
        name = f"daily-lineitem-{day}-r{evidence.ROWS}"
        publication: dict[str, object] = {
            "day": day,
            "publication_name": name,
            "row_count": evidence.ROWS,
            "approved_input_sha256": "",
            "catalog_sha256": "",
            "bundle_manifest_sha256": "",
            "publication_manifest_digest": _digest(f"online:{day}:publication"),
            "dictionary_digest": _digest(f"online:{day}:dictionary"),
            "sidecar_digest": _digest(f"online:{day}:semantic-sidecar"),
            "schema_digest": _digest(f"online:{day}:live-schema"),
            "hot_artifact_sha256": _digest(f"online:{day}:hot-file"),
            "cold_artifact_sha256": _digest(f"online:{day}:cold-file"),
            "sidecar_artifact_sha256": _digest(f"online:{day}:sidecar-file"),
            "direct_result_sha256": _digest(f"online:{day}:direct-result"),
        }
        bundle = _bundle(day, publication)
        bundle_raw = _write_json(root / "artifacts" / name / f"{name}.bundle.json", bundle)
        publication["bundle_manifest_sha256"] = hashlib.sha256(bundle_raw).hexdigest()
        approved_raw = _write_json(
            root / "approved-inputs" / f"{day}.json", _approved(day, publication, bundle)
        )
        publication["approved_input_sha256"] = hashlib.sha256(approved_raw).hexdigest()
        catalog_raw = f"catalog_version: test-{day}\npublication: {name}\n".encode("utf-8")
        catalog_path = root / "catalogs" / f"{day}.yaml"
        catalog_path.parent.mkdir(parents=True, exist_ok=True)
        catalog_path.write_bytes(catalog_raw)
        publication["catalog_sha256"] = hashlib.sha256(catalog_raw).hexdigest()
        publications.append(publication)
        bundles.append(bundle)

    preparation = {
        "schema_version": evidence.PREPARATION_SCHEMA,
        "publications": [
            {
                "day": day,
                "publication_name": publication["publication_name"],
                "rows": evidence.ROWS,
                "schema_digest": publication["schema_digest"],
                "manifest_digest": publication["publication_manifest_digest"],
                "dictionary_digest": publication["dictionary_digest"],
                "sidecar_digest": publication["sidecar_digest"],
                "input_sha256": publication["approved_input_sha256"],
            }
            for day, publication in zip(evidence.DAYS, publications, strict=True)
        ],
    }
    _write_json(root / "preparation.json", preparation)

    cache_keys = [_digest(f"cache:{day}") for day in evidence.DAYS]
    transitions: list[dict[str, object]] = []
    timing = ((1.0, 10.0, 4.0), (2.0, 20.0, 5.0), (100.0, 30.0, 6.0))
    for index, (switch, first, replay) in enumerate(timing):
        old = publications[index]
        new = publications[index + 1]
        task = "task_" + _digest(f"task:{index}")[:32]
        ledger = _digest(f"ledger:{index}")
        transitions.append({
            "from": evidence.DAYS[index],
            "to": evidence.DAYS[index + 1],
            "switch_wall_ms": switch,
            "first_query_wall_ms": first,
            "replay_wall_ms": replay,
            "old_task": {
                "publication_digest_before": old["publication_manifest_digest"],
                "publication_digest_after": old["publication_manifest_digest"],
                "expected_publication_digest": old["publication_manifest_digest"],
                "result_sha256_before": old["direct_result_sha256"],
                "result_sha256_after": old["direct_result_sha256"],
                "expected_result_sha256": old["direct_result_sha256"],
            },
            "new_task": {
                "publication_digest": new["publication_manifest_digest"],
                "expected_publication_digest": new["publication_manifest_digest"],
                "result_sha256": new["direct_result_sha256"],
                "expected_result_sha256": new["direct_result_sha256"],
            },
            "old_ledger": {
                "before_switch_sha256": ledger,
                "after_switch_sha256": ledger,
            },
            "cache": {
                "old_cache_key_sha256": cache_keys[index],
                "first_new_cache_key_sha256": cache_keys[index + 1],
                "first_new_semantic_replay": False,
                "replay_new_cache_key_sha256": cache_keys[index + 1],
                "replay_new_semantic_replay": True,
            },
            "delegation": {
                "root_task_id": task,
                "child_root_task_id": task,
                "child_parent_task_id": task,
                "root_publication_digest": new["publication_manifest_digest"],
                "child_publication_digest": new["publication_manifest_digest"],
            },
        })
    online = {
        "schema_version": evidence.ONLINE_SCHEMA,
        "routing_model": evidence.ROUTING_MODEL,
        "rows_per_publication": evidence.ROWS,
        "measurement_boundary": evidence.ONLINE_MEASUREMENT_BOUNDARY,
        "fixture": {
            "fixture_class": "correctness_fixture",
            "rows_per_publication": evidence.ROWS,
            **bindings,
            "publications": publications,
        },
        "transitions": transitions,
    }
    path = root / "online-evidence.json"
    _write_json(path, online)
    return path


def _mutate(path: pathlib.Path, mutation) -> None:
    value = json.loads(path.read_text(encoding="utf-8"))
    mutation(value)
    _write_json(path, value)


class CombinedRQ5EvidenceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name) / "online"
        self.online = _make_online_pack(self.root)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_valid_combined_evidence_returns_canonical_metrics(self) -> None:
        result = evidence.validate_rq5(OFFLINE_PACK, self.online)
        self.assertEqual(result["status"], "complete")
        self.assertEqual(
            result["dataset_relation"]["classification"],
            "same_dataset_distinct_attested_artifacts",
        )
        self.assertEqual(result["offline"]["rows_per_publication"], 345_000)
        self.assertEqual(result["offline"]["days"]["day1"]["changes"]["updated_rows"], 3_450)
        self.assertEqual(result["offline"]["metrics"]["maximum_cycle_ms"], 77_055.232232)
        self.assertEqual(result["online"]["transition_count"], 3)
        self.assertTrue(result["online"]["all_five_conditions_pass"])
        self.assertEqual(set(result["online"]["conditions"]), set(evidence.CONDITIONS))
        self.assertTrue(all(value["pass_count"] == 3
                            for value in result["online"]["conditions"].values()))
        self.assertEqual(result["online"]["timing_ms"]["switch"]["p50"], 2.0)
        self.assertAlmostEqual(result["online"]["timing_ms"]["switch"]["p95"], 90.2)
        self.assertEqual(
            result["online"]["timing_ms"]["switch"]["quantile_method"],
            "Hyndman-Fan Type 7",
        )
        comparison = result["online"]["artifact_identity_comparison"]
        self.assertIn("schema_digest", comparison["day0"]["differing_fields"])

    def test_two_thousand_row_smoke_cannot_satisfy_paper_validator(self) -> None:
        _mutate(self.online, lambda value: value.__setitem__("rows_per_publication", 2_000))
        with self.assertRaisesRegex(evidence.EvidenceError, "must equal 345000"):
            evidence.validate_rq5(OFFLINE_PACK, self.online)

    def test_cross_run_dataset_binding_is_exact(self) -> None:
        _mutate(
            self.online,
            lambda value: value["fixture"].__setitem__("config_sha256", _digest("other config")),
        )
        with self.assertRaisesRegex(evidence.EvidenceError, "config_sha256 differs"):
            evidence.validate_rq5(OFFLINE_PACK, self.online)

    def test_retained_dataset_manifest_is_rehashed(self) -> None:
        (self.root / "dataset-manifest.json").write_text("{}\n", encoding="utf-8")
        with self.assertRaisesRegex(evidence.EvidenceError, "dataset manifest SHA-256 differs"):
            evidence.validate_rq5(OFFLINE_PACK, self.online)

    def test_online_bundle_descriptor_is_bound_to_fixture(self) -> None:
        _mutate(
            self.online,
            lambda value: value["fixture"]["publications"][0].__setitem__(
                "hot_artifact_sha256", _digest("tampered HOT descriptor")
            ),
        )
        with self.assertRaisesRegex(evidence.EvidenceError, "hot descriptor SHA-256 differs"):
            evidence.validate_rq5(OFFLINE_PACK, self.online)

    def test_each_of_the_five_conditions_fails_closed(self) -> None:
        mutations = {
            "old_task_returns_old_data": lambda value: value["transitions"][0]["old_task"].__setitem__(
                "result_sha256_after", _digest("changed old result")
            ),
            "new_task_sees_new_data": lambda value: value["transitions"][0]["new_task"].__setitem__(
                "result_sha256", _digest("wrong new result")
            ),
            "old_task_ledger_unchanged_by_switch": lambda value: value["transitions"][0]["old_ledger"].__setitem__(
                "after_switch_sha256", _digest("changed old ledger")
            ),
            "new_publication_misses_old_cache": lambda value: value["transitions"][0]["cache"].__setitem__(
                "first_new_semantic_replay", True
            ),
            "delegated_child_uses_root_publication": lambda value: value["transitions"][0]["delegation"].__setitem__(
                "child_publication_digest", _digest("wrong child publication")
            ),
        }
        baseline = self.online.read_bytes()
        for condition, mutation in mutations.items():
            with self.subTest(condition=condition):
                self.online.write_bytes(baseline)
                _mutate(self.online, mutation)
                with self.assertRaisesRegex(evidence.EvidenceError, condition):
                    evidence.validate_rq5(OFFLINE_PACK, self.online)

    def test_extra_online_field_is_rejected(self) -> None:
        _mutate(self.online, lambda value: value.__setitem__("reported_status", "pass"))
        with self.assertRaisesRegex(evidence.EvidenceError, "online evidence fields differ"):
            evidence.validate_rq5(OFFLINE_PACK, self.online)

    def test_default_is_formal_scale_evidence_not_smoke(self) -> None:
        self.assertEqual(
            evidence.DEFAULT_ONLINE_EVIDENCE.relative_to(ROOT).as_posix(),
            "evaluation/daily-publication-online/evidence/scale-20260730-final/online-evidence.json",
        )
        self.assertNotIn("smoke", evidence.DEFAULT_ONLINE_EVIDENCE.as_posix())


if __name__ == "__main__":
    unittest.main()
