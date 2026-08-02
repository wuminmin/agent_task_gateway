#!/usr/bin/env bash
set -euo pipefail
go run ./evaluation/cmd/final-v5 validate --root evaluation/final-v5-wsl2
evaluation/final-v5-wsl2/scripts/validate-pilot-harness.sh
