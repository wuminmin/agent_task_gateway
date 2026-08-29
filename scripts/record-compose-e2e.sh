#!/usr/bin/env bash
set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
log_path="$root_dir/evaluation/v5-outcome/raw/compose-e2e.log"
receipt_path="$root_dir/evaluation/v5-outcome/compose-receipt.json"
submission_commit=${TASKGATE_SUBMISSION_COMMIT:-$(git -C "$root_dir" --no-replace-objects rev-parse HEAD)}
measured_paths=(
  Dockerfile compose.yaml go.mod go.sum
  cmd internal config db
  scripts/compose-test.sh scripts/integration-test.sh scripts/record-compose-e2e.sh
  paper/tkde/generate_evidence.py
)

fail() {
  echo "compose evidence recording failed: $*" >&2
  exit 1
}

[[ "$submission_commit" =~ ^[0-9a-f]{40}$ ]] || fail "submission commit must be a full SHA"
git -C "$root_dir" --no-replace-objects cat-file -e "${submission_commit}^{commit}" 2>/dev/null || fail "submission commit does not exist"
git -C "$root_dir" --no-replace-objects merge-base --is-ancestor "$submission_commit" HEAD || \
  fail "submission commit is not an ancestor of HEAD"
git -C "$root_dir" --no-replace-objects diff --quiet "$submission_commit" -- "${measured_paths[@]}" || \
  fail "measured paths differ from submission commit"
if ! measured_status=$(git -C "$root_dir" --no-replace-objects status --porcelain --untracked-files=all -- "${measured_paths[@]}"); then
  fail "cannot inspect measured-path status"
fi
if [[ -n $measured_status ]]; then
  fail "measured paths contain uncommitted or untracked changes"
fi

# Fail before the costly Compose run unless the existing V5 source manifest and
# raw execution receipt already describe this frozen tree. This wrapper records
# Compose evidence; it deliberately does not manufacture or refresh those inputs.
if ! PYTHONPATH="$root_dir/paper/tkde" python3 - "$submission_commit" <<'PY'
import sys

import generate_evidence as evidence

requested_submission = sys.argv[1]
record = evidence.load_json(evidence.V5_OUTCOME)
schema_version = record.get("schema_version")
if schema_version == 2:
    evidence.validate_v5_outcome_evidence("draft")
elif schema_version == 3:
    if record.get("submission_commit") != requested_submission:
        raise ValueError("existing V5 evidence names a different submission commit")
    evidence.validate_v5_outcome_evidence("final")
else:
    raise ValueError("cannot record Compose evidence for an unknown V5 evidence schema")
PY
then
  fail "prepare V5 source-manifest and raw-execution evidence for the frozen submission before Compose recording"
fi

mkdir -p "$(dirname -- "$log_path")"
image_tsv=$(mktemp /tmp/taskgate-compose-images.XXXXXX)
runtime_tsv=$(mktemp /tmp/taskgate-compose-runtime.XXXXXX)
run_log=$(mktemp /tmp/taskgate-compose-e2e.XXXXXX)
receipt_tmp=$(mktemp /tmp/taskgate-compose-receipt.XXXXXX)
cleanup() {
  rm -f "$image_tsv" "$runtime_tsv" "$run_log" "$receipt_tmp"
}
trap cleanup EXIT

set +e
TASKGATE_COMPOSE_EVIDENCE_IMAGES="$image_tsv" \
TASKGATE_COMPOSE_EVIDENCE_RUNTIME="$runtime_tsv" \
  "$root_dir/scripts/integration-test.sh" 2>&1 | tee "$run_log"
exit_code=${PIPESTATUS[0]}
set -e
cp "$run_log" "$log_path"

