#!/bin/sh
set -eu

cleanup() {
  docker compose down --volumes --remove-orphans
}
trap cleanup EXIT

docker compose up --build --detach --wait
curl --fail --silent http://127.0.0.1:8080/health/ready >/dev/null
curl --fail --silent http://127.0.0.1:8090/health/ready >/dev/null
