#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$paper_dir/../.." && pwd)
image_name=${TASKGATE_TKDE_PAPER_IMAGE:-taskgate-tkde-paper:local}

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

docker build \
    --file "$paper_dir/Dockerfile" \
    --tag "$image_name" \
    "$repository_root"

docker run --rm \
    --user "$(id -u):$(id -g)" \
    --env HOME=/tmp \
    --env TEXMFCONFIG=/tmp/texmf-config \
    --env TEXMFVAR=/tmp/texmf-var \
    --volume "$repository_root:/workspace" \
    "$image_name" "$evidence_mode"
