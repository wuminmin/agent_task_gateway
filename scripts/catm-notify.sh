#!/usr/bin/env bash
# Push a progress heartbeat to the author's phone through the catm MCP server.
#
#   scripts/catm-notify.sh sync   "<what I am doing>"   open/refresh the work cycle
#   scripts/catm-notify.sh notify "<three-line heartbeat>"  push to the author
#   scripts/catm-notify.sh done   "<final answer, verbatim>"  close the work cycle
#
# Why this exists: the `mcp__catm__*` tools are not always exposed to a Claude
# session even when `claude mcp list` reports catm as connected (observed
# 2026-08-16). Without this script the 30-minute heartbeat rule in CLAUDE.md
# would silently become unexecutable in exactly those sessions -- and a rule
# that cannot run is worse than no rule, because it reads as if it ran.
#
# The bearer token is read from ~/.claude.json at call time and never printed,
# so no credential enters this repository or any log this script writes.
set -euo pipefail

CONFIG=${CATM_CONFIG:-$HOME/.claude.json}
STATE=${CATM_STATE:-${TMPDIR:-/tmp}/catm-session-$(id -u).json}
WORKSPACE=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

die() { echo "catm-notify: $*" >&2; exit 1; }

read_config() {  # -> CATM_URL, CATM_TOKEN
  [ -f "$CONFIG" ] || die "no $CONFIG to read the catm endpoint from"
  eval "$(python3 - "$CONFIG" <<'PY'
import json, shlex, sys
cfg = json.load(open(sys.argv[1]))
srv = cfg.get("mcpServers", {}).get("catm")
if not srv:
    sys.exit("catm-notify: no 'catm' server in mcpServers")
auth = srv.get("headers", {}).get("Authorization", "")
if not srv.get("url") or not auth:
    sys.exit("catm-notify: catm entry has no url or Authorization header")
print("CATM_URL=%s" % shlex.quote(srv["url"]))
print("CATM_AUTH=%s" % shlex.quote(auth))
PY
)"
}

# Open a fresh MCP HTTP session. The server hands back an mcp-session-id header
# that every later call must carry; a stale one fails with "invalid or missing
# MCP session", so we always handshake rather than cache it.
mcp_open() {
  local headers
  headers=$(curl -sS -D - -o /dev/null --max-time 60 -X POST "$CATM_URL" \
    -H "Authorization: $CATM_AUTH" -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"catm-notify.sh","version":"1"}}}')
  MCP_SESSION=$(printf '%s' "$headers" | tr -d '\r' | awk 'tolower($1)=="mcp-session-id:"{print $2}')
  [ -n "$MCP_SESSION" ] || die "server returned no mcp-session-id"
  curl -sS --max-time 60 -X POST "$CATM_URL" \
    -H "Authorization: $CATM_AUTH" -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" -H "mcp-session-id: $MCP_SESSION" \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
}

# mcp_call <tool> <json-arguments> -> prints the tool's structured result.
# Responses arrive as SSE ("event: message\ndata: {...}"), so strip the framing.
mcp_call() {
  local body result
  body=$(printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"%s","arguments":%s}}' "$1" "$2")
  result=$(curl -sS --max-time 60 -X POST "$CATM_URL" \
    -H "Authorization: $CATM_AUTH" -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" -H "mcp-session-id: $MCP_SESSION" \
    -d "$body")
  python3 -c '
import json, sys
raw = sys.argv[1]
payload = next((l[6:] for l in raw.splitlines() if l.startswith("data: ")), raw)
try:
    msg = json.loads(payload)
except json.JSONDecodeError:
    sys.exit("catm-notify: unparsable response: %s" % raw[:200])
if "error" in msg:
    sys.exit("catm-notify: %s" % msg["error"].get("message", msg["error"]))
res = msg.get("result", {})
if res.get("isError"):
    sys.exit("catm-notify: %s" % res["content"][0]["text"])
print(json.dumps(res.get("structuredContent", res)))
' "$result"
}

# args_json k1 v1 k2 v2 ... -> a JSON object. Building the object here rather
# than with nested "$(printf ...)" is deliberate: the nested form parsed wrong
# inside a case branch, and bash swallowed the rest of the script into the
# substitution instead of erroring.
args_json() {
  python3 -c '
import json, sys
a = sys.argv[1:]
print(json.dumps(dict(zip(a[0::2], a[1::2])), ensure_ascii=False))
' "$@"
}

field() { python3 -c 'import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }

# sync_session both opens the work cycle and refreshes its stage line. Its
# session_id/work_cycle_id are what notify_author and notify_work_completed key
# on, so every mode runs it first -- one stale id and the heartbeat lands in a
# cycle the author has already closed.
# Reuse the cached session_id when there is one: sync_session mints a brand-new
# session whenever the field is omitted, which would scatter one work session's
# heartbeats across a dozen sessions in the author's feed.
open_cycle() {
  local payload out prior=""
  [ -f "$STATE" ] && prior=$(field "$(cat "$STATE")" session_id 2>/dev/null || true)
  payload=$(args_json agent claude workspace "$WORKSPACE" \
    label "${CATM_LABEL:-TaskGate / TKDE}" status working stage "$1" \
    ${prior:+session_id "$prior"})
  out=$(mcp_call sync_session "$payload")
  printf '%s' "$out" >"$STATE"
  SESSION_ID=$(field "$out" session_id)
  CYCLE_ID=$(field "$out" work_cycle_id)
}

mode=${1:-}; text=${2:-}
[ -n "$mode" ] && [ -n "$text" ] || die "usage: $0 {sync|notify|done} \"<message>\""

read_config
mcp_open

case "$mode" in
  sync)
    open_cycle "$text"
    echo "catm: session=$SESSION_ID cycle=$CYCLE_ID stage synced"
    ;;
  notify)
    open_cycle "$text"
    payload=$(args_json session_id "$SESSION_ID" work_cycle_id "$CYCLE_ID" message "$text")
    mcp_call notify_author "$payload"
    echo "catm: heartbeat sent at $(date -u +%H:%M) UTC"
    ;;
  done)
    open_cycle "delivering final answer"
    payload=$(args_json session_id "$SESSION_ID" work_cycle_id "$CYCLE_ID" summary "$text")
    mcp_call notify_work_completed "$payload"
    echo "catm: work cycle $CYCLE_ID closed"
    ;;
  *)
    die "unknown mode '$mode' (expected sync, notify or done)"
    ;;
esac
