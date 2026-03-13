#!/usr/bin/env bash
# run_test.sh — validates the 409 idempotency fix for terraform-provider-fmc.
#
# Usage:
#   ./run_test.sh -u <username> -p <password> --url <fmc_url> [--terraform /path/to/terraform]
#
# The script:
#   1. Builds the provider from the repository root.
#   2. Runs a normal create/destroy cycle (regression test).
#   3. Pre-creates a host object via the FMC REST API, then runs
#      "terraform apply" and verifies it ingests the existing object
#      instead of failing with a conflict error.
#
# Requirements: go, terraform (>=1.0), curl, python3

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
pass()  { echo -e "${GREEN}✓ $*${NC}"; }
fail()  { echo -e "${RED}✗ $*${NC}"; exit 1; }
info()  { echo -e "${YELLOW}→ $*${NC}"; }
header(){ echo -e "\n${BOLD}══ $* ══${NC}"; }

# ── Argument parsing ──────────────────────────────────────────────────────────
FMC_USERNAME=""
FMC_PASSWORD=""
FMC_URL=""
TERRAFORM_BIN=""   # optional; defaults to 'terraform' found on PATH

usage() {
  echo "Usage: $0 -u <username> -p <password> --url <fmc_url> [--terraform /path/to/terraform]"
  echo ""
  echo "  -u, --username      FMC username"
  echo "  -p, --password      FMC password"
  echo "  --url               FMC base URL  (e.g. https://10.0.0.1)"
  echo "  --terraform <path>  Path to the terraform binary (default: terraform on PATH)"
  exit 1
}

while [[ $# -gt 0 ]]; do
  case $1 in
    -u|--username)  FMC_USERNAME="$2";  shift 2 ;;
    -p|--password)  FMC_PASSWORD="$2";  shift 2 ;;
    --url)          FMC_URL="$2";       shift 2 ;;
    --terraform)    TERRAFORM_BIN="$2"; shift 2 ;;
    -h|--help)      usage ;;
    *) echo "Unknown option: $1"; usage ;;
  esac
done

[[ -z "$FMC_USERNAME" || -z "$FMC_PASSWORD" || -z "$FMC_URL" ]] && {
  echo -e "${RED}Error: --username, --password and --url are all required.${NC}"
  usage
}

# Strip trailing slash from URL
FMC_URL="${FMC_URL%/}"

# ── Paths ─────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"   # tests/idempotency → tests → repo root
PROVIDER_BIN="$SCRIPT_DIR/terraform-provider-fmc"
TFRC="$SCRIPT_DIR/dev.tfrc"

# Test object settings
TEST_HOST_NAME="tf-idempotency-test-host"
TEST_HOST_IP="192.0.2.111"

# ── Resolve terraform binary ──────────────────────────────────────────────────
# If --terraform was not supplied, fall back to whatever is on PATH.
if [[ -z "$TERRAFORM_BIN" ]]; then
  TERRAFORM_BIN="$(command -v terraform 2>/dev/null || true)"
fi

# ── Prerequisite checks ───────────────────────────────────────────────────────
header "Checking prerequisites"
for cmd in go curl python3; do
  command -v "$cmd" &>/dev/null && pass "$cmd found" || fail "$cmd not found – please install it"
done

if [[ -z "$TERRAFORM_BIN" ]]; then
  fail "terraform not found on PATH — pass its location with --terraform /path/to/terraform"
fi
if [[ ! -x "$TERRAFORM_BIN" ]]; then
  fail "terraform binary not executable: $TERRAFORM_BIN"
fi
pass "terraform found ($TERRAFORM_BIN)"

# Alias so the rest of the script uses the resolved path
terraform() { "$TERRAFORM_BIN" "$@"; }

# ── Build provider ────────────────────────────────────────────────────────────
header "Building provider"
info "Running: go build -o $PROVIDER_BIN"
(cd "$REPO_DIR" && go build -o "$PROVIDER_BIN" .) || fail "go build failed"
pass "Provider binary: $PROVIDER_BIN"

# ── Write dev override config ─────────────────────────────────────────────────
cat > "$TFRC" <<EOF
provider_installation {
  dev_overrides {
    "CiscoDevNet/fmc" = "${SCRIPT_DIR}"
  }
  direct {}
}
EOF

