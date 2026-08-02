#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$paper_dir"

if [ "$#" -gt 1 ]; then
    printf 'usage: %s [draft|final]\n' "$0" >&2
    exit 2
fi

evidence_mode=${1:-draft}
case "$evidence_mode" in
    draft|final)
        ;;
    *)
        printf 'usage: %s [draft|final]\n' "$0" >&2
        exit 2
        ;;
esac

python3 generate_evidence.py --evidence-mode "$evidence_mode"
latexmk -g -pdf -interaction=nonstopmode -halt-on-error -file-line-error main.tex
latexmk -g -pdf -interaction=nonstopmode -halt-on-error -file-line-error supplement.tex
