import json
import pathlib
import shutil
import sys
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import seal  # noqa: E402
import validate  # noqa: E402


class OnlineEvidencePackTest(unittest.TestCase):
    def test_checked_in_pack_and_all_online_conditions_validate(self) -> None:
        result = validate.validate_pack()
        self.assertEqual(result["status"], "complete")
        self.assertEqual(result["rows_per_publication"], 345_000)
        self.assertEqual(result["retained_file_count"], 15)
        self.assertEqual(result["pack_file_count"], 16)
        self.assertEqual(result["omitted_artifact_count"], 12)
        self.assertEqual(result["transition_count"], 3)
        self.assertTrue(result["all_five_conditions_pass"])

    def test_manifest_is_reproducible_across_output_directories(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            first = root / "first"
            second = root / "second"
            seal.seal(seal.DEFAULT_SOURCE, first)
            seal.seal(seal.DEFAULT_SOURCE, second)
            checked_in = (validate.DEFAULT_PACK / "pack-manifest.json").read_bytes()
            self.assertEqual(
                (first / "pack-manifest.json").read_bytes(),
                (second / "pack-manifest.json").read_bytes(),
            )
            self.assertEqual((first / "pack-manifest.json").read_bytes(), checked_in)

    def test_extra_file_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pack = pathlib.Path(directory) / "pack"
            shutil.copytree(validate.DEFAULT_PACK, pack)
            (pack / "unexpected.json").write_text("{}\n", encoding="utf-8")
            with self.assertRaisesRegex(validate.EvidenceError, "exact file set"):
                validate.validate_pack(pack)

    def test_retained_file_hash_drift_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pack = pathlib.Path(directory) / "pack"
            shutil.copytree(validate.DEFAULT_PACK, pack)
            catalog = pack / "catalogs/day2.yaml"
            catalog.write_bytes(catalog.read_bytes() + b"\n")
            with self.assertRaisesRegex(validate.EvidenceError, "byte count differs"):
                validate.validate_pack(pack)

    def test_resealed_semantic_failure_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pack = pathlib.Path(directory) / "pack"
            shutil.copytree(validate.DEFAULT_PACK, pack)
            evidence_path = pack / "online-evidence.json"
            evidence = json.loads(evidence_path.read_text(encoding="utf-8"))
            evidence["transitions"][0]["cache"]["first_new_semantic_replay"] = True
            evidence_path.write_bytes(validate.pretty_bytes(evidence))
            (pack / "pack-manifest.json").write_bytes(
                validate.pretty_bytes(seal.pack_manifest(pack))
            )
            with self.assertRaisesRegex(validate.EvidenceError, "RQ5 validation failed"):
                validate.validate_pack(pack)


if __name__ == "__main__":
    unittest.main()
