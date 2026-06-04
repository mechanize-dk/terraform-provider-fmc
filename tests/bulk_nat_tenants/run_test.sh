#!/usr/bin/env bash
# run_test.sh — integration test for fmc_mze_manual_nat_rules + fmc_mze_auto_nat_rules.
# Mirrors tests/idempotency/run_test.sh shape.

set -euo pipefail
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
pass()   { echo -e "${GREEN}✓ $*${NC}"; }
fail()   { echo -e "${RED}✗ $*${NC}"; exit 1; }
info()   { echo -e "${YELLOW}→ $*${NC}"; }
header() { echo -e "\n${BOLD}══ $* ══${NC}"; }

FMC_USERNAME=""; FMC_PASSWORD=""; FMC_URL=""; TERRAFORM_BIN=""; FMC_FTDV_NAME="${FMC_FTDV:-}"
while [[ $# -gt 0 ]]; do
  case $1 in
    -u|--username) FMC_USERNAME="$2"; shift 2 ;;
    -p|--password) FMC_PASSWORD="$2"; shift 2 ;;
    --url) FMC_URL="${2%/}"; shift 2 ;;
    --terraform) TERRAFORM_BIN="$2"; shift 2 ;;
    --ftdv) FMC_FTDV_NAME="$2"; shift 2 ;;
    -h|--help) echo "Usage: $0 -u U -p P --url URL [--terraform PATH] [--ftdv NAME]"; exit 0 ;;
    *) fail "Unknown arg: $1" ;;
  esac
done
[[ -z "$FMC_USERNAME" || -z "$FMC_PASSWORD" || -z "$FMC_URL" ]] && fail "missing -u/-p/--url"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) PROVIDER_BIN="$SCRIPT_DIR/terraform-provider-fmc.exe" ;;
  *) PROVIDER_BIN="$SCRIPT_DIR/terraform-provider-fmc" ;;
esac
to_native_path() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else echo "$1"; fi; }
TFRC="$SCRIPT_DIR/dev.tfrc"

[[ -z "$TERRAFORM_BIN" ]] && TERRAFORM_BIN="$(command -v terraform 2>/dev/null || true)"
[[ -x "$TERRAFORM_BIN" ]] || fail "terraform not found"
terraform() { "$TERRAFORM_BIN" "$@"; }

header "Building provider"
(cd "$REPO_DIR" && go build -o "$PROVIDER_BIN" .) || fail "go build failed"
pass "Provider binary: $PROVIDER_BIN"

