#!/usr/bin/env bash
set -euo pipefail

mode=""
while (($#)); do
  case "$1" in
    --mode) mode="${2:-}"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ "$mode" == smoke || "$mode" == pilot || "$mode" == publication ]] || { echo "--mode smoke|pilot|publication is required" >&2; exit 2; }

repo="$(git rev-parse --show-toplevel)"
failures=()
for required_command in git go jq sha256sum; do
  command -v "$required_command" >/dev/null 2>&1 || failures+=("required command unavailable: $required_command")
done
commit="$(git rev-parse HEAD)"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || failures+=("Git commit is not a full SHA")

if [[ "$mode" != smoke ]]; then
  command -v docker >/dev/null 2>&1 || failures+=("required command unavailable: docker")
  grep -qi microsoft /proc/sys/kernel/osrelease || failures+=("not WSL2")
  docker info >/dev/null 2>&1 || failures+=("Docker unavailable")
  [[ "$(stat -fc %T /sys/fs/cgroup)" == cgroup2fs ]] || failures+=("cgroup v2 unavailable")
  compose_version="$(docker compose version --short 2>/dev/null | sed 's/^v//')"
  [[ "$(printf '%s\n' 2.24.4 "$compose_version" | sort -V | head -n1)" == 2.24.4 ]] || failures+=("Docker Compose < 2.24.4")
fi

if [[ "$mode" == publication ]]; then
  for required_command in psql containerd; do
    command -v "$required_command" >/dev/null 2>&1 || failures+=("required command unavailable: $required_command")
  done
  # shellcheck disable=SC1091
  . /etc/os-release
  [[ "${ID:-}" == ubuntu && "${VERSION_ID:-}" == 22.04 ]] || failures+=("publication requires Ubuntu 22.04")
  [[ "$repo" != /mnt/?/* ]] || failures+=("repository is on a Windows mount")
  memory_kib="$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"
  ((memory_kib >= 24*1024*1024)) || failures+=("WSL-visible memory < 24 GiB")
  processors="$(nproc)"; ((processors >= 8)) || failures+=("nproc < 8")
  swap_kib="$(awk '/^SwapTotal:/{print $2}' /proc/meminfo)"; ((swap_kib == 0)) || failures+=("SwapTotal is not zero")
  free_kib="$(df -Pk "$repo" | awk 'NR==2{print $4}')"; ((free_kib >= 180*1024*1024)) || failures+=("filesystem free space < 180 GiB")
  dirty="$(git status --porcelain=v1 --untracked-files=all -- . ':(exclude)evaluation/final-v5-wsl2/raw' ':(exclude)evaluation/final-v5-wsl2/generated')"
  [[ -z "$dirty" ]] || failures+=("measured paths are dirty")
  daemon_restarts="$(systemctl show docker.service --property=NRestarts --value 2>/dev/null || true)"
  [[ -z "$daemon_restarts" || "$daemon_restarts" == 0 ]] || failures+=("Docker daemon restart detected")
fi

if ((${#failures[@]})); then
  printf 'preflight failure: %s\n' "${failures[@]}" >&2
  exit 1
fi
if [[ "$mode" == publication ]]; then echo "publication_eligible=true"; else echo "publication_eligible=false"; fi
echo "commit=$commit"
