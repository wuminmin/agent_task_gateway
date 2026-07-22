#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <tpch|tpcds> <1|10> <generated-data-dir> <output-manifest>" >&2
  exit 2
fi

family=$1
scale=$2
data_dir=$3
output=$4

case "$family" in tpch|tpcds) ;; *) echo "unsupported family: $family" >&2; exit 2 ;; esac
case "$scale" in 1|10) ;; *) echo "scale factor must be 1 or 10" >&2; exit 2 ;; esac
[ -d "$data_dir" ] || { echo "generated data directory does not exist: $data_dir" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }

tmp=$(mktemp /tmp/taskgate-dataset-manifest.XXXXXX)
cleanup() {
  case "$tmp" in /tmp/taskgate-dataset-manifest.*) rm -f "$tmp" ;; esac
}
trap cleanup EXIT INT TERM

(
  cd "$data_dir"
  find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do
    sha256sum "$file"
  done
) >"$tmp"

[ -s "$tmp" ] || { echo "no generated data files found in $data_dir" >&2; exit 1; }
mkdir -p "$(dirname -- "$output")"
{
  echo "schema_version=1"
  echo "family=$family"
  echo "scale_factor=$scale"
  echo "generator=${TPC_GENERATOR_NAME:-unrecorded}"
  echo "generator_version=${TPC_GENERATOR_VERSION:-unrecorded}"
  echo "generator_seed=${TPC_GENERATOR_SEED:-unrecorded}"
  echo "generated_at_utc=${SOURCE_DATE_EPOCH:-unrecorded}"
  echo "files_begin"
  sed 's#  \./#  #' "$tmp"
  echo "files_end"
} >"$output"

digest=$(sha256sum "$output" | cut -d ' ' -f 1)
echo "$digest"