# Export env vars consumed by the provider and Terraform
export TF_CLI_CONFIG_FILE="$TFRC"
export FMC_USERNAME FMC_PASSWORD FMC_URL
export FMC_INSECURE=true

# ── FMC API helpers ───────────────────────────────────────────────────────────
CURL=( curl -s -k )

# Authenticates with FMC and populates AUTH_TOKEN + DOMAIN_UUID from response headers
AUTH_HDR_FILE="$(mktemp)"
fmc_authenticate() {
  "${CURL[@]}" -D "$AUTH_HDR_FILE" -X POST "${FMC_URL}/api/fmc_platform/v1/auth/generatetoken" \
    --user "${FMC_USERNAME}:${FMC_PASSWORD}" \
    -H "Content-Type: application/json" \
    -o /dev/null
  AUTH_TOKEN="$(grep -i "^X-auth-access-token:" "$AUTH_HDR_FILE" | awk '{print $2}' | tr -d '\r\n' || true)"
  DOMAIN_UUID="$(grep -i "^DOMAIN_UUID:" "$AUTH_HDR_FILE" | awk '{print $2}' | tr -d '\r\n' || true)"
}

# Creates a host object; prints the full JSON response
fmc_create_host() {
  local tok="$1" domain="$2" name="$3" ip="$4"
  "${CURL[@]}" -X POST \
    "${FMC_URL}/api/fmc_config/v1/domain/${domain}/object/hosts" \
    -H "X-auth-access-token: ${tok}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${name}\",\"value\":\"${ip}\",\"type\":\"Host\"}"
}

# Creates a network group object with one literal network; prints the full JSON response
fmc_create_network_group() {
  local tok="$1" domain="$2" name="$3" cidr="$4"
  "${CURL[@]}" -X POST \
    "${FMC_URL}/api/fmc_config/v1/domain/${domain}/object/networkgroups" \
    -H "X-auth-access-token: ${tok}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${name}\",\"type\":\"NetworkGroup\",\"literals\":[{\"type\":\"Network\",\"value\":\"${cidr}\"}]}"
}

# Creates an access control policy; prints the full JSON response
fmc_create_acp() {
  local tok="$1" domain="$2" name="$3"
  "${CURL[@]}" -X POST \
    "${FMC_URL}/api/fmc_config/v1/domain/${domain}/policy/accesspolicies" \
    -H "X-auth-access-token: ${tok}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${name}\",\"type\":\"AccessPolicy\",\"defaultAction\":{\"action\":\"BLOCK\"}}"
}

# Creates an access category inside an ACP; prints the full JSON response
fmc_create_access_category() {
  local tok="$1" domain="$2" acp_id="$3" name="$4"
  "${CURL[@]}" -X POST \
    "${FMC_URL}/api/fmc_config/v1/domain/${domain}/policy/accesspolicies/${acp_id}/categories" \
    -H "X-auth-access-token: ${tok}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${name}\",\"type\":\"Category\"}"
}

# Creates an access rule inside an ACP; prints the full JSON response
fmc_create_access_rule() {
  local tok="$1" domain="$2" acp_id="$3" name="$4"
  "${CURL[@]}" -X POST \
    "${FMC_URL}/api/fmc_config/v1/domain/${domain}/policy/accesspolicies/${acp_id}/accessrules" \
    -H "X-auth-access-token: ${tok}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${name}\",\"action\":\"ALLOW\",\"type\":\"AccessRule\",\"enabled\":true}"
}

# Generic DELETE by URL path
fmc_delete() {
  local tok="$1" path="$2"
  "${CURL[@]}" -X DELETE \
    "${FMC_URL}${path}" \
    -H "X-auth-access-token: ${tok}" > /dev/null
}

# Extracts .id from a JSON string
json_id() { python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('id',''))"; }

# Extracts the Terraform resource ID from "terraform show -json" output
tf_resource_id() {
  local rtype="$1" rname="$2"
  terraform show -json 2>/dev/null \
  | python3 -c "
import sys, json
state = json.load(sys.stdin)
for r in state.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='${rtype}' and r.get('name')=='${rname}':
        print(r.get('values',{}).get('id',''))
        break
"
}

