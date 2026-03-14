#!/usr/bin/env bash
# run_test.sh — diagnostic tests for fmc_network_groups_safe
#
# Tests whether removing a network group that is still referenced by access rules
# breaks terraform apply when using fmc_network_groups (expected), and succeeds
# gracefully using fmc_network_groups_safe (expected).
#
# Tests:
#   1. fmc_network_groups   — remove one group → confirm BREAKS
#   2. fmc_network_groups   — remove all groups → confirm BREAKS
#   3. fmc_network_groups_safe — remove one group → confirm SUCCEEDS + GC cleans up
#   4. fmc_network_groups_safe — remove all groups → confirm SUCCEEDS + GC cleans up
#   5. fmc_network_groups_safe — nested groups (group-a child of group-b) → confirm SUCCEEDS
#
# Usage:
#   ./run_test.sh -u <username> -p <password> --url <fmc_url> [--terraform /path/to/terraform]

set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
pass()   { echo -e "${GREEN}✓ $*${NC}"; }
fail()   { echo -e "${RED}✗ $*${NC}"; OVERALL_FAIL=1; }
info()   { echo -e "${YELLOW}→ $*${NC}"; }
header() { echo -e "\n${BOLD}══ $* ══${NC}"; }

OVERALL_FAIL=0

# ── Argument parsing ───────────────────────────────────────────────────────────
FMC_USERNAME=""; FMC_PASSWORD=""; FMC_URL=""; TERRAFORM_BIN=""