cat > "$TFRC" <<EOF
provider_installation {
  dev_overrides { "CiscoDevNet/fmc" = "$(to_native_path "$SCRIPT_DIR")" }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE="$(to_native_path "$TFRC")"
export FMC_USERNAME FMC_PASSWORD FMC_URL FMC_INSECURE=true

cd "$SCRIPT_DIR"
echo "ftdv_name = \"${FMC_FTDV_NAME}\"" > terraform.tfvars

cleanup() {
  info "Cleaning up..."
  cd "$SCRIPT_DIR"
  # Unassign any NAT policies still bound to a device — destroy of fmc_ftd_nat_policy
  # fails (HTTP 400) when the policy is still assigned. PUT-empty-targets is the
  # supported unassign path (DELETE on /assignment/policyassignments returns 405).
  if [[ -f terraform.tfstate ]] && [[ -n "$AUTH_TOKEN" ]]; then
    NATP_FOR_CLEANUP=$(terraform show -json 2>/dev/null | python3 -c "
import sys, json
try:
    s = json.load(sys.stdin)
    for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
        if r.get('type')=='fmc_ftd_nat_policy' and r.get('name')=='shared':
            print(r.get('values',{}).get('id',''))
            break
except Exception:
    pass
" 2>/dev/null || true)
    if [[ -n "$NATP_FOR_CLEANUP" ]]; then
      "${CURL[@]}" -X PUT "${FMC_URL}/api/fmc_config/v1/domain/${DOMAIN_UUID}/assignment/policyassignments/${NATP_FOR_CLEANUP}" \
        -H "X-auth-access-token: ${AUTH_TOKEN}" -H "Content-Type: application/json" \
        -d "{\"type\":\"PolicyAssignment\",\"policy\":{\"id\":\"${NATP_FOR_CLEANUP}\",\"type\":\"FTDNatPolicy\"},\"targets\":[]}" \
        > /dev/null 2>&1 || true
    fi
  fi
  [[ -f terraform.tfstate ]] && terraform destroy -auto-approve 2>/dev/null || true
  rm -f terraform.tfstate terraform.tfstate.backup .terraform.lock.hcl terraform.tfvars "$PROVIDER_BIN" "$TFRC" "$AUTH_HDR_FILE"
  rm -rf .terraform
}
trap cleanup EXIT

# Helper for FMC API auth (used by later scenarios)
CURL=(curl -s -k)
AUTH_HDR_FILE="$(mktemp)"
AUTH_TOKEN=""
DOMAIN_UUID=""
fmc_authenticate() {
  "${CURL[@]}" -D "$AUTH_HDR_FILE" -X POST "${FMC_URL}/api/fmc_platform/v1/auth/generatetoken" \
    --user "${FMC_USERNAME}:${FMC_PASSWORD}" -H "Content-Type: application/json" -o /dev/null
  AUTH_TOKEN="$(awk 'tolower($1)=="x-auth-access-token:"{print $2}' "$AUTH_HDR_FILE" | tr -d '\r\n')"
  DOMAIN_UUID="$(awk 'tolower($1)=="domain_uuid:"{print $2}' "$AUTH_HDR_FILE" | tr -d '\r\n')"
  [[ -z "$AUTH_TOKEN" ]] && return 1
  return 0
}

# ─── SCENARIO 1 — multi-tenant baseline ──────────────────────────────────────
header "SCENARIO 1 — multi-tenant baseline"
info "terraform apply (full config)..."
terraform apply -auto-approve
pass "Apply succeeded"

info "terraform plan -detailed-exitcode (expect 0 = no changes)..."
terraform plan -detailed-exitcode -out=/dev/null
pass "SCENARIO 1 CONFIRMED: no drift after baseline apply"

# ─── SCENARIO 2 — idempotency (pre-create rule, expect adoption) ────────────
header "SCENARIO 2 — idempotency"

[[ -z "$FMC_FTDV_NAME" ]] && fail "SCENARIO 2 requires --ftdv (FMC's content-level dedup only fires when the NAT policy is bound to a device)"

# Pull resource IDs from the live state.
TF_STATE_JSON=$(terraform show -json)
get_state_id() {
  local rtype="$1" rname="$2"
  echo "$TF_STATE_JSON" | python3 -c "
import sys, json
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='${rtype}' and r.get('name')=='${rname}':
        print(r.get('values',{}).get('id',''))
        break
"
}
NATP_ID=$(get_state_id fmc_ftd_nat_policy shared)
SRC_A=$(get_state_id fmc_host src_a)
TRANS_A=$(get_state_id fmc_host trans_a)
ZONE_A=$(get_state_id fmc_security_zone tenant_a)

info "Removing tenant_a manual_nat_rules from FMC + state..."
terraform destroy -auto-approve -target=fmc_mze_manual_nat_rules.tenant_a > /dev/null

info "Assigning NAT policy to '$FMC_FTDV_NAME' so FMC's content dedup activates..."
fmc_authenticate || fail "FMC re-auth failed"
DEV_RESP=$("${CURL[@]}" -H "X-auth-access-token: $AUTH_TOKEN" \
  "${FMC_URL}/api/fmc_config/v1/domain/${DOMAIN_UUID}/devices/devicerecords?limit=1000&expanded=true")
DEVICE_ID=$(echo "$DEV_RESP" | FMC_FTDV_NAME="$FMC_FTDV_NAME" python3 -c "
import sys, json, os
want = os.environ['FMC_FTDV_NAME']
d = json.loads(sys.stdin.read())
for it in d.get('items', []):
    if it.get('name') == want:
        print(it.get('id',''))
        break
")
[[ -z "$DEVICE_ID" ]] && fail "FTDv '$FMC_FTDV_NAME' not found"

ASSIGN_PATH="/api/fmc_config/v1/domain/${DOMAIN_UUID}/assignment/policyassignments"
ASSIGN_RESP=$("${CURL[@]}" -X POST "${FMC_URL}${ASSIGN_PATH}" \
  -H "X-auth-access-token: ${AUTH_TOKEN}" -H "Content-Type: application/json" \
  -d "{\"type\":\"PolicyAssignment\",\"policy\":{\"id\":\"${NATP_ID}\",\"type\":\"FTDNatPolicy\"},\"targets\":[{\"id\":\"${DEVICE_ID}\",\"type\":\"Device\",\"name\":\"${FMC_FTDV_NAME}\"}]}")
ASSIGN_STATUS=$(echo "$ASSIGN_RESP" | python3 -c "
import sys, json
d = json.loads(sys.stdin.read())
if 'id' in d:
    print('OK')
else:
    msgs = d.get('error',{}).get('messages', [{}])
    print(msgs[0].get('description', 'unknown'))
")
if [[ "$ASSIGN_STATUS" != "OK" ]]; then
  info "Assignment returned: $ASSIGN_STATUS (may already be bound — continuing)"
fi
pass "NAT policy assignment in place"

info "Pre-creating a tenant_a-shaped manual NAT rule via API..."
fmc_authenticate || fail "FMC re-auth failed"
PRECREATE_RESP=$("${CURL[@]}" -X POST \
  "${FMC_URL}/api/fmc_config/v1/domain/${DOMAIN_UUID}/policy/ftdnatpolicies/${NATP_ID}/manualnatrules?section=before_auto" \
  -H "X-auth-access-token: ${AUTH_TOKEN}" -H "Content-Type: application/json" \
  -d "{\"type\":\"FTDManualNatRule\",\"natType\":\"STATIC\",\"originalSource\":{\"id\":\"${SRC_A}\"},\"translatedSource\":{\"id\":\"${TRANS_A}\"},\"sourceInterface\":{\"id\":\"${ZONE_A}\"}}")
PRECREATED_ID=$(echo "$PRECREATE_RESP" | python3 -c "import sys,json;print(json.loads(sys.stdin.read()).get('id',''))")
[[ -z "$PRECREATED_ID" ]] && fail "pre-create failed: $PRECREATE_RESP"
pass "Pre-created rule id: $PRECREATED_ID"

info "terraform apply -target=fmc_mze_manual_nat_rules.tenant_a (expect Patch-11 adoption)..."
terraform apply -auto-approve -target=fmc_mze_manual_nat_rules.tenant_a

TF_ID=$(terraform show -json | python3 -c "
import sys, json
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_mze_manual_nat_rules' and r.get('name')=='tenant_a':
        items = r.get('values',{}).get('before_auto',[])
        if items: print(items[0].get('id',''))
        break
")
if [[ "$TF_ID" == "$PRECREATED_ID" ]]; then
  pass "SCENARIO 2 CONFIRMED: adoption worked — state ID matches pre-created ($TF_ID)"
else
  fail "SCENARIO 2: ID mismatch — state=$TF_ID, pre-created=$PRECREATED_ID"
fi

# Scenarios 3-5 follow in subsequent commits.

echo
echo -e "${GREEN}${BOLD}══════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  All scenarios passed.${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════${NC}"
