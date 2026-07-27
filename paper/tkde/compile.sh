#!/bin/sh
set -eu

paper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$paper_dir"

python3 generate_evidence.py
latexmk -g -pdf -interaction=nonstopmode -halt-on-error -file-line-error main.tex
latexmk -g -pdf -interaction=nonstopmode -halt-on-error -file-line-error supplement.tex