python3 - "$root_dir" "$submission_commit" "$exit_code" "$image_tsv" "$runtime_tsv" "$log_path" "$receipt_tmp" <<'PY'
import datetime
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
submission_commit = sys.argv[2]
exit_code = int(sys.argv[3])
image_tsv = pathlib.Path(sys.argv[4])
runtime_tsv = pathlib.Path(sys.argv[5])
log_path = pathlib.Path(sys.argv[6])
receipt_path = pathlib.Path(sys.argv[7])
raw = log_path.read_bytes()
text = raw.decode("utf-8", errors="replace")
markers = {
    "caller_predicate": "ok - caller SQL lowers through V5 atomization, Parquet publication, and zero-execution semantic replay",
    "parquet_available": "ok - approved query creates an AVAILABLE canonical Parquet; preview paginates and delivery streams a complete file",
    "semantic_replay": "ok - V5 semantic replay avoided Business PostgreSQL and repeated exposure charge",
    "promotion_recovery": "ok - canonical-copy/AVAILABLE-commit crash-window recovery passed",
    "go_test_skips_declared": "ok - complete PostgreSQL-backed unit and race tests accepted: every skip declared with a due milestone",
}
images = []
if image_tsv.is_file():
    for line in image_tsv.read_text(encoding="utf-8").splitlines():
        service, reference, image_id = line.split("\t")
        images.append({"service": service, "reference": reference, "image_id": image_id})
images.sort(key=lambda item: item["service"])
runtime = {}
if runtime_tsv.is_file():
    for line in runtime_tsv.read_text(encoding="utf-8").splitlines():
        key, value = line.split("\t")
        runtime[key] = value
tooling_paths = [
    "paper/tkde/generate_evidence.py",
    "scripts/integration-test.sh",
    "scripts/record-compose-e2e.sh",
]
tooling_files = [
    {"path": path, "sha256": hashlib.sha256((root / path).read_bytes()).hexdigest()}
    for path in tooling_paths
]
tooling_payload = json.dumps(tooling_files, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
receipt = {
    "schema_version": 2,
    "submission_commit": submission_commit,
    "executed_at": datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "command": ["./scripts/integration-test.sh"],
    "compose_images": images,
    "catalog_file_sha256": hashlib.sha256((root / "config/catalog.yaml").read_bytes()).hexdigest(),
    "catalog_runtime_digest": runtime.get("catalog_runtime_digest", ""),
    "evidence_tooling": {
        "algorithm": "sha256-canonical-json-v1",
        "files": tooling_files,
        "sha256": hashlib.sha256(tooling_payload).hexdigest(),
    },
    "exit_code": exit_code,
    "assertions": {name: marker in text for name, marker in markers.items()},
    "raw_log": "evaluation/v5-outcome/raw/compose-e2e.log",
    "raw_log_sha256": hashlib.sha256(raw).hexdigest(),
}
receipt_path.write_text(json.dumps(receipt, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
mv "$receipt_tmp" "$receipt_path"

if [[ "$exit_code" -ne 0 ]]; then
  fail "Compose E2E exited with $exit_code; failure receipt was retained"
fi

python3 - "$root_dir/evaluation/v5-outcome/evidence.json" "$receipt_path" "$submission_commit" <<'PY'
import hashlib
import json
import pathlib
import sys

evidence_path = pathlib.Path(sys.argv[1])
receipt_path = pathlib.Path(sys.argv[2])
submission_commit = sys.argv[3]
evidence = json.loads(evidence_path.read_text(encoding="utf-8"))
if evidence.get("schema_version") not in {2, 3}:
    raise SystemExit("cannot promote an unknown V5 evidence schema")
evidence["schema_version"] = 3
evidence["submission_commit"] = submission_commit
evidence["compose_execution"] = {
    "receipt": "evaluation/v5-outcome/compose-receipt.json",
    "receipt_sha256": hashlib.sha256(receipt_path.read_bytes()).hexdigest(),
}
evidence_path.write_text(json.dumps(evidence, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
python3 "$root_dir/paper/tkde/generate_evidence.py" --evidence-mode final
echo "ok - Compose E2E receipt: ${receipt_path#$root_dir/}"
