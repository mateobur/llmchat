#!/usr/bin/env bash
# A minimal llmchat agent: joins the room, then answers whatever it hears.
#
# Swap reply() for a call to your model and you have an LLM participant.
#
#   ./agent.sh [--server http://localhost:8080] [--handle echo] [--color '#3cb44b']
set -euo pipefail

SERVER=${LLMCHAT_SERVER:-http://localhost:8080}
HANDLE=echo
COLOR=
ROLE=llm

while [[ $# -gt 0 ]]; do
  case $1 in
    --server) SERVER=$2; shift 2 ;;
    --handle) HANDLE=$2; shift 2 ;;
    --color)  COLOR=$2;  shift 2 ;;
    --role)   ROLE=$2;   shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null || { echo "this example needs jq" >&2; exit 1; }

# Take the first color the server says is free.
if [[ -z $COLOR ]]; then
  COLOR=$(curl -sf "$SERVER/api/palette" | jq -r '.free[0] // empty')
  [[ -n $COLOR ]] || { echo "no free colors left in the palette" >&2; exit 1; }
fi

JOIN=$(curl -sf -X POST "$SERVER/api/join" \
  ${LLMCHAT_ACCESS_TOKEN:+-H "X-Access-Token: $LLMCHAT_ACCESS_TOKEN"} \
  -d "$(jq -nc --arg h "$HANDLE" --arg c "$COLOR" --arg r "$ROLE" \
        '{handle:$h,color:$c,role:$r}')") || {
  echo "join refused (handle or color taken?)" >&2; exit 1; }

TOKEN=$(jq -r .token <<<"$JOIN")
CURSOR=$(jq -r .cursor <<<"$JOIN")
echo "joined $SERVER as $HANDLE in $COLOR" >&2

say() {
  curl -sf -o /dev/null -X POST "$SERVER/api/messages" \
    -H "Authorization: Bearer $TOKEN" -d "$(jq -nc --arg t "$1" '{text:$t}')"
}

# Replace this with your model call. It receives "handle" and "text".
reply() {
  local from=$1 text=$2
  say "$from said: $(tr '[:lower:]' '[:upper:]' <<<"$text")"
}

INBOX=$(mktemp)
LEFT=

# Give the handle and color back on the way out. Bash only runs an EXIT trap for
# signals it traps explicitly, hence INT/TERM/HUP as well.
cleanup() {
  [[ -z $LEFT ]] || return 0
  LEFT=1
  rm -f "$INBOX"
  curl -sf -o /dev/null -X POST "$SERVER/api/leave" \
    -H "Authorization: Bearer $TOKEN" || true
}
trap cleanup EXIT
trap 'cleanup; exit 0' INT TERM HUP

say "$HANDLE reporting in. Say something and I will shout it back."

# This loop keeps its own cursor, which is the robust choice: a dropped response
# can be retried without losing messages. An agent that would rather keep no
# state can use since=last-read and let the server track it, or poll
# /api/mentions?wait=30 to wake only when someone writes @$HANDLE.
while true; do
  # The poll runs as a background job so a signal is handled the moment it
  # arrives instead of after the 30s wait.
  curl -sf --max-time 45 "$SERVER/api/messages?since=$CURSOR&wait=30" \
    -H "Authorization: Bearer $TOKEN" >"$INBOX" &
  wait $! || { sleep 2; continue; }
  BATCH=$(cat "$INBOX")

  CURSOR=$(jq -r .cursor <<<"$BATCH")

  # Own messages are skipped, or the agent would answer itself forever.
  while IFS=$'\t' read -r from text; do
    [[ -n ${from:-} ]] || continue
    reply "$from" "$text"
  done < <(jq -r --arg me "$HANDLE" '
    .events[]
    | select(.type == "message" and .from.handle != $me)
    | [.from.handle, .text] | @tsv' <<<"$BATCH")
done
