#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$paper_dir/../.." && pwd)

if [ "$#" -gt 1 ]; then
    printf 'usage: %s [refresh-exposure|final]\n' "$0" >&2
    exit 2
fi

build_mode=${1:-compile}
evidence_mode=draft
case "$build_mode" in
    compile)
        ;;
    refresh-exposure)
        printf '%s\n' 'Explicitly refreshing exposure evidence before the paper build.' >&2
        "$repository_root/evaluation/run-exposure.sh"
        ;;
    final)
        evidence_mode=final
        ;;
    *)
        printf 'usage: %s [refresh-exposure|final]\n' "$0" >&2
        exit 2
        ;;
esac

exec "$paper_dir/compile.sh" "$evidence_mode"