# ── Cleanup handler ───────────────────────────────────────────────────────────
# Tracks orphaned API-created objects that must be deleted if terraform destroy didn't run
EMERGENCY_CLEANUPS=()   # entries: "DELETE_PATH"

cleanup() {
  info "Running cleanup..."
  cd "$SCRIPT_DIR"

  # Destroy any Terraform-managed resources still in state
  if [[ -f terraform.tfstate ]]; then
    terraform destroy -auto-approve 2>/dev/null || true
  fi

  # Delete any API-created objects that terraform destroy did not clean up
  for path in "${EMERGENCY_CLEANUPS[@]:-}"; do
    [[ -z "$path" ]] && continue
    info "Deleting orphaned object at ${path}..."
    fmc_delete "$AUTH_TOKEN" "$path" && pass "Orphaned object deleted" || true
  done

  # Remove temp files
  rm -f terraform.tfstate terraform.tfstate.backup .terraform.lock.hcl \
        "$PROVIDER_BIN" "$TFRC" "$AUTH_HDR_FILE"
  rm -rf .terraform
}
trap cleanup EXIT

# Helper: run an idempotency test for one resource
#   $1  test label        e.g. "fmc_host idempotency"
#   $2  tf resource type  e.g. "fmc_host"
#   $3  tf resource name  e.g. "idempotency_test"
#   $4  pre-created ID    from the API call
#   $5  delete path       FMC REST path to delete the object on emergency cleanup
#   $6  apply target(s)   e.g. "-target=fmc_host.idempotency_test" (space-separated ok)
run_idempotency_test() {
  local label="$1" rtype="$2" rname="$3" pre_id="$4" del_path="$5"
  shift 5
  local targets=("$@")

  EMERGENCY_CLEANUPS+=("$del_path")

  info "terraform apply ${targets[*]} (object already exists — expecting idempotent ingest)..."
  # shellcheck disable=SC2086
  terraform apply "${targets[@]}" -auto-approve
  pass "terraform apply succeeded"

  info "Verifying Terraform state ID matches the pre-created object ID..."
  local tf_id
  tf_id=$(tf_resource_id "$rtype" "$rname")
  if [[ "$tf_id" == "$pre_id" ]]; then
    pass "State ID matches pre-created object ($tf_id)"
  else
    fail "ID mismatch: Terraform state='$tf_id', pre-created='$pre_id'"
  fi

  # Remove from emergency list — terraform destroy will clean it up
  EMERGENCY_CLEANUPS=("${EMERGENCY_CLEANUPS[@]/$del_path/}")

  info "terraform plan ${targets[*]} (expecting: no changes after ingest)..."
  terraform plan "${targets[@]}" -detailed-exitcode -out=/dev/null
  pass "No changes detected after ingest"

  info "terraform destroy ${targets[*]}..."
  terraform destroy "${targets[@]}" -auto-approve
  pass "${label} PASSED — existing object was ingested correctly"
}

# ── Authenticate ──────────────────────────────────────────────────────────────
header "Authenticating with FMC"
info "URL: $FMC_URL"
AUTH_TOKEN=""
DOMAIN_UUID=""
fmc_authenticate
[[ -z "$AUTH_TOKEN" ]] && fail "Could not obtain auth token — check credentials and URL"
pass "Auth token obtained"
[[ -z "$DOMAIN_UUID" ]] && fail "Could not obtain Global domain UUID from auth headers"
pass "Domain UUID: $DOMAIN_UUID"
rm -f "$AUTH_HDR_FILE"

cd "$SCRIPT_DIR"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 1 — Normal create/destroy regression (all resources)"
# Verifies that the basic create→plan→destroy workflow is still intact.
# ══════════════════════════════════════════════════════════════════════════════

info "terraform apply (fresh create of all resources)..."
terraform apply -auto-approve
pass "terraform apply succeeded"

info "terraform plan (expecting: no changes)..."
terraform plan -detailed-exitcode -out=/dev/null
pass "No changes detected after apply"

info "terraform destroy..."
terraform destroy -auto-approve
pass "TEST 1 PASSED — normal create/destroy works correctly"

# ── Re-authenticate before idempotency tests ──────────────────────────────────
info "Re-authenticating with FMC..."
fmc_authenticate
[[ -z "$AUTH_TOKEN" ]] && fail "Could not re-obtain auth token"
pass "Auth token refreshed"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 2 — fmc_host idempotency"
# ══════════════════════════════════════════════════════════════════════════════

