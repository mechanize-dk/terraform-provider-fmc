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
# shellcheck source=../lib/fmc_api.sh
source "$SCRIPT_DIR/../lib/fmc_api.sh"

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

# Test objects are recognised by name prefix.
TEST_PREFIXES="tf-bnt- tf-bulk-nat-test"
LEFTOVER_ENDPOINTS="policy/ftdnatpolicies object/hosts object/networks object/securityzones"

# Reads an FMC list response on stdin; prints "<id> <name>" for test objects.
test_items() {
  python3 -c "
import sys, json
prefixes = tuple(sys.argv[1].split())
for it in json.load(sys.stdin).get('items', []):
    if it.get('name', '').startswith(prefixes):
        print(it['id'], it['name'])
" "$TEST_PREFIXES"
}

# Prints "<endpoint> <id> <name>" for every test object still on the FMC.
list_leftovers() {
  local endpoint items
  for endpoint in $LEFTOVER_ENDPOINTS; do
    items=$(fmc_api GET "/$endpoint?limit=1000&expanded=true" | test_items) || return 1
    if [[ -n "$items" ]]; then
      sed "s|^|$endpoint |" <<< "$items"
    fi
  done
  return 0
}

unassign_body() {
  echo "{\"type\":\"PolicyAssignment\",\"policy\":{\"id\":\"$1\",\"type\":\"FTDNatPolicy\"},\"targets\":[]}"
}

# Pre-flight: if a previous run was killed mid-cleanup, the FMC may still
# carry test objects. Wipe them via the API so the apply below starts clean.
# Policies first: their rules are what keep the hosts and zones in use.
preflight_cleanup() {
  local leftovers endpoint id name
  leftovers=$(list_leftovers) || fail "pre-flight: could not list FMC objects"
  while read -r endpoint id name; do
    [[ "$endpoint" == "policy/ftdnatpolicies" ]] || continue
    info "pre-flight: removing NAT policy $name"
    # PUT with empty targets is the supported unassign (DELETE returns 405);
    # 404 = the policy was never assigned.
    fmc_api --ok 404 PUT "/assignment/policyassignments/$id" "$(unassign_body "$id")" > /dev/null \
      || fail "pre-flight: could not unassign NAT policy $name"
    fmc_api --ok 404 DELETE "/policy/ftdnatpolicies/$id" > /dev/null \
      || fail "pre-flight: could not delete NAT policy $name"
  done <<< "$leftovers"
  while read -r endpoint id name; do
    [[ -z "$endpoint" || "$endpoint" == "policy/ftdnatpolicies" ]] && continue
    info "pre-flight: removing $endpoint $name"
    fmc_api --ok 404 DELETE "/$endpoint/$id" > /dev/null \
      || fail "pre-flight: could not delete $endpoint $name"
  done <<< "$leftovers"
}

# Runs on every exit. Each step reports its own failure instead of hiding it,
# and the run only counts as passed if no test object is left on the FMC.
cleanup() {
  local rc=$? ok=1 out leftovers
  info "Cleaning up..."
  cd "$SCRIPT_DIR"
  if [[ -f terraform.tfstate ]]; then
    # destroy of fmc_ftd_nat_policy fails (HTTP 400) while the policy is
    # still assigned to a device, so unassign it first.
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
      fmc_api --ok 404 PUT "/assignment/policyassignments/${NATP_FOR_CLEANUP}" \
        "$(unassign_body "$NATP_FOR_CLEANUP")" > /dev/null || ok=0
    fi
    if ! out=$(terraform destroy -auto-approve -no-color 2>&1); then
      echo "$out" | tail -20
      ok=0
    fi
  fi
  rm -f terraform.tfstate terraform.tfstate.backup .terraform.lock.hcl terraform.tfvars "$PROVIDER_BIN" "$TFRC"
  rm -rf .terraform
  if ! leftovers=$(list_leftovers); then
    ok=0
  elif [[ -n "$leftovers" ]]; then
    echo -e "${RED}Test objects left on the FMC:${NC}"
    echo "$leftovers"
    ok=0
  fi
  if [[ $ok == 1 ]]; then
    pass "Cleanup verified: no test objects left on the FMC"
  else
    echo -e "${RED}✗ Cleanup failed — see above. The next run's pre-flight removes leftovers.${NC}"
    [[ $rc -ne 0 ]] || rc=1
  fi
  exit $rc
}
trap cleanup EXIT

