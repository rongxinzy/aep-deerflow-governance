#!/usr/bin/env bash
# Seed the cicd AEP deployment with the minimum the digital-employee stack
# needs: wecom identity source + saber mapping, gov-service account, demo
# users. Idempotent-ish (PUT mapping, best-effort creates).
#
# Usage (from the cicd host or anywhere with access to the control service):
#   AEP_BASE=http://127.0.0.1:30180 ADMIN_PASSWORD=... GOV_PASSWORD=... \
#   ADMIN_ID=<aep-admin-user-id|auto> ./seed-aep.sh
set -euo pipefail

AEP_BASE="${AEP_BASE:-http://127.0.0.1:30180}"
: "${ADMIN_PASSWORD:?}"
: "${GOV_PASSWORD:?}"

hdr=(-H "X-AEP-Protocol-Version: 1.0" -H "Content-Type: application/json")

token() {
  curl -s -X POST "$AEP_BASE/aep/v1/auth/password/login" "${hdr[@]}" \
    -d "{\"deploymentId\":\"demo\",\"sessionId\":\"seed-$(date +%s)\",\"username\":\"admin\",\"password\":\"$ADMIN_PASSWORD\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["accessToken"])'
}
TOKEN="$(token)"
auth=(-H "Authorization: Bearer $TOKEN" "${hdr[@]}")

if [ -z "${ADMIN_ID:-}" ]; then
  ADMIN_ID="$(curl -s "$AEP_BASE/aep/v1/user/me" "${auth[@]}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["user"]["id"])')"
fi
echo "admin id: $ADMIN_ID"

# 1. Identity source + mapping
curl -s -X POST "$AEP_BASE/aep/v1/admin/identity-sources" "${auth[@]}" \
  -d '{"id":"wecom","kind":"directory","displayName":"企业微信"}' | head -c 120; echo
curl -s -X PUT "$AEP_BASE/aep/v1/admin/identity-sources/wecom/mappings" "${auth[@]}" \
  -d "{\"externalSubjectType\":\"user\",\"externalId\":\"saber\",\"localSubjectId\":\"$ADMIN_ID\"}" | head -c 120; echo

# 2. Team + role for the governance service account
TEAM="$(curl -s -X POST "$AEP_BASE/aep/v1/admin/teams" "${auth[@]}" \
  -d '{"id":"platform","name":"平台服务"}' \
  | python3 -c 'import json,sys
d = json.load(sys.stdin)
print(d.get("id") or d.get("team", {}).get("id", ""))' 2>/dev/null || true)"
if [ -z "$TEAM" ]; then
  TEAM="$(curl -s "$AEP_BASE/aep/v1/admin/teams?limit=100" "${auth[@]}" \
    | python3 -c 'import json,sys
for t in json.load(sys.stdin).get("items", []):
    if t.get("name") == "platform":
        print(t["id"]); break')"
fi
echo "team: $TEAM"

ROLE="$(curl -s -X POST "$AEP_BASE/aep/v1/admin/roles" "${auth[@]}" \
  -d '{"id":"gov-service","name":"治理服务","permissions":["identity.read","data_scope.read"]}' \
  | python3 -c 'import json,sys
d = json.load(sys.stdin)
print(d.get("id") or d.get("role", {}).get("id", ""))' 2>/dev/null || true)"
if [ -z "$ROLE" ]; then
  ROLE="$(curl -s "$AEP_BASE/aep/v1/admin/roles?limit=100" "${auth[@]}" \
    | python3 -c 'import json,sys
for r in json.load(sys.stdin).get("items", []):
    if r.get("name") == "gov-service":
        print(r["id"]); break')"
fi
echo "role: $ROLE"

# 3. gov-service user (governance middleware client credentials)
curl -s -X POST "$AEP_BASE/aep/v1/admin/users" "${auth[@]}" \
  -d "{\"deploymentId\":\"demo\",\"username\":\"gov-service\",\"displayName\":\"gov-service\",\"temporaryPassword\":\"${GOV_PASSWORD}xxA1\",\"teamIds\":[\"$TEAM\"],\"roleIds\":[\"$ROLE\"],\"requirePasswordChange\":false}" | head -c 160; echo

echo "seed done"
