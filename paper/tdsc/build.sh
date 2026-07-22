#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$paper_dir/../.." && pwd)

# Reconstruct paper-results.json from its raw inputs. In particular, this
# overwrites a manually edited generated summary before any table is rendered.
"$repository_root/evaluation/generate-artifacts.sh" --allow-empty
exec "$paper_dir/compile.sh"