info "Pre-creating host '${TEST_HOST_NAME}' (${TEST_HOST_IP}) via FMC REST API..."
API_RESPONSE=$(fmc_create_host "$AUTH_TOKEN" "$DOMAIN_UUID" "$TEST_HOST_NAME" "$TEST_HOST_IP")
PRE_CREATED_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$PRE_CREATED_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
pass "Object pre-created via API (ID: $PRE_CREATED_ID)"

run_idempotency_test \
  "TEST 2 — fmc_host" \
  "fmc_host" "idempotency_test" \
  "$PRE_CREATED_ID" \
  "/api/fmc_config/v1/domain/${DOMAIN_UUID}/object/hosts/${PRE_CREATED_ID}" \
  "-target=fmc_host.idempotency_test"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 3 — fmc_network_group idempotency"
# ══════════════════════════════════════════════════════════════════════════════

fmc_authenticate && [[ -n "$AUTH_TOKEN" ]] && pass "Auth token refreshed" || fail "Re-authentication failed"
info "Pre-creating network group 'tf-idempotency-test-netgrp' via FMC REST API..."
API_RESPONSE=$(fmc_create_network_group "$AUTH_TOKEN" "$DOMAIN_UUID" "tf-idempotency-test-netgrp" "10.99.0.0/24")
PRE_CREATED_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$PRE_CREATED_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
pass "Object pre-created via API (ID: $PRE_CREATED_ID)"

run_idempotency_test \
  "TEST 3 — fmc_network_group" \
  "fmc_network_group" "idempotency_test" \
  "$PRE_CREATED_ID" \
  "/api/fmc_config/v1/domain/${DOMAIN_UUID}/object/networkgroups/${PRE_CREATED_ID}" \
  "-target=fmc_network_group.idempotency_test"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 4 — fmc_access_category idempotency"
# ══════════════════════════════════════════════════════════════════════════════

info "Creating access control policy (prerequisite)..."
terraform apply -target=fmc_access_control_policy.test_acp -auto-approve
ACP_ID=$(tf_resource_id "fmc_access_control_policy" "test_acp")
[[ -z "$ACP_ID" ]] && fail "Could not get ACP ID from Terraform state"
pass "ACP created (ID: $ACP_ID)"

fmc_authenticate && [[ -n "$AUTH_TOKEN" ]] && pass "Auth token refreshed" || fail "Re-authentication failed"
info "Pre-creating access category 'tf-idempotency-test-cat' via FMC REST API..."
API_RESPONSE=$(fmc_create_access_category "$AUTH_TOKEN" "$DOMAIN_UUID" "$ACP_ID" "tf-idempotency-test-cat")
PRE_CREATED_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$PRE_CREATED_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
pass "Object pre-created via API (ID: $PRE_CREATED_ID)"

run_idempotency_test \
  "TEST 4 — fmc_access_category" \
  "fmc_access_category" "idempotency_test" \
  "$PRE_CREATED_ID" \
  "/api/fmc_config/v1/domain/${DOMAIN_UUID}/policy/accesspolicies/${ACP_ID}/categories/${PRE_CREATED_ID}" \
  "-target=fmc_access_category.idempotency_test"

info "Destroying access control policy..."
terraform destroy -target=fmc_access_control_policy.test_acp -auto-approve
pass "ACP destroyed"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 5 — fmc_access_rule idempotency"
# ══════════════════════════════════════════════════════════════════════════════

info "Creating access control policy (prerequisite)..."
terraform apply -target=fmc_access_control_policy.test_acp -auto-approve
ACP_ID=$(tf_resource_id "fmc_access_control_policy" "test_acp")
[[ -z "$ACP_ID" ]] && fail "Could not get ACP ID from Terraform state"
pass "ACP created (ID: $ACP_ID)"

fmc_authenticate && [[ -n "$AUTH_TOKEN" ]] && pass "Auth token refreshed" || fail "Re-authentication failed"
info "Pre-creating access rule 'tf-idempotency-test-rule' via FMC REST API..."
API_RESPONSE=$(fmc_create_access_rule "$AUTH_TOKEN" "$DOMAIN_UUID" "$ACP_ID" "tf-idempotency-test-rule")
PRE_CREATED_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$PRE_CREATED_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
pass "Object pre-created via API (ID: $PRE_CREATED_ID)"

