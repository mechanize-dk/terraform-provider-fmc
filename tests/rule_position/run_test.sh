#!/usr/bin/env bash
# run_test.sh — diagnostic test: does Terraform force-recreate an fmc_access_rule
#               when its position in the policy is changed externally?
#
# NOTE: FMC's PUT endpoint does not accept ?insert_before as a query parameter.
#       The test therefore simulates a position change via DELETE + POST, which
#       gives the re-created rule a new ID in FMC while keeping the same name.
#       Terraform's state will reference the stale (old) ID.
#
# This tests two behaviours:
#   a) Does Terraform plan a *replace* (destroy + create) for the moved rule?
#      → Would indicate the bug the user reported.
#   b) Does Terraform plan a *create* for the moved rule (ID gone from FMC)?
#      → Expected when state holds a stale ID. Our idempotency fix handles this.
#
# Usage:
#   ./run_test.sh -u <username> -p <password> --url <fmc_url> [--terraform /path/to/terraform]

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
pass()   { echo -e "${GREEN}✓ $*${NC}"; }
fail()   { echo -e "${RED}✗ $*${NC}"; exit 1; }
info()   { echo -e "${YELLOW}→ $*${NC}"; }
header() { echo -e "\n${BOLD}══ $* ══${NC}"; }

# ── Argument parsing ──────────────────────────────────────────────────────────
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

# ── Paths ─────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"   # tests/rule_position → tests → repo root
PROVIDER_BIN="$SCRIPT_DIR/terraform-provider-fmc"
TFRC="$SCRIPT_DIR/dev.tfrc"

if [[ -z "$TERRAFORM_BIN" ]]; then
  TERRAFORM_BIN="$(command -v terraform 2>/dev/null || true)"
fi

# ── Prerequisites ─────────────────────────────────────────────────────────────
header "Checking prerequisites"
for cmd in go python3; do
  command -v "$cmd" &>/dev/null && pass "$cmd found" || fail "$cmd not found — please install it"
done

[[ -z "$TERRAFORM_BIN" ]] && fail "terraform not found on PATH — pass its location with --terraform"
[[ ! -x "$TERRAFORM_BIN" ]] && fail "terraform binary not executable: $TERRAFORM_BIN"
pass "terraform found ($TERRAFORM_BIN)"

terraform() { "$TERRAFORM_BIN" "$@"; }

# ── Build provider ────────────────────────────────────────────────────────────
header "Building provider"
(cd "$REPO_DIR" && go build -o "$PROVIDER_BIN" .) || fail "go build failed"
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

# ── Cleanup handler ───────────────────────────────────────────────────────────
cleanup() {
  info "Cleaning up..."
  cd "$SCRIPT_DIR"
  if [[ -f terraform.tfstate ]]; then
    terraform destroy -auto-approve 2>/dev/null || true
  fi
  rm -f terraform.tfstate terraform.tfstate.backup .terraform.lock.hcl \
        "$PROVIDER_BIN" "$TFRC" move_result.json
  rm -rf .terraform
}
trap cleanup EXIT

cd "$SCRIPT_DIR"

# ══════════════════════════════════════════════════════════════════════════════
header "Step 1 — Create 5 access rules via terraform (for_each map)"
# ══════════════════════════════════════════════════════════════════════════════

info "terraform apply (creating ACP + 5 rules)..."
terraform apply -auto-approve
pass "5 rules created successfully"

POLICY_ID=$(terraform output -raw policy_id)
[[ -z "$POLICY_ID" ]] && fail "Could not read policy_id output from terraform state"
info "Policy ID: $POLICY_ID"

info "terraform plan (baseline — expecting no changes)..."
terraform plan -detailed-exitcode -out=/dev/null
pass "Baseline plan shows no changes"

# ══════════════════════════════════════════════════════════════════════════════
header "Step 2 — Move last rule to position 1 via FMC API (DELETE + POST)"
# ══════════════════════════════════════════════════════════════════════════════

python3 "$SCRIPT_DIR/move_rule.py" "$POLICY_ID"
pass "Rule repositioned"

MOVED_RULE=$(python3 -c "import json; d=json.load(open('move_result.json')); print(d['rule_name'])")
OLD_ID=$(python3 -c "import json; d=json.load(open('move_result.json')); print(d['old_id'])")
NEW_ID=$(python3 -c "import json; d=json.load(open('move_result.json')); print(d['new_id'])")
info "Moved rule: $MOVED_RULE"
info "Old ID (still in terraform state): $OLD_ID"
info "New ID (assigned by FMC):          $NEW_ID"

# ══════════════════════════════════════════════════════════════════════════════
header "Step 3 — terraform plan (checking for planned changes)"
# ══════════════════════════════════════════════════════════════════════════════

info "Running terraform plan..."
# Allow exit 2 (changes detected) — we want to inspect the plan regardless.
terraform plan 2>&1 | tee /tmp/tf_plan_output.txt || true

info "Analysing plan output..."

# Strip ANSI colour codes before grepping so patterns match reliably.
PLAIN_PLAN=$(sed 's/\x1b\[[0-9;]*m//g' /tmp/tf_plan_output.txt)

# Resources planned for destroy+create (true force-replace)
# Detected by the "must be replaced" marker in Terraform's text output.
REPLACE_REPORT=$(echo "$PLAIN_PLAN" | grep -E "^\s+# .*must be replaced" \
  | sed 's/.*# //' | sed 's/ must be replaced//' || true)

# Resources planned for create only (stale ID — resource gone from FMC).
# "will be created" lines that are NOT also "must be replaced".
CREATE_REPORT=$(echo "$PLAIN_PLAN" | grep -E "^\s+# .* will be created" \
  | sed 's/.*# //' | sed 's/ will be created//' || true)

# ══════════════════════════════════════════════════════════════════════════════
header "Result"
# ══════════════════════════════════════════════════════════════════════════════

echo ""
if [[ -n "$REPLACE_REPORT" ]]; then
  echo -e "${RED}${BOLD}BUG CONFIRMED${NC} — Terraform plans destroy+recreate for:"
  echo ""
  while IFS= read -r addr; do
    echo -e "  ${RED}✗  $addr${NC}"
  done <<< "$REPLACE_REPORT"
  echo ""
  info "An attribute with requires_replace changed after the rule was repositioned."

elif [[ -n "$CREATE_REPORT" ]]; then
  echo -e "${YELLOW}${BOLD}EXPECTED BEHAVIOUR${NC} — Terraform plans create (not replace) for:"
  echo ""
  while IFS= read -r addr; do
    echo -e "  ${YELLOW}→  $addr${NC}"
  done <<< "$CREATE_REPORT"
  echo ""
  info "The rule's old ID ($OLD_ID) is gone from FMC (deleted during move)."
  info "Terraform sees it as missing and plans to create it — not force-replace."
  info "Our idempotency fix will ingest the existing rule by name on next apply."

else
  echo -e "${GREEN}${BOLD}NO CHANGES DETECTED${NC} — Terraform plans no action for the moved rule."
  echo ""
  info "Unexpected: the rule was deleted from FMC but Terraform sees no change."
fi

echo ""
echo -e "${BOLD}Plan summary:${NC}"
echo "$PLAIN_PLAN" | grep -E "^Plan:|No changes\." || true
