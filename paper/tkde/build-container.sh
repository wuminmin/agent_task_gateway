#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$paper_dir/../.." && pwd)
image_name=${TASKGATE_TKDE_PAPER_IMAGE:-taskgate-tkde-paper:local}

"$repository_root/evaluation/run-exposure.sh"

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
    "$image_name"
