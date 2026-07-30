#!/bin/sh
set -eu

# The production sidecar registry deliberately gives content digests a unique
# owner. Day0-day2 retain the same entity-key set, so their sidecar content is
# deduplicated even though their publication manifests differ. Each retained
# Catalog-bound service therefore receives its own cloned reporting database
# and its own durable registry, matching the experiment's retained-instance
# routing model.
for day in day0 day1 day2 day3; do
  createdb \
    --username "$POSTGRES_USER" \
    --maintenance-db postgres \
    --template "$POSTGRES_DB" \
    --owner "$POSTGRES_USER" \
    "${POSTGRES_DB}_${day}"
done