run_idempotency_test \
  "TEST 5 — fmc_access_rule" \
  "fmc_access_rule" "idempotency_test" \
  "$PRE_CREATED_ID" \
  "/api/fmc_config/v1/domain/${DOMAIN_UUID}/policy/accesspolicies/${ACP_ID}/accessrules/${PRE_CREATED_ID}" \
  "-target=fmc_access_rule.idempotency_test"

info "Destroying access control policy..."
terraform destroy -target=fmc_access_control_policy.test_acp -auto-approve
pass "ACP destroyed"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 6 — fmc_network_groups (bulk) idempotency"
# Pre-create both network groups via API, then verify the bulk resource ingests them.
# ══════════════════════════════════════════════════════════════════════════════

fmc_authenticate && [[ -n "$AUTH_TOKEN" ]] && pass "Auth token refreshed" || fail "Re-authentication failed"

info "Pre-creating network group 'tf-idempotency-test-ng1' via FMC REST API..."
API_RESPONSE=$(fmc_create_network_group "$AUTH_TOKEN" "$DOMAIN_UUID" "tf-idempotency-test-ng1" "10.88.0.0/24")
NG1_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$NG1_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
EMERGENCY_CLEANUPS+=("/api/fmc_config/v1/domain/${DOMAIN_UUID}/object/networkgroups/${NG1_ID}")
pass "ng1 pre-created (ID: $NG1_ID)"

info "Pre-creating network group 'tf-idempotency-test-ng2' via FMC REST API..."
API_RESPONSE=$(fmc_create_network_group "$AUTH_TOKEN" "$DOMAIN_UUID" "tf-idempotency-test-ng2" "10.88.1.0/24")
NG2_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$NG2_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
EMERGENCY_CLEANUPS+=("/api/fmc_config/v1/domain/${DOMAIN_UUID}/object/networkgroups/${NG2_ID}")
pass "ng2 pre-created (ID: $NG2_ID)"

info "terraform apply -target=fmc_network_groups.idempotency_test (expecting idempotent ingest)..."
terraform apply -target=fmc_network_groups.idempotency_test -auto-approve
pass "terraform apply succeeded"

