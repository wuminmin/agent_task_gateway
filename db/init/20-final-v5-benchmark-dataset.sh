#!/bin/sh
set -eu

# Create the Final-V5 benchmark corpus from the exact contract-indexed
# generator bytes mounted read-only at /opt/taskgate/final-v5-sql. Executing
# the frozen file itself, rather than a copy inside db/init, keeps a deployment
# from drifting away from evaluation/final-v5-wsl2/contracts/index-v1.json.
#
# The 20 prefix is load bearing. The generator asserts that
# 07-freeze-publications.sql already installed taskgate_snapshot_owner and
# taskgate_ordinal.reject_frozen_publication_mutation, and its own conditional
# GRANT only reaches gateway_reader once 10-reader.sh has created that role.
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set ON_ERROR_STOP=on \
  --file /opt/taskgate/final-v5-sql/benchmark-v1-generate.sql
