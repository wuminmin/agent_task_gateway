#!/bin/sh
set -eu

# Internal compile-only step. Public build entrypoints regenerate the evaluation
# summary from raw evidence before invoking this script.
paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$paper_dir"

python3 verify_manifest_vector.py
python3 generate_tables.py
# Force a pass so a container that supplies IEEEtran cannot reuse a PDF made by
# the host-only article fallback from the mounted workspace.
latexmk -g -pdf -interaction=nonstopmode -halt-on-error -file-line-error main.tex
