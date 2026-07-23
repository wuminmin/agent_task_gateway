#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$paper_dir/../.." && pwd)

"$repository_root/evaluation/run-exposure.sh"
exec "$paper_dir/compile.sh"
