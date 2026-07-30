import copy
import hashlib
import json
import math
import pathlib
import shutil
import sys
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import seal  # noqa: E402
import validate  # noqa: E402


class EvidencePackTest(unittest.TestCase):
    def test_cycle_sum_is_stable_across_python_float_sum_versions(self) -> None:
        # These two measured triples are sensitive to the Python 3.12 change
        # in built-in sum(float): Python 3.11 rounds each naive result one ULP
        # away from the retained campaign value.
        self.assertEqual(
            validate._stable_float_sum(
                [67_725.490259, 4_057.322998, 1_033.026020]
            ),
            72_815.83927699999,
        )
        self.assertEqual(
            validate._stable_float_sum(
                [69_514.259179, 4_101.568208, 1_088.664165]
            ),
            74_704.491552,
        )

    def test_checked_in_pack_validates_and_recomputes_metrics(self) -> None:
        result = validate.validate_pack()
        self.assertEqual(result["offline_status"], "complete")
        self.assertEqual(
            result["rq5_overall_status"],
            "incomplete_without_online_transition_evidence",
        )
        self.assertEqual(result["workload"]["facts_per_publication"], 3_105_000)
        self.assertEqual(result["metrics"]["maximum_cycle_ms"], 77_055.232232)
        self.assertEqual(
            result["metrics"]["maximum_builder_peak_rss_bytes"], 5_550_759_936
        )
        self.assertFalse(
            result["metrics"]["prior_4_gib_builder_envelope_satisfied"]
        )

    def test_recomputation_does_not_trust_retained_canonical_summary(self) -> None:
        expected = validate.recompute_canonical()
        retained = validate.load_json(validate.DEFAULT_PACK / "canonical-offline.json")
        self.assertEqual(expected, retained)

    def test_manifest_rejects_an_unlisted_file(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pack = pathlib.Path(directory) / "pack"
            shutil.copytree(validate.DEFAULT_PACK, pack)
            (pack / "unlisted.json").write_text("{}\n", encoding="utf-8")
            with self.assertRaisesRegex(validate.EvidenceError, "exact file set"):
                validate.validate_pack(pack)

    def test_exact_bundle_determinism_rejects_semantically_equal_bytes(self) -> None:
        compact = b'{"a":1}\n'
        spaced = b'{"a": 1}\n'
        descriptor = {
            "hot": {"name": "x.hot", "bytes": 1, "sha256": "0" * 64},
            "cold": {"name": "x.cold", "bytes": 1, "sha256": "1" * 64},
            "sidecar": {"name": "x.sidecar", "bytes": 1, "sha256": "2" * 64},
        }
        items = [
            {
                "sha256": hashlib.sha256(compact).hexdigest(),
                "transport_descriptors": copy.deepcopy(descriptor),
            }
            for _ in range(4)
        ]
        items[-1]["sha256"] = hashlib.sha256(spaced).hexdigest()
        self.assertEqual(json.loads(compact), json.loads(spaced))
        with self.assertRaisesRegex(validate.EvidenceError, "byte-identical"):
            validate.require_exact_bundle_determinism(items, "day0")

    def test_resealed_but_changed_canonical_summary_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pack = pathlib.Path(directory) / "pack"
            shutil.copytree(validate.DEFAULT_PACK, pack)
            canonical_path = pack / "canonical-offline.json"
            canonical = validate.load_json(canonical_path)
            maximum_cycle = canonical["metrics"]["maximum_cycle_ms"]
            canonical["metrics"]["maximum_cycle_ms"] = math.nextafter(
                maximum_cycle, math.inf
            )
            canonical_path.write_bytes(validate.pretty_bytes(canonical))
            (pack / "pack-manifest.json").write_bytes(
                validate.pretty_bytes(seal.pack_manifest(pack))
            )
            with self.assertRaisesRegex(validate.EvidenceError, "differs from recomputation"):
                validate.validate_pack(pack)

    def test_resealed_but_incomplete_omission_inventory_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pack = pathlib.Path(directory) / "pack"
            shutil.copytree(validate.DEFAULT_PACK, pack)
            omission_path = pack / "transport-omissions.json"
            omission = validate.load_json(omission_path)
            omission["artifacts"].pop()
            omission["artifact_count"] -= 1
            omission_path.write_bytes(validate.pretty_bytes(omission))
            (pack / "pack-manifest.json").write_bytes(
                validate.pretty_bytes(seal.pack_manifest(pack))
            )
            with self.assertRaisesRegex(validate.EvidenceError, "omission inventory"):
                validate.validate_pack(pack)

    def test_nonfinite_json_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "bad.json"
            path.write_text('{"wall_ms":NaN}\n', encoding="utf-8")
            with self.assertRaisesRegex(validate.EvidenceError, "non-finite"):
                validate.load_json(path)


if __name__ == "__main__":
    unittest.main()