info "Verifying Terraform state IDs match the pre-created object IDs..."
TF_STATE_JSON=$(terraform show -json 2>/dev/null)
TF_NG1_ID=$(echo "$TF_STATE_JSON" | python3 -c "
import sys,json
s=json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_network_groups' and r.get('name')=='idempotency_test':
        print(r.get('values',{}).get('items',{}).get('tf-idempotency-test-ng1',{}).get('id',''))
" 2>/dev/null || true)
TF_NG2_ID=$(echo "$TF_STATE_JSON" | python3 -c "
import sys,json
s=json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_network_groups' and r.get('name')=='idempotency_test':
        print(r.get('values',{}).get('items',{}).get('tf-idempotency-test-ng2',{}).get('id',''))
" 2>/dev/null || true)

[[ "$TF_NG1_ID" == "$NG1_ID" ]] && pass "ng1 state ID matches ($TF_NG1_ID)" || fail "ng1 ID mismatch: state='$TF_NG1_ID', pre-created='$NG1_ID'"
[[ "$TF_NG2_ID" == "$NG2_ID" ]] && pass "ng2 state ID matches ($TF_NG2_ID)" || fail "ng2 ID mismatch: state='$TF_NG2_ID', pre-created='$NG2_ID'"
EMERGENCY_CLEANUPS=("${EMERGENCY_CLEANUPS[@]//*networkgroups*}")

info "terraform plan -target=fmc_network_groups.idempotency_test (expecting: no changes)..."
terraform plan -target=fmc_network_groups.idempotency_test -detailed-exitcode -out=/dev/null
pass "No changes detected after ingest"

info "terraform destroy -target=fmc_network_groups.idempotency_test..."
terraform destroy -target=fmc_network_groups.idempotency_test -auto-approve
pass "TEST 6 — fmc_network_groups PASSED — existing objects were ingested correctly"

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 7 — fmc_access_rules (bulk) idempotency"
# Pre-create both rules via API, then verify the bulk resource ingests them.
# ══════════════════════════════════════════════════════════════════════════════

info "Creating access control policy (prerequisite)..."
terraform apply -target=fmc_access_control_policy.test_acp2 -auto-approve
ACP2_ID=$(tf_resource_id "fmc_access_control_policy" "test_acp2")
[[ -z "$ACP2_ID" ]] && fail "Could not get ACP2 ID from Terraform state"
pass "ACP2 created (ID: $ACP2_ID)"

fmc_authenticate && [[ -n "$AUTH_TOKEN" ]] && pass "Auth token refreshed" || fail "Re-authentication failed"

info "Pre-creating access rule 'tf-idempotency-test-bulk-rule1' via FMC REST API..."
API_RESPONSE=$(fmc_create_access_rule "$AUTH_TOKEN" "$DOMAIN_UUID" "$ACP2_ID" "tf-idempotency-test-bulk-rule1")
RULE1_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$RULE1_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
EMERGENCY_CLEANUPS+=("/api/fmc_config/v1/domain/${DOMAIN_UUID}/policy/accesspolicies/${ACP2_ID}/accessrules/${RULE1_ID}")
pass "rule1 pre-created (ID: $RULE1_ID)"

info "Pre-creating access rule 'tf-idempotency-test-bulk-rule2' via FMC REST API..."
API_RESPONSE=$(fmc_create_access_rule "$AUTH_TOKEN" "$DOMAIN_UUID" "$ACP2_ID" "tf-idempotency-test-bulk-rule2")
RULE2_ID=$(echo "$API_RESPONSE" | json_id)
[[ -z "$RULE2_ID" ]] && fail "API pre-creation failed. Response: $API_RESPONSE"
EMERGENCY_CLEANUPS+=("/api/fmc_config/v1/domain/${DOMAIN_UUID}/policy/accesspolicies/${ACP2_ID}/accessrules/${RULE2_ID}")
pass "rule2 pre-created (ID: $RULE2_ID)"

info "terraform apply -target=fmc_access_rules.idempotency_test (expecting idempotent ingest)..."
terraform apply -target=fmc_access_rules.idempotency_test -auto-approve
pass "terraform apply succeeded"

info "Verifying Terraform state IDs match the pre-created rule IDs..."
TF_STATE_JSON=$(terraform show -json 2>/dev/null)
TF_RULE1_ID=$(echo "$TF_STATE_JSON" | python3 -c "
import sys,json
s=json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_access_rules' and r.get('name')=='idempotency_test':
        items=r.get('values',{}).get('items',[])
        if items: print(items[0].get('id',''))
" 2>/dev/null || true)
TF_RULE2_ID=$(echo "$TF_STATE_JSON" | python3 -c "
import sys,json
s=json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_access_rules' and r.get('name')=='idempotency_test':
        items=r.get('values',{}).get('items',[])
        if len(items)>1: print(items[1].get('id',''))
" 2>/dev/null || true)

[[ "$TF_RULE1_ID" == "$RULE1_ID" ]] && pass "rule1 state ID matches ($TF_RULE1_ID)" || fail "rule1 ID mismatch: state='$TF_RULE1_ID', pre-created='$RULE1_ID'"
[[ "$TF_RULE2_ID" == "$RULE2_ID" ]] && pass "rule2 state ID matches ($TF_RULE2_ID)" || fail "rule2 ID mismatch: state='$TF_RULE2_ID', pre-created='$RULE2_ID'"
EMERGENCY_CLEANUPS=("${EMERGENCY_CLEANUPS[@]//*accessrules*}")

info "terraform plan -target=fmc_access_rules.idempotency_test (expecting: no changes)..."
terraform plan -target=fmc_access_rules.idempotency_test -detailed-exitcode -out=/dev/null
pass "No changes detected after ingest"

info "terraform destroy -target=fmc_access_rules.idempotency_test -target=fmc_access_control_policy.test_acp2..."
terraform destroy -target=fmc_access_rules.idempotency_test -target=fmc_access_control_policy.test_acp2 -auto-approve
pass "TEST 7 — fmc_access_rules PASSED — existing rules were ingested correctly"

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}${BOLD}══════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  All tests passed!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════${NC}"
