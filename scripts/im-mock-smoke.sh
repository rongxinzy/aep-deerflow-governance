#!/usr/bin/env bash
# im-mock-smoke.sh — protocol-level smoke for an IM channel leg.
#
# Runs the picoclaw mockfeishu double (a real Lark websocket + REST double)
# and probes the deployed runtime's Feishu leg: wait for the runtime's ws
# client to attach, push one inbound message, then dump what the channel
# sent back (reaction / thinking card / final card).
#
# The mock is built from rongxinzy/picoclaw:
#   GOOS=linux GOARCH=amd64 go build -tags 'goolm,stdjson' \
#     -o /tmp/mockfeishu ./tests/aep-e2e/mockfeishu
#
# Env:
#   MOCK_FEISHU_BIN   path to the mockfeishu binary (default /tmp/mockfeishu)
#   MOCK_HTTP_PORT    http/bootstrap port (default 18086)
#   MOCK_WS_PORT      websocket port (default 18087)
#   ADVERTISE         host:port the runtime must dial back (defaults to
#                     <host-ip>:<MOCK_WS_PORT>)
#   APP_ID / APP_SECRET  must match channels.feishu in the runtime config
#   SENDER_OPEN_ID    platform sender id to push from (default ou_zhang_gov)
#   ATTACH_TIMEOUT    seconds to wait for the runtime ws client (default 180,
#                     the runtime retries the platform about every 120s)
set -u

BIN="${MOCK_FEISHU_BIN:-/tmp/mockfeishu}"
HTTP_PORT="${MOCK_HTTP_PORT:-18086}"
WS_PORT="${MOCK_WS_PORT:-18087}"
APP_ID="${APP_ID:-cli_gov}"
APP_SECRET="${APP_SECRET:-gov-secret}"
SENDER="${SENDER_OPEN_ID:-ou_zhang_gov}"
ADVERTISE="${ADVERTISE:-$(hostname -I | awk '{print $1}'):${WS_PORT}}"
ATTACH_TIMEOUT="${ATTACH_TIMEOUT:-180}"
BASE="http://127.0.0.1:${HTTP_PORT}"

echo "mockfeishu: ${BIN}  http=:${HTTP_PORT} ws=:${WS_PORT} advertise=${ADVERTISE}"
pkill -f "${BIN}" 2>/dev/null || true
sleep 1
nohup "${BIN}" -http "0.0.0.0:${HTTP_PORT}" -ws "0.0.0.0:${WS_PORT}" \
  -app-id "${APP_ID}" -app-secret "${APP_SECRET}" \
  -advertise "${ADVERTISE}" >/tmp/mockfeishu.log 2>&1 &
disown
sleep 2
cat /tmp/mockfeishu.log

echo "--- waiting for the runtime ws client to attach (<=${ATTACH_TIMEOUT}s) ---"
push() {
  curl -sS --noproxy '*' -o /dev/null -w '%{http_code}' --max-time 8 \
    -X POST "${BASE}/_test/push" -H 'Content-Type: application/json' \
    -d "{\"chatID\":\"oc_smoke\",\"senderOpenID\":\"${SENDER}\",\"text\":\"你好\",\"chatType\":\"p2p\"}"
}
for i in $(seq 1 "$((ATTACH_TIMEOUT / 5))"); do
  sleep 5
  code="$(push)"
  echo "t+$((i * 5))s push=${code}"
  [ "${code}" = "200" ] && break
done

echo "--- what the channel sent back (poll 60s) ---"
body='{"sent":null}'
for i in $(seq 1 20); do
  sleep 3
  body="$(curl -sS --noproxy '*' --max-time 5 "${BASE}/_test/sent" || echo '{"sent":null}')"
  n="$(printf '%s' "$body" | grep -o '"msg_type"' | wc -l)"
  echo "t+$((i * 3))s entries=${n}"
  [ "${n}" -ge 3 ] && break
done
printf '%s' "$body" | python3 -c 'import sys,json
d=json.load(sys.stdin).get("sent") or []
[print(i, (x.get("msg_type") or "?"), (x.get("content") or "")[:200]) for i,x in enumerate(d)]' 2>/dev/null \
  || printf '%s\n' "$body"
