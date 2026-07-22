#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$paper_dir/../.." && pwd)
image_name=${TASKGATE_PAPER_IMAGE:-taskgate-paper:local}

# Generate the evaluation summary once on the host. The paper container invokes
# only compile.sh, so `make paper` and this direct wrapper do not generate it a
# second time.
"$repository_root/evaluation/generate-artifacts.sh" --allow-empty

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