# Asserts that a rule added to an EXISTING bulk resource (Update path) got
# match_on's zone on FMC. Before the plan-time fill these rules were POSTed
# without sourceInterface and plan still reported no changes.
# Usage: assert_rule_zone <rtype> <rname> <list> <key> <endpoint> <want_zone_id> <label>
assert_rule_zone() {
  local rtype="$1" rname="$2" list="$3" key="$4" endpoint="$5" want="$6" label="$7"
  local rid natp got
  rid=$(terraform show -json | python3 -c "
import sys, json
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='${rtype}' and r.get('name')=='${rname}':
        v = r.get('values',{}).get('${list}')
        for k, it in (v.items() if isinstance(v, dict) else ((i.get('key'), i) for i in v)):
            if k == '${key}': print(it.get('id',''))
")
  natp=$(terraform show -json | python3 -c "
import sys, json
for r in json.load(sys.stdin).get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_ftd_nat_policy' and r.get('name')=='shared': print(r['values']['id'])
")
  [[ -z "$rid" ]] && fail "$label: no state id for ${rname}.${list}[${key}]"
  got=$(fmc_api GET "/policy/ftdnatpolicies/${natp}/${endpoint}/${rid}" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('sourceInterface',{}).get('id',''))") \
    || fail "$label: could not read rule $rid from FMC"
  [[ "$got" == "$want" ]] || fail "$label: rule $rid has sourceInterface '$got', want '$want'"
  pass "$label: added rule carries match_on zone"
}

info "Pre-flight cleanup..."
preflight_cleanup
pass "Pre-flight done"

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
DEV_RESP=$(fmc_api GET "/devices/devicerecords?limit=1000&expanded=true") \
  || fail "SCENARIO 2: could not list devices"
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

fmc_api POST "/assignment/policyassignments" \
  "{\"type\":\"PolicyAssignment\",\"policy\":{\"id\":\"${NATP_ID}\",\"type\":\"FTDNatPolicy\"},\"targets\":[{\"id\":\"${DEVICE_ID}\",\"type\":\"Device\",\"name\":\"${FMC_FTDV_NAME}\"}]}" \
  > /dev/null || fail "SCENARIO 2: could not assign NAT policy to '$FMC_FTDV_NAME'"
pass "NAT policy assignment in place"

info "Pre-creating a tenant_a-shaped manual NAT rule via API..."
PRECREATE_RESP=$(fmc_api POST "/policy/ftdnatpolicies/${NATP_ID}/manualnatrules?section=before_auto" \
  "{\"type\":\"FTDManualNatRule\",\"natType\":\"STATIC\",\"originalSource\":{\"id\":\"${SRC_A}\"},\"translatedSource\":{\"id\":\"${TRANS_A}\"},\"sourceInterface\":{\"id\":\"${ZONE_A}\"}}") \
  || fail "SCENARIO 2: pre-create failed"
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

# ─── SCENARIO 3 — reorder ────────────────────────────────────────────────────
header "SCENARIO 3 — reorder"

# Back up main.tf so we can mutate-and-restore.
cp main.tf /tmp/bnt_main_backup.tf
restore_main_tf() { cp /tmp/bnt_main_backup.tf "$SCRIPT_DIR/main.tf"; }
# Ensure restoration even on early exit.
trap 'restore_main_tf; cleanup' EXIT

info "Adding rule_2 to tenant_a..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
needle = '''  before_auto = [
    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
  ]
}

resource "fmc_mze_manual_nat_rules" "tenant_b"'''
replacement = '''  before_auto = [
    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
    {
      key                  = "rule_2"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.trans_a.id
      translated_source_id = fmc_host.src_a.id
    },
  ]
}

resource "fmc_mze_manual_nat_rules" "tenant_b"'''
if needle not in s: raise SystemExit("scenario 3: anchor block not found in main.tf")
p.write_text(s.replace(needle, replacement, 1))
PY
terraform apply -auto-approve > /dev/null
pass "rule_2 added"
assert_rule_zone fmc_mze_manual_nat_rules tenant_a before_auto rule_2 manualnatrules "$ZONE_A" "SCENARIO 3"

info "Swapping rule_1 and rule_2 positions..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
old = '''    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
    {
      key                  = "rule_2"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.trans_a.id
      translated_source_id = fmc_host.src_a.id
    },'''
new = '''    {
      key                  = "rule_2"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.trans_a.id
      translated_source_id = fmc_host.src_a.id
    },
    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },'''
if old not in s: raise SystemExit("scenario 3: swap anchor not found")
p.write_text(s.replace(old, new, 1))
PY
terraform apply -auto-approve > /dev/null
pass "Reorder applied"

# Verify state shows rule_2 first, rule_1 second. Use a Python-side
# comparison (not bash string equality) to dodge CR/LF differences on MSYS.
ORDER_OK=$(terraform show -json | python3 -c "
import sys, json
want = ['rule_2', 'rule_1']
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_mze_manual_nat_rules' and r.get('name')=='tenant_a':
        got = [it.get('key','') for it in r.get('values',{}).get('before_auto',[])]
        print('OK' if got == want else 'got={} want={}'.format(got, want))
        break
")
if [[ "$ORDER_OK" == "OK" ]]; then
  pass "SCENARIO 3 CONFIRMED: state reflects swapped order (rule_2 first, rule_1 second)"
else
  fail "SCENARIO 3: state order mismatch — $ORDER_OK"
fi

restore_main_tf
info "Re-applying baseline config to reconcile state for subsequent scenarios..."
terraform apply -auto-approve > /dev/null
trap cleanup EXIT  # restore normal cleanup-only trap

# ─── SCENARIO 4 — out-of-band rule coexistence ───────────────────────────────
header "SCENARIO 4 — out-of-band rule coexistence"

NATP_ID=$(get_state_id fmc_ftd_nat_policy shared)
ZONE_A=$(get_state_id fmc_security_zone tenant_a)
SRC_A=$(get_state_id fmc_host src_a)
SRC_B=$(get_state_id fmc_host src_b)
TRANS_B=$(get_state_id fmc_host trans_b)

# OOB rule uses src_b/trans_b (distinct from tenant_a's rule) and lives in
# tenant_a's source zone — so it matches tenant_a's match_on but isn't in
# tenant_a's items list. If cooperative ownership works, tenant_a will
# ignore it.
info "Creating an out-of-band rule in tenant_a's zone via API..."
OOB_RESP=$(fmc_api POST "/policy/ftdnatpolicies/${NATP_ID}/manualnatrules?section=before_auto" \
  "{\"type\":\"FTDManualNatRule\",\"natType\":\"STATIC\",\"originalSource\":{\"id\":\"${SRC_B}\"},\"translatedSource\":{\"id\":\"${TRANS_B}\"},\"sourceInterface\":{\"id\":\"${ZONE_A}\"},\"description\":\"out-of-band rule for SCENARIO 4\"}") \
  || fail "SCENARIO 4: OOB pre-create failed"
OOB_ID=$(echo "$OOB_RESP" | python3 -c "import sys,json;print(json.loads(sys.stdin.read()).get('id',''))")
[[ -z "$OOB_ID" ]] && fail "OOB pre-create failed: $OOB_RESP"
pass "OOB rule id: $OOB_ID"

info "terraform plan -detailed-exitcode (expect 0 = no drift, cooperative ownership)..."
if terraform plan -detailed-exitcode -out=/dev/null; then
  pass "SCENARIO 4 CONFIRMED: OOB rule ignored (no drift)"
else
  fail "SCENARIO 4: terraform plan detected drift on OOB rule (cooperative ownership broken)"
fi

info "Cleanup: delete OOB rule..."
fmc_api DELETE "/policy/ftdnatpolicies/${NATP_ID}/manualnatrules/${OOB_ID}" > /dev/null \
  || fail "SCENARIO 4: could not delete OOB rule $OOB_ID"

# ─── SCENARIO 5 — auto-NAT specificity ───────────────────────────────────────
header "SCENARIO 5 — auto-NAT specificity"

# Add a Network-typed auto-NAT entry alongside the existing Host-typed one.
# FMC will reorder them by specificity (Host before Network); our resource's
# Read should refresh by stored ID, ignoring FMC's position.
cp main.tf /tmp/bnt_main_backup.tf
trap 'restore_main_tf; cleanup' EXIT

info "Adding a Network-typed auto-NAT entry to tenant_a_auto..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
# Insert a fmc_network resource and extend the auto-NAT map.
if 'fmc_network "net_a"' not in s:
    s = s.replace(
        'resource "fmc_host" "trans_b" {',
        '''resource "fmc_network" "net_a" {
  name   = "tf-bnt-net-a"
  prefix = "10.30.0.0/24"
}

resource "fmc_host" "trans_b" {''', 1)
needle = '''  rules = {
    "obj_a" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_host.src_a.id
      translated_network_id = fmc_host.trans_a.id
    }
  }
}'''
replacement = '''  rules = {
    "obj_a" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_host.src_a.id
      translated_network_id = fmc_host.trans_a.id
    }
    "obj_a_net" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_network.net_a.id
      translated_network_id = fmc_host.trans_a.id
    }
  }
}'''
if needle not in s: raise SystemExit("scenario 5: anchor for tenant_a_auto rules not found")
p.write_text(s.replace(needle, replacement, 1))
PY
terraform apply -auto-approve > /dev/null
pass "Network rule added"
assert_rule_zone fmc_mze_auto_nat_rules tenant_a_auto rules obj_a_net autonatrules "$ZONE_A" "SCENARIO 5"

info "terraform plan -detailed-exitcode (expect 0 = no drift even though FMC reorders by specificity)..."
if terraform plan -detailed-exitcode -out=/dev/null; then
  pass "SCENARIO 5 CONFIRMED: Read by stored ID is order-independent"
else
  fail "SCENARIO 5: plan detected drift — Read should be order-independent"
fi

# Two-step revert: FMC blocks deletion of a network object that is still
# referenced by a NAT policy, so we must remove the obj_a_net auto-NAT
# entry from tenant_a_auto BEFORE removing fmc_network.net_a from HCL.
info "Step 1/2: removing obj_a_net entry while keeping fmc_network.net_a (drops the auto-NAT rule)..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
old = '''    "obj_a_net" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_network.net_a.id
      translated_network_id = fmc_host.trans_a.id
    }
'''
if old in s:
    p.write_text(s.replace(old, '', 1))
PY
terraform apply -auto-approve > /dev/null

info "Step 2/2: restoring baseline main.tf (drops fmc_network.net_a)..."
restore_main_tf
terraform apply -auto-approve > /dev/null
trap cleanup EXIT

# ─── helper: reset to baseline between scenarios that mutate main.tf ─────────
# Restores main.tf from the snapshot and re-applies so state matches config.
reset_to_baseline() {
  cp /tmp/bnt_main_backup.tf "$SCRIPT_DIR/main.tf"
  terraform apply -auto-approve > /dev/null
}

# Convenience accessor for item ids out of the live state.
mze_item_id() {
  local rtype="$1" rname="$2" key="$3" list="$4"  # list = before_auto|after_auto|rules
  terraform show -json | python3 -c "
import sys, json
rtype, rname, key, list_ = '${rtype}', '${rname}', '${key}', '${list}'
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')==rtype and r.get('name')==rname:
        v = r.get('values',{}).get(list_)
        if isinstance(v, list):
            for it in v:
                if it.get('key')==key:
                    print(it.get('id','')); break
        elif isinstance(v, dict):
            it = v.get(key, {})
            print(it.get('id',''))
        break
"
}

# ─── SCENARIO 6 — ImportState (smoke test) ──────────────────────────────────
# Verifies the ImportState code path runs and sets id + ftd_nat_policy_id.
# Does NOT assert convergence of items — that's S2's job (Patch 11 adoption).
header "SCENARIO 6 — ImportState"

ORIG_ID=$(get_state_id fmc_mze_manual_nat_rules tenant_a)
NATP_FOR_S6=$(get_state_id fmc_ftd_nat_policy shared)
[[ -z "$ORIG_ID" ]] && fail "SCENARIO 6: prerequisite missing (orig_id)"

info "Removing tenant_a from state (FMC rule remains)..."
terraform state rm fmc_mze_manual_nat_rules.tenant_a > /dev/null

info "Importing tenant_a via 'terraform import' with synthetic id $ORIG_ID..."
terraform import fmc_mze_manual_nat_rules.tenant_a "$ORIG_ID" > /dev/null

NEW_ID=$(get_state_id fmc_mze_manual_nat_rules tenant_a)
NEW_FTD=$(terraform show -json | python3 -c "
import sys, json
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_mze_manual_nat_rules' and r.get('name')=='tenant_a':
        print(r.get('values',{}).get('ftd_nat_policy_id',''))
        break
")
if [[ "$NEW_ID" == "$ORIG_ID" && "$NEW_FTD" == "$NATP_FOR_S6" ]]; then
  pass "SCENARIO 6 CONFIRMED: ImportState set synthetic id ($NEW_ID) + ftd_nat_policy_id"
else
  fail "SCENARIO 6: import incomplete — orig=$ORIG_ID new=$NEW_ID expected_natp=$NATP_FOR_S6 got_natp=$NEW_FTD"
fi

# Re-apply so subsequent scenarios start from a fully-tracked state.
# (Patch 11 recovery may or may not converge depending on FMC's pending-
# state semantics for a still-present rule; either way state ends up
# pointing at *some* tenant_a rule_1, and we'll reset_to_baseline before
# the next scenario.)
terraform apply -auto-approve > /dev/null 2>&1 || true
reset_to_baseline 2>&1 | tail -3 || true

# ─── SCENARIO 7 — Auto-NAT duplicate-recovery (FindDuplicateAutoNatRule) ─────
header "SCENARIO 7 — auto-NAT duplicate-recovery end-to-end"

NATP_ID=$(get_state_id fmc_ftd_nat_policy shared)
SRC_A=$(get_state_id fmc_host src_a)
TRANS_A=$(get_state_id fmc_host trans_a)

PRE_AUTO_ID=$(mze_item_id fmc_mze_auto_nat_rules tenant_a_auto obj_a rules)
[[ -z "$PRE_AUTO_ID" ]] && fail "SCENARIO 7: could not read tenant_a_auto obj_a state id"

info "Destroying tenant_a_auto so we can probe the duplicate-recovery path..."
terraform destroy -auto-approve -target=fmc_mze_auto_nat_rules.tenant_a_auto > /dev/null

info "Pre-creating an auto-NAT rule (obj_a content) via API..."
PRE_AUTO_RESP=$(fmc_api POST "/policy/ftdnatpolicies/${NATP_ID}/autonatrules" \
  "{\"type\":\"FTDAutoNatRule\",\"natType\":\"STATIC\",\"originalNetwork\":{\"id\":\"${SRC_A}\",\"type\":\"Host\"},\"translatedNetwork\":{\"id\":\"${TRANS_A}\",\"type\":\"Host\"}}") \
  || fail "SCENARIO 7: pre-create failed"
PRE_AUTO_NEW_ID=$(echo "$PRE_AUTO_RESP" | python3 -c "import sys,json;print(json.loads(sys.stdin.read()).get('id',''))")
[[ -z "$PRE_AUTO_NEW_ID" ]] && fail "SCENARIO 7 pre-create failed: $PRE_AUTO_RESP"
pass "Auto-NAT rule pre-created (id=$PRE_AUTO_NEW_ID)"

info "terraform apply -target=fmc_mze_auto_nat_rules.tenant_a_auto (expect FindDuplicateAutoNatRule to adopt it)..."
terraform apply -auto-approve -target=fmc_mze_auto_nat_rules.tenant_a_auto > /dev/null

ADOPTED_ID=$(mze_item_id fmc_mze_auto_nat_rules tenant_a_auto obj_a rules)
if [[ "$ADOPTED_ID" == "$PRE_AUTO_NEW_ID" ]]; then
  pass "SCENARIO 7 CONFIRMED: auto-NAT dedup-recovery adopted the pre-created rule ($ADOPTED_ID)"
else
  fail "SCENARIO 7: auto-NAT recovery failed — pre-created=$PRE_AUTO_NEW_ID state=$ADOPTED_ID"
fi

# Re-apply full config so other auto-NAT items (if any) come back, then continue.
terraform apply -auto-approve > /dev/null

# ─── SCENARIO 8 — Cross-section move (BEFORE_AUTO -> AFTER_AUTO) ────────────
header "SCENARIO 8 — cross-section move"

cp main.tf /tmp/bnt_main_backup.tf
trap 'cp /tmp/bnt_main_backup.tf "$SCRIPT_DIR/main.tf"; cleanup' EXIT

# Pre-record the current rule_1 ID so we can verify FMC gave it a NEW id on
# the move (FMC re-IDs rules on cross-section moves — see CLAUDE.md).
PRE_MOVE_ID=$(mze_item_id fmc_mze_manual_nat_rules tenant_a rule_1 before_auto)

info "Moving rule_1 from before_auto to after_auto..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
needle = '''resource "fmc_mze_manual_nat_rules" "tenant_a" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.shared.id
  match_on = {
    source_interface_id = fmc_security_zone.tenant_a.id
  }
  before_auto = [
    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
  ]
}'''
replacement = '''resource "fmc_mze_manual_nat_rules" "tenant_a" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.shared.id
  match_on = {
    source_interface_id = fmc_security_zone.tenant_a.id
  }
  before_auto = []
  after_auto = [
    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
  ]
}'''
if needle not in s: raise SystemExit("scenario 8: anchor block not found")
p.write_text(s.replace(needle, replacement, 1))
PY
terraform apply -auto-approve > /dev/null
pass "Cross-section move applied"

POST_MOVE_BEFORE=$(terraform show -json | python3 -c "
import sys, json
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_mze_manual_nat_rules' and r.get('name')=='tenant_a':
        b = r.get('values',{}).get('before_auto') or []
        a = r.get('values',{}).get('after_auto') or []
        print('before_count=%d after_count=%d' % (len(b), len(a)))
        if a: print('after_key=%s after_id=%s' % (a[0].get('key'), a[0].get('id')))
        break
")
POST_MOVE_ID=$(mze_item_id fmc_mze_manual_nat_rules tenant_a rule_1 after_auto)
if echo "$POST_MOVE_BEFORE" | grep -q 'before_count=0 after_count=1' && \
   echo "$POST_MOVE_BEFORE" | grep -q 'after_key=rule_1' && \
   [[ -n "$POST_MOVE_ID" ]]; then
  if [[ "$POST_MOVE_ID" != "$PRE_MOVE_ID" ]]; then
    pass "SCENARIO 8 CONFIRMED: rule moved to AFTER_AUTO with new FMC id ($PRE_MOVE_ID -> $POST_MOVE_ID)"
  else
    fail "SCENARIO 8: rule moved sections but FMC id unchanged ($POST_MOVE_ID) — FMC should re-id on cross-section moves"
  fi
else
  fail "SCENARIO 8: unexpected state after move — $POST_MOVE_BEFORE"
fi

reset_to_baseline
trap cleanup EXIT

# ─── SCENARIO 9 — PUT path in Update (modified ∧ ¬reordered) ────────────────
header "SCENARIO 9 — in-place PUT (body change, position stable)"

cp main.tf /tmp/bnt_main_backup.tf
trap 'cp /tmp/bnt_main_backup.tf "$SCRIPT_DIR/main.tf"; cleanup' EXIT

PRE_PUT_ID=$(mze_item_id fmc_mze_manual_nat_rules tenant_a rule_1 before_auto)

info "Adding a description to rule_1 without changing position..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
needle = '''      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
  ]
}

resource "fmc_mze_manual_nat_rules" "tenant_b"'''
replacement = '''      key                  = "rule_1"
      description          = "scenario 9 in-place PUT marker"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
  ]
}

resource "fmc_mze_manual_nat_rules" "tenant_b"'''
if needle not in s: raise SystemExit("scenario 9: anchor not found")
p.write_text(s.replace(needle, replacement, 1))
PY
terraform apply -auto-approve > /dev/null

POST_PUT_ID=$(mze_item_id fmc_mze_manual_nat_rules tenant_a rule_1 before_auto)
DESC=$(terraform show -json | python3 -c "
import sys, json
s = json.load(sys.stdin)
for r in s.get('values',{}).get('root_module',{}).get('resources',[]):
    if r.get('type')=='fmc_mze_manual_nat_rules' and r.get('name')=='tenant_a':
        for it in r.get('values',{}).get('before_auto', []):
            if it.get('key')=='rule_1':
                print(it.get('description',''))
                break
        break
")
if [[ "$POST_PUT_ID" == "$PRE_PUT_ID" && "$DESC" == "scenario 9 in-place PUT marker" ]]; then
  pass "SCENARIO 9 CONFIRMED: PUT preserved FMC id ($POST_PUT_ID) while updating description"
else
  fail "SCENARIO 9: PUT semantics broken — pre=$PRE_PUT_ID post=$POST_PUT_ID desc='$DESC'"
fi

reset_to_baseline
trap cleanup EXIT

# ─── SCENARIO 10 — match_on conflict surfaces as plan-time error ────────────
header "SCENARIO 10 — match_on validation error path"

cp main.tf /tmp/bnt_main_backup.tf
trap 'cp /tmp/bnt_main_backup.tf "$SCRIPT_DIR/main.tf"; cleanup' EXIT

info "Adding a rule to tenant_a with source_interface_id pointing at tenant_b's zone (conflict)..."
python3 - <<'PY'
import pathlib
p = pathlib.Path("main.tf")
s = p.read_text()
needle = '''      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
  ]
}

resource "fmc_mze_manual_nat_rules" "tenant_b"'''
replacement = '''      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
    },
    {
      key                  = "rule_bad"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_a.id
      translated_source_id = fmc_host.trans_a.id
      source_interface_id  = fmc_security_zone.tenant_b.id
    },
  ]
}

resource "fmc_mze_manual_nat_rules" "tenant_b"'''
if needle not in s: raise SystemExit("scenario 10: anchor not found")
p.write_text(s.replace(needle, replacement, 1))
PY

info "terraform apply (expect failure with match_on conflict message)..."
APPLY_OUT=$(terraform apply -auto-approve 2>&1 || true)
# Terraform wraps error messages across lines; strip the line-prefixes and
# concatenate so multi-line phrases like "conflicts with match_on" match.
NORMALIZED=$(echo "$APPLY_OUT" | sed 's/^[│|│] //' | tr '\n' ' ')
if echo "$NORMALIZED" | grep -q 'conflicts with .*match_on'; then
  pass "SCENARIO 10 CONFIRMED: match_on conflict surfaced as resource error"
else
  fail "SCENARIO 10: expected 'conflicts with match_on' in apply output, got: $(echo "$APPLY_OUT" | tail -5)"
fi

reset_to_baseline
trap cleanup EXIT

echo
echo -e "${GREEN}${BOLD}══════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  All scenarios passed.${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════${NC}"
