#!/usr/bin/env bash
# probe_autonat_dup.sh — one-shot probe that captures FMC's response when an
# auto-NAT rule is POSTed twice with identical content.
#
# Usage: ./probe_autonat_dup.sh -u USER -p PASS --url https://fmc
#
# Outputs:
#   - prints the first-POST response (expected: 201 with rule body)
#   - prints the second-POST response (expected: 400 or 409 — what we want to see)
#   - leaves /tmp/probe_autonat_dup_first.txt and /tmp/probe_autonat_dup_second.txt
#     for analysis
# Cleans up after itself (DELETEs the NAT policy + probe hosts).

set -euo pipefail
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'

FMC_USERNAME=""; FMC_PASSWORD=""; FMC_URL=""
while [[ $# -gt 0 ]]; do
  case $1 in
    -u|--username) FMC_USERNAME="$2"; shift 2 ;;
    -p|--password) FMC_PASSWORD="$2"; shift 2 ;;
    --url) FMC_URL="${2%/}"; shift 2 ;;
    *) echo "Unknown: $1"; exit 1 ;;
  esac
done
[[ -z "$FMC_USERNAME" || -z "$FMC_PASSWORD" || -z "$FMC_URL" ]] && { echo "missing -u/-p/--url"; exit 1; }

CURL=(curl -s -k)
AUTH_HDR=$(mktemp)
"${CURL[@]}" -D "$AUTH_HDR" -X POST "$FMC_URL/api/fmc_platform/v1/auth/generatetoken" \
  --user "$FMC_USERNAME:$FMC_PASSWORD" -H "Content-Type: application/json" -o /dev/null
TOK=$(awk 'tolower($1)=="x-auth-access-token:"{print $2}' "$AUTH_HDR" | tr -d '\r\n')
DOM=$(awk 'tolower($1)=="domain_uuid:"{print $2}' "$AUTH_HDR" | tr -d '\r\n')
rm -f "$AUTH_HDR"
[[ -z "$TOK" ]] && { echo "auth failed"; exit 1; }

API="$FMC_URL/api/fmc_config/v1/domain/$DOM"

# Create temporary NAT policy + host objects
NATP=$("${CURL[@]}" -X POST "$API/policy/ftdnatpolicies" \
  -H "X-auth-access-token: $TOK" -H "Content-Type: application/json" \
  -d '{"name":"autonat-dup-probe","type":"FTDNatPolicy"}' \
  | python3 -c "import sys,json;print(json.loads(sys.stdin.read()).get('id',''))")
[[ -z "$NATP" ]] && { echo "policy create failed"; exit 1; }
echo -e "${YELLOW}→${NC} NAT policy: $NATP"

ORIG=$("${CURL[@]}" -X POST "$API/object/hosts" \
  -H "X-auth-access-token: $TOK" -H "Content-Type: application/json" \
  -d '{"name":"autonat-dup-probe-orig","value":"10.99.99.1","type":"Host"}' \
  | python3 -c "import sys,json;print(json.loads(sys.stdin.read()).get('id',''))")
TRANS=$("${CURL[@]}" -X POST "$API/object/hosts" \
  -H "X-auth-access-token: $TOK" -H "Content-Type: application/json" \
  -d '{"name":"autonat-dup-probe-trans","value":"10.99.99.2","type":"Host"}' \
  | python3 -c "import sys,json;print(json.loads(sys.stdin.read()).get('id',''))")
[[ -z "$ORIG" || -z "$TRANS" ]] && { echo "host create failed"; exit 1; }
echo -e "${YELLOW}→${NC} orig host: $ORIG"
echo -e "${YELLOW}→${NC} trans host: $TRANS"

# First POST: should succeed
echo
echo -e "${YELLOW}═══ First POST (expecting 201) ═══${NC}"
"${CURL[@]}" -X POST -i "$API/policy/ftdnatpolicies/$NATP/autonatrules" \
  -H "X-auth-access-token: $TOK" -H "Content-Type: application/json" \
  -d "{\"type\":\"FTDAutoNatRule\",\"natType\":\"STATIC\",\"originalNetwork\":{\"id\":\"$ORIG\",\"type\":\"Host\"},\"translatedNetwork\":{\"id\":\"$TRANS\",\"type\":\"Host\"}}" \
  | tee /tmp/probe_autonat_dup_first.txt

# Second POST: same content, expecting failure
echo
echo -e "${YELLOW}═══ Second POST (expecting 400 or 409 — capture this) ═══${NC}"
"${CURL[@]}" -X POST -i "$API/policy/ftdnatpolicies/$NATP/autonatrules" \
  -H "X-auth-access-token: $TOK" -H "Content-Type: application/json" \
  -d "{\"type\":\"FTDAutoNatRule\",\"natType\":\"STATIC\",\"originalNetwork\":{\"id\":\"$ORIG\",\"type\":\"Host\"},\"translatedNetwork\":{\"id\":\"$TRANS\",\"type\":\"Host\"}}" \
  | tee /tmp/probe_autonat_dup_second.txt

# Cleanup
echo
echo -e "${YELLOW}→${NC} Cleaning up..."
"${CURL[@]}" -X DELETE -H "X-auth-access-token: $TOK" "$API/policy/ftdnatpolicies/$NATP" > /dev/null
"${CURL[@]}" -X DELETE -H "X-auth-access-token: $TOK" "$API/object/hosts/$ORIG" > /dev/null
"${CURL[@]}" -X DELETE -H "X-auth-access-token: $TOK" "$API/object/hosts/$TRANS" > /dev/null
echo -e "${GREEN}✓${NC} probe complete. Inspect /tmp/probe_autonat_dup_second.txt"