usage() {
  echo "Usage: $0 -u <username> -p <password> --url <fmc_url> [--terraform /path/to/terraform]"
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

FMC_URL="${FMC_URL%/}"

# ── Paths ──────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"
PROVIDER_BIN="$SCRIPT_DIR/terraform-provider-fmc"
TFRC="$SCRIPT_DIR/dev.tfrc"
TFVARS="$SCRIPT_DIR/test.auto.tfvars"

if [[ -z "$TERRAFORM_BIN" ]]; then
  TERRAFORM_BIN="$(command -v terraform 2>/dev/null || true)"
fi

# ── Prerequisites ──────────────────────────────────────────────────────────────
header "Checking prerequisites"
for cmd in go python3 curl; do
  command -v "$cmd" &>/dev/null && pass "$cmd found" || { echo -e "${RED}✗ $cmd not found${NC}"; exit 1; }
done

[[ -z "$TERRAFORM_BIN" ]] && { echo -e "${RED}✗ terraform not found — pass with --terraform${NC}"; exit 1; }
[[ ! -x "$TERRAFORM_BIN" ]] && { echo -e "${RED}✗ terraform not executable: $TERRAFORM_BIN${NC}"; exit 1; }
pass "terraform found ($TERRAFORM_BIN)"

terraform() { "$TERRAFORM_BIN" "$@"; }

# ── Build provider ─────────────────────────────────────────────────────────────
header "Building provider"
(cd "$REPO_DIR" && go build -o "$PROVIDER_BIN" .) || { echo -e "${RED}✗ go build failed${NC}"; exit 1; }
pass "Provider binary: $PROVIDER_BIN"

cat > "$TFRC" <<EOF
provider_installation {
  dev_overrides { "CiscoDevNet/fmc" = "${SCRIPT_DIR}" }
  direct {}
}
EOF

export TF_CLI_CONFIG_FILE="$TFRC"
export FMC_USERNAME FMC_PASSWORD FMC_URL
export FMC_INSECURE=true

# ── FMC API helpers ────────────────────────────────────────────────────────────

# Fetch a short-lived auth token from FMC and print the domain UUID.
# Writes TOKEN and DOMAIN_UUID into the environment.
fmc_auth() {
  local headers
  headers=$(curl -sk -X POST "$FMC_URL/api/fmc_platform/v1/auth/generatetoken" \
    -u "$FMC_USERNAME:$FMC_PASSWORD" -D - -o /dev/null)
  TOKEN=$(echo "$headers" | grep -i "X-auth-access-token:" | awk '{print $2}' | tr -d '\r')
  [[ -z "$TOKEN" ]] && { fail "Could not obtain FMC auth token"; return 1; }

  DOMAIN_UUID=$(curl -sk "$FMC_URL/api/fmc_platform/v1/info/domain" \
    -H "X-auth-access-token: $TOKEN" | \
    python3 -c "import json,sys; d=json.load(sys.stdin); \
      print([x for x in d['items'] if x['name']=='Global'][0]['uuid'])")
  [[ -z "$DOMAIN_UUID" ]] && { fail "Could not determine FMC Global domain UUID"; return 1; }
}

# Print names of all network groups whose name starts with __gc_
fmc_list_gc_groups() {
  curl -sk "$FMC_URL/api/fmc_config/v1/domain/$DOMAIN_UUID/object/networkgroups?limit=1000&expanded=false" \
    -H "X-auth-access-token: $TOKEN" | \
    python3 -c "
import json, sys
d = json.load(sys.stdin)
gc = [x['name'] for x in d.get('items', []) if x['name'].startswith('__gc_')]
print('\n'.join(gc))
"
}

# ── State cleanup helper ───────────────────────────────────────────────────────
# destroy_state: tries to destroy all resources in the current terraform state.
# Uses -auto-approve and swallows errors (cleanup is best-effort).
destroy_state() {
  info "Destroying terraform state..."
  terraform destroy -auto-approve 2>&1 | tail -5 || true
}

# reset_state: removes all local terraform state and lock files.
reset_state() {
  rm -f "$SCRIPT_DIR/terraform.tfstate" \
        "$SCRIPT_DIR/terraform.tfstate.backup" \
        "$SCRIPT_DIR/.terraform.lock.hcl" \
        "$TFVARS"
  rm -rf "$SCRIPT_DIR/.terraform"
}

# full_cleanup: destroy resources then wipe local state.
full_cleanup() {
  destroy_state
  reset_state
}

# ── Global cleanup on exit ─────────────────────────────────────────────────────
trap_cleanup() {
  info "Final cleanup..."
  cd "$SCRIPT_DIR"
  [[ -f terraform.tfstate ]] && destroy_state || true
  reset_state
  rm -f "$PROVIDER_BIN" "$TFRC"
}
trap trap_cleanup EXIT

cd "$SCRIPT_DIR"
terraform -chdir="$SCRIPT_DIR" init -upgrade > /dev/null 2>&1 || true

# ══════════════════════════════════════════════════════════════════════════════
# Shared test data
# ══════════════════════════════════════════════════════════════════════════════

# Full config: three groups, three rules each referencing their own group.
FULL_UNSAFE_TFVARS=$(cat <<'EOF'
use_safe = false
network_groups = {
  "tf-ng-safe-test-group-a" = { literal = "10.11.1.0/24" }
  "tf-ng-safe-test-group-b" = { literal = "10.11.2.0/24" }
  "tf-ng-safe-test-group-c" = { literal = "10.11.3.0/24" }
}
access_rule_groups = {
  "tf-ng-safe-test-rule-a" = "tf-ng-safe-test-group-a"
  "tf-ng-safe-test-rule-b" = "tf-ng-safe-test-group-b"
  "tf-ng-safe-test-rule-c" = "tf-ng-safe-test-group-c"
}
EOF
)

# Partial config (unsafe): remove group-a and rule-a.
PARTIAL_UNSAFE_TFVARS=$(cat <<'EOF'
use_safe = false
network_groups = {
  "tf-ng-safe-test-group-b" = { literal = "10.11.2.0/24" }
  "tf-ng-safe-test-group-c" = { literal = "10.11.3.0/24" }
}
access_rule_groups = {
  "tf-ng-safe-test-rule-b" = "tf-ng-safe-test-group-b"
  "tf-ng-safe-test-rule-c" = "tf-ng-safe-test-group-c"
}
EOF
)

# Empty config (unsafe): no groups, no rules.
EMPTY_UNSAFE_TFVARS=$(cat <<'EOF'
use_safe = false
network_groups    = {}
access_rule_groups = {}
EOF
)

# Full config (safe): same three groups + three rules, using fmc_network_groups_safe.
FULL_SAFE_TFVARS=$(cat <<'EOF'
use_safe = true
network_groups = {
  "tf-ng-safe-test-group-a" = { literal = "10.11.1.0/24" }
  "tf-ng-safe-test-group-b" = { literal = "10.11.2.0/24" }
  "tf-ng-safe-test-group-c" = { literal = "10.11.3.0/24" }
}
access_rule_groups = {
  "tf-ng-safe-test-rule-a" = "tf-ng-safe-test-group-a"
  "tf-ng-safe-test-rule-b" = "tf-ng-safe-test-group-b"
  "tf-ng-safe-test-rule-c" = "tf-ng-safe-test-group-c"
}
EOF
)

# Partial config (safe): remove group-a and rule-a.
PARTIAL_SAFE_TFVARS=$(cat <<'EOF'
use_safe = true
network_groups = {
  "tf-ng-safe-test-group-b" = { literal = "10.11.2.0/24" }
  "tf-ng-safe-test-group-c" = { literal = "10.11.3.0/24" }
}
access_rule_groups = {
  "tf-ng-safe-test-rule-b" = "tf-ng-safe-test-group-b"
  "tf-ng-safe-test-rule-c" = "tf-ng-safe-test-group-c"
}
EOF
)

# Empty config (safe): no groups, no rules.
EMPTY_SAFE_TFVARS=$(cat <<'EOF'
use_safe = true
network_groups    = {}
access_rule_groups = {}
EOF
)

# Nested groups config (safe): group-a is a child of group-b.
# group-b and group-c are referenced by rules. group-a is only referenced by group-b.
NESTED_SAFE_TFVARS=$(cat <<'EOF'
use_safe = true
network_groups = {
  "tf-ng-safe-test-group-a" = { literal = "10.11.1.0/24" }
  "tf-ng-safe-test-group-b" = { literal = "10.11.2.0/24" }
  "tf-ng-safe-test-group-c" = { literal = "10.11.3.0/24" }
}
access_rule_groups = {
  "tf-ng-safe-test-rule-b" = "tf-ng-safe-test-group-b"
  "tf-ng-safe-test-rule-c" = "tf-ng-safe-test-group-c"
}
EOF
)

# Note: the nested relationship (group-a inside group-b) is set via network_groups attribute
# in fmc_network_groups_safe. We override main.tf for test 5 via a separate tfvars approach.
# Since main.tf does not expose network_groups children via variables, test 5 uses its own
# terraform config written inline by the script.

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 1 — fmc_network_groups: remove one group (expect BREAK)"
# ══════════════════════════════════════════════════════════════════════════════

echo "$FULL_UNSAFE_TFVARS" > "$TFVARS"
info "Applying full config (3 groups, 3 rules)..."
if ! terraform apply -auto-approve > /dev/null 2>&1; then
  fail "TEST 1: Initial full apply failed — cannot proceed"
else
  pass "Full config applied"

  echo "$PARTIAL_UNSAFE_TFVARS" > "$TFVARS"
  info "Applying partial config (remove group-a + rule-a)..."
  if terraform apply -auto-approve > /tmp/tf_test1.txt 2>&1; then
    fail "TEST 1: apply succeeded — fmc_network_groups should have failed with 409"
  else
    PLAIN=$(sed 's/\x1b\[[0-9;]*m//g' /tmp/tf_test1.txt)
    if echo "$PLAIN" | grep -qi "StatusCode 4"; then
      pass "TEST 1 CONFIRMED: fmc_network_groups failed (network group still referenced by access rule)"
    else
      fail "TEST 1: apply failed but not with an expected HTTP 4xx error — check output: /tmp/tf_test1.txt"
    fi
  fi

  info "Cleaning up test 1..."
  echo "$FULL_UNSAFE_TFVARS" > "$TFVARS"
  full_cleanup
fi

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 2 — fmc_network_groups: remove all groups (expect BREAK)"
# ══════════════════════════════════════════════════════════════════════════════

echo "$FULL_UNSAFE_TFVARS" > "$TFVARS"
info "Applying full config (3 groups, 3 rules)..."
if ! terraform apply -auto-approve > /dev/null 2>&1; then
  fail "TEST 2: Initial full apply failed — cannot proceed"
else
  pass "Full config applied"

  echo "$EMPTY_UNSAFE_TFVARS" > "$TFVARS"
  info "Applying empty config (remove all groups and rules)..."
  if terraform apply -auto-approve > /tmp/tf_test2.txt 2>&1; then
    fail "TEST 2: apply succeeded — fmc_network_groups should have failed with 409"
  else
    PLAIN=$(sed 's/\x1b\[[0-9;]*m//g' /tmp/tf_test2.txt)
    if echo "$PLAIN" | grep -qi "StatusCode 4"; then
      pass "TEST 2 CONFIRMED: fmc_network_groups failed (all groups still referenced by access rules)"
    else
      fail "TEST 2: apply failed but not with an expected HTTP 4xx error — check output: /tmp/tf_test2.txt"
    fi
  fi

  info "Cleaning up test 2..."
  echo "$FULL_UNSAFE_TFVARS" > "$TFVARS"
  full_cleanup
fi

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 3 — fmc_network_groups_safe: remove one group (expect SUCCESS + GC)"
# ══════════════════════════════════════════════════════════════════════════════

echo "$FULL_SAFE_TFVARS" > "$TFVARS"
info "Applying full config (3 groups, 3 rules)..."
if ! terraform apply -auto-approve > /dev/null 2>&1; then
  fail "TEST 3: Initial full apply failed — cannot proceed"
else
  pass "Full config applied"

  echo "$PARTIAL_SAFE_TFVARS" > "$TFVARS"
  info "Applying partial config (remove group-a + rule-a)..."
  if ! terraform apply -auto-approve > /tmp/tf_test3a.txt 2>&1; then
    fail "TEST 3: apply failed — fmc_network_groups_safe should have soft-deleted group-a"
    cat /tmp/tf_test3a.txt
  else
    pass "Partial apply succeeded"

    # Verify __gc_ group exists in FMC
    fmc_auth
    GC_GROUPS=$(fmc_list_gc_groups)
    if [[ -n "$GC_GROUPS" ]]; then
      pass "Soft-deleted group found in FMC:"
      echo "$GC_GROUPS" | while IFS= read -r g; do info "  $g"; done
    else
      fail "TEST 3: No __gc_ group found in FMC after soft-delete"
    fi

    # Second apply — GC pass should delete the __gc_ group
    info "Running second apply (GC pass)..."
    if ! terraform apply -auto-approve > /tmp/tf_test3b.txt 2>&1; then
      fail "TEST 3: Second apply failed"
    else
      pass "Second apply succeeded"

      fmc_auth
      GC_GROUPS_AFTER=$(fmc_list_gc_groups)
      if [[ -z "$GC_GROUPS_AFTER" ]]; then
        pass "TEST 3 CONFIRMED: __gc_ group successfully removed from FMC by GC pass"
      else
        fail "TEST 3: __gc_ group still present in FMC after GC pass:"
        echo "$GC_GROUPS_AFTER" | while IFS= read -r g; do info "  $g"; done
      fi
    fi
  fi

  info "Cleaning up test 3..."
  full_cleanup
fi

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 4 — fmc_network_groups_safe: remove all groups (expect SUCCESS + GC)"
# ══════════════════════════════════════════════════════════════════════════════

echo "$FULL_SAFE_TFVARS" > "$TFVARS"
info "Applying full config (3 groups, 3 rules)..."
if ! terraform apply -auto-approve > /dev/null 2>&1; then
  fail "TEST 4: Initial full apply failed — cannot proceed"
else
  pass "Full config applied"

  echo "$EMPTY_SAFE_TFVARS" > "$TFVARS"
  info "Applying empty config (remove all groups and rules)..."
  if ! terraform apply -auto-approve > /tmp/tf_test4a.txt 2>&1; then
    fail "TEST 4: apply failed — fmc_network_groups_safe should have soft-deleted all groups"
    cat /tmp/tf_test4a.txt
  else
    pass "Empty apply succeeded"

    fmc_auth
    GC_GROUPS=$(fmc_list_gc_groups)
    GC_COUNT=$(echo "$GC_GROUPS" | grep -c "__gc_" || true)
    if [[ "$GC_COUNT" -eq 3 ]]; then
      pass "All 3 groups soft-deleted in FMC:"
      echo "$GC_GROUPS" | while IFS= read -r g; do info "  $g"; done
    else
      fail "TEST 4: Expected 3 __gc_ groups in FMC, found $GC_COUNT"
      echo "$GC_GROUPS"
    fi

    # Second apply — GC pass should delete all __gc_ groups
    info "Running second apply (GC pass)..."
    if ! terraform apply -auto-approve > /tmp/tf_test4b.txt 2>&1; then
      fail "TEST 4: Second apply failed"
    else
      pass "Second apply succeeded"

      fmc_auth
      GC_GROUPS_AFTER=$(fmc_list_gc_groups)
      if [[ -z "$GC_GROUPS_AFTER" ]]; then
        pass "TEST 4 CONFIRMED: All __gc_ groups successfully removed from FMC by GC pass"
      else
        fail "TEST 4: __gc_ groups still present in FMC after GC pass:"
        echo "$GC_GROUPS_AFTER" | while IFS= read -r g; do info "  $g"; done
      fi
    fi
  fi

  info "Cleaning up test 4..."
  full_cleanup
fi

# ══════════════════════════════════════════════════════════════════════════════
header "TEST 5 — fmc_network_groups_safe: nested groups (group-a child of group-b, expect SUCCESS)"
# ══════════════════════════════════════════════════════════════════════════════
#
# group-a is a child of group-b (via the network_groups attribute).
# group-b and group-c are referenced by access rules. group-a is only referenced by group-b.
# When group-a is removed: updateSubresources first updates group-b (removes the child ref),
# then deletes group-a — which now succeeds cleanly without any soft-delete.
# This tests that within-resource topological ordering handles nested references correctly.

T5_DIR="$SCRIPT_DIR/.test5_tmp"
mkdir -p "$T5_DIR"
cp "$SCRIPT_DIR/providers.tf" "$T5_DIR/providers.tf"

# Write the TFRC so the temp workspace finds the local provider binary.
cat > "$TFRC" <<EOF
provider_installation {
  dev_overrides { "CiscoDevNet/fmc" = "${SCRIPT_DIR}" }
  direct {}
}
EOF

cat > "$T5_DIR/main.tf" <<'TFEOF'
resource "fmc_access_control_policy" "test" {
  name              = "tf-ng-safe-test5-acp"
  default_action    = "BLOCK"
  manage_rules      = false
  manage_categories = false
}

# group-a is a child of group-b via the network_groups attribute.
resource "fmc_network_groups_safe" "test" {
  items = {
    "tf-ng-safe-test5-group-a" = {
      literals = [{ value = "10.11.1.0/24" }]
    }
    "tf-ng-safe-test5-group-b" = {
      literals       = [{ value = "10.11.2.0/24" }]
      network_groups = ["tf-ng-safe-test5-group-a"]
    }
    "tf-ng-safe-test5-group-c" = {
      literals = [{ value = "10.11.3.0/24" }]
    }
  }
}

resource "fmc_access_rules" "test" {
  access_control_policy_id = fmc_access_control_policy.test.id
  items = [
    {
      name   = "tf-ng-safe-test5-rule-b"
      action = "ALLOW"
      destination_network_objects = [{
        id   = fmc_network_groups_safe.test.items["tf-ng-safe-test5-group-b"].id
        type = "NetworkGroup"
      }]
    },
    {
      name   = "tf-ng-safe-test5-rule-c"
      action = "ALLOW"
      destination_network_objects = [{
        id   = fmc_network_groups_safe.test.items["tf-ng-safe-test5-group-c"].id
        type = "NetworkGroup"
      }]
    },
  ]
}
TFEOF
cp "$T5_DIR/main.tf" "$T5_DIR/main_full.tf.tpl"

# Partial config: remove group-a; group-b no longer references it.
cat > "$T5_DIR/main_partial.tf.tpl" <<'TFEOF'
resource "fmc_access_control_policy" "test" {
  name              = "tf-ng-safe-test5-acp"
  default_action    = "BLOCK"
  manage_rules      = false
  manage_categories = false
}

# group-a removed. group-b no longer references it.
resource "fmc_network_groups_safe" "test" {
  items = {
    "tf-ng-safe-test5-group-b" = {
      literals = [{ value = "10.11.2.0/24" }]
    }
    "tf-ng-safe-test5-group-c" = {
      literals = [{ value = "10.11.3.0/24" }]
    }
  }
}

resource "fmc_access_rules" "test" {
  access_control_policy_id = fmc_access_control_policy.test.id
  items = [
    {
      name   = "tf-ng-safe-test5-rule-b"
      action = "ALLOW"
      destination_network_objects = [{
        id   = fmc_network_groups_safe.test.items["tf-ng-safe-test5-group-b"].id
        type = "NetworkGroup"
      }]
    },
    {
      name   = "tf-ng-safe-test5-rule-c"
      action = "ALLOW"
      destination_network_objects = [{
        id   = fmc_network_groups_safe.test.items["tf-ng-safe-test5-group-c"].id
        type = "NetworkGroup"
      }]
    },
  ]
}
TFEOF

t5_cleanup() {
  info "Cleaning up test 5..."
  # Restore full config before destroying so all resources are in state.
  cp "$T5_DIR/main_full.tf.tpl" "$T5_DIR/main.tf" 2>/dev/null || true
  (cd "$T5_DIR" && TF_CLI_CONFIG_FILE="$TFRC" terraform destroy -auto-approve 2>/dev/null) || true
  rm -rf "$T5_DIR"
}

info "Applying full nested config (group-a child of group-b)..."
if ! (cd "$T5_DIR" && TF_CLI_CONFIG_FILE="$TFRC" terraform apply -auto-approve > /tmp/tf_test5_init.txt 2>&1); then
  fail "TEST 5: Initial full apply failed — cannot proceed"
  cat /tmp/tf_test5_init.txt >&2
  t5_cleanup
else
  pass "Full nested config applied"

  # Switch to partial config (group-a removed, group-b no longer references it)
  cp "$T5_DIR/main_partial.tf.tpl" "$T5_DIR/main.tf"

  info "Applying partial config (remove group-a, update group-b to drop the child reference)..."
  if ! (cd "$T5_DIR" && TF_CLI_CONFIG_FILE="$TFRC" terraform apply -auto-approve > /tmp/tf_test5a.txt 2>&1); then
    fail "TEST 5: apply failed — fmc_network_groups_safe should have soft-deleted group-a"
    cat /tmp/tf_test5a.txt
  else
    pass "Partial apply succeeded"

    # For nested groups within a single fmc_network_groups_safe resource, the topological sort
    # in updateSubresources updates group-b (removing group-a from its children) BEFORE deleting
    # group-a. So group-a is deleted cleanly without needing a soft-delete.
    fmc_auth
    GC_GROUPS=$(fmc_list_gc_groups)
    if [[ -z "$GC_GROUPS" ]]; then
      pass "TEST 5 CONFIRMED: group-a deleted cleanly — within-resource topological ordering handled the nested reference without needing a soft-delete"
    else
      fail "TEST 5: Unexpected __gc_ group found — topological ordering should have handled this:"
      echo "$GC_GROUPS" | while IFS= read -r g; do info "  $g"; done
    fi
  fi

  t5_cleanup
fi

# ══════════════════════════════════════════════════════════════════════════════
header "Summary"
# ══════════════════════════════════════════════════════════════════════════════

if [[ "$OVERALL_FAIL" -eq 0 ]]; then
  echo -e "\n${GREEN}${BOLD}All tests passed.${NC}\n"
else
  echo -e "\n${RED}${BOLD}One or more tests failed. Review output above.${NC}\n"
  exit 1
fi
