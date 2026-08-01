#!/usr/bin/env bash
set -uo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
log_path="$root_dir/evaluation/v5-outcome/raw/compose-e2e.log"
receipt_path="$root_dir/evaluation/v5-outcome/compose-receipt.json"
submission_commit=${TASKGATE_SUBMISSION_COMMIT:-$(git -C "$root_dir" rev-parse HEAD)}
measured_paths=(
  Dockerfile compose.yaml go.mod go.sum
  cmd internal config db
  scripts/compose-test.sh scripts/integration-test.sh
)

fail() {
  echo "compose evidence recording failed: $*" >&2
  exit 1
}

[[ "$submission_commit" =~ ^[0-9a-f]{40}$ ]] || fail "submission commit must be a full SHA"
git -C "$root_dir" cat-file -e "${submission_commit}^{commit}" 2>/dev/null || fail "submission commit does not exist"
git -C "$root_dir" diff --quiet "$submission_commit" -- "${measured_paths[@]}" || \
  fail "measured paths differ from submission commit"
if [[ -n $(git -C "$root_dir" status --porcelain --untracked-files=all -- "${measured_paths[@]}") ]]; then
  fail "measured paths contain uncommitted or untracked changes"
fi

mkdir -p "$(dirname -- "$log_path")"
image_tsv=$(mktemp /tmp/taskgate-compose-images.XXXXXX)
run_log=$(mktemp /tmp/taskgate-compose-e2e.XXXXXX)
receipt_tmp=$(mktemp /tmp/taskgate-compose-receipt.XXXXXX)
cleanup() {
  rm -f "$image_tsv" "$run_log" "$receipt_tmp"
}
trap cleanup EXIT

set +e
TASKGATE_COMPOSE_EVIDENCE_IMAGES="$image_tsv" \
  "$root_dir/scripts/integration-test.sh" 2>&1 | tee "$run_log"
exit_code=${PIPESTATUS[0]}
set -e
cp "$run_log" "$log_path"

python3 - "$root_dir" "$submission_commit" "$exit_code" "$image_tsv" "$log_path" "$receipt_tmp" <<'PY'
import datetime
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
submission_commit = sys.argv[2]
exit_code = int(sys.argv[3])
image_tsv = pathlib.Path(sys.argv[4])
log_path = pathlib.Path(sys.argv[5])
receipt_path = pathlib.Path(sys.argv[6])
raw = log_path.read_bytes()
text = raw.decode("utf-8", errors="replace")
markers = {
    "caller_predicate": "ok - caller SQL lowers through V5 atomization, Parquet publication, and zero-execution semantic replay",
    "parquet_available": "ok - approved query creates an AVAILABLE canonical Parquet; preview paginates and delivery streams a complete file",
    "semantic_replay": "ok - V5 semantic replay avoided Business PostgreSQL and repeated exposure charge",
    "promotion_recovery": "ok - canonical-copy/AVAILABLE-commit crash-window recovery passed",
}
images = []
if image_tsv.is_file():
    for line in image_tsv.read_text(encoding="utf-8").splitlines():
        service, reference, image_id = line.split("\t")
        images.append({"service": service, "reference": reference, "image_id": image_id})
images.sort(key=lambda item: item["service"])
receipt = {
    "schema_version": 1,
    "submission_commit": submission_commit,
    "executed_at": datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "command": ["./scripts/integration-test.sh"],
    "compose_images": images,
    "catalog_sha256": hashlib.sha256((root / "config/catalog.yaml").read_bytes()).hexdigest(),
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
python3 "$root_dir/paper/tkde/generate_evidence.py"
echo "ok - Compose E2E receipt: ${receipt_path#$root_dir/}"
