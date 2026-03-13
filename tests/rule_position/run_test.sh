#!/usr/bin/env bash
# run_test.sh — diagnostic test: does Terraform force-recreate an fmc_access_rule
#               when its position in the policy is changed externally?
#
# Steps:
#   1. Build the provider.
#   2. Create 5 access rules via Terraform (for_each map).
#   3. Move the last rule to position 1 via the FMC REST API (move_rule.py).
#   4. Run terraform plan and inspect the output for replace actions.
#   5. Report: BUG CONFIRMED or NO ISSUE DETECTED.
#   6. Clean up (terraform destroy).
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
info "Running: go build -o $PROVIDER_BIN"
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
        "$PROVIDER_BIN" "$TFRC" plan.tfplan
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
header "Step 2 — Move last rule to position 1 via FMC API"
# ══════════════════════════════════════════════════════════════════════════════

python3 "$SCRIPT_DIR/move_rule.py" "$POLICY_ID"
pass "Rule repositioned"

# ══════════════════════════════════════════════════════════════════════════════
header "Step 3 — terraform plan (checking for unexpected replacements)"
# ══════════════════════════════════════════════════════════════════════════════

info "Running terraform plan..."
# Allow non-zero exit (exit 2 = changes detected) — we want to inspect the plan.
terraform plan -out=plan.tfplan 2>&1 | tee /tmp/tf_plan_output.txt || true

info "Analysing plan for destroy+recreate actions..."
REPLACE_REPORT=$(terraform show -json plan.tfplan 2>/dev/null | python3 - <<'PYEOF'
import json, sys
data = json.load(sys.stdin)
for rc in data.get("resource_changes", []):
    actions = rc.get("change", {}).get("actions", [])
    if "delete" in actions and "create" in actions:
        print(rc["address"])
PYEOF
)

# ══════════════════════════════════════════════════════════════════════════════
header "Result"
# ══════════════════════════════════════════════════════════════════════════════

echo ""
if [[ -n "$REPLACE_REPORT" ]]; then
  echo -e "${RED}${BOLD}BUG CONFIRMED${NC} — Terraform plans destroy+recreate for the following resource(s):"
  echo ""
  while IFS= read -r addr; do
    echo -e "  ${RED}✗  $addr${NC}"
  done <<< "$REPLACE_REPORT"
  echo ""
  info "Repositioning an access rule (within the same section/category) triggers"
  info "force-replacement. The position change was detected as a configuration drift."
  echo ""
  # Show the relevant section of the plan output
  echo -e "${BOLD}Relevant plan output:${NC}"
  grep -A5 "must be replaced\|will be replaced\|force replacement" /tmp/tf_plan_output.txt || true
else
  echo -e "${GREEN}${BOLD}NO ISSUE DETECTED${NC} — Terraform plans no replacement after repositioning."
  echo ""
  info "Moving a rule to a different position does not trigger any planned changes."
fi

echo ""
echo -e "${BOLD}Plan summary:${NC}"
grep -E "^Plan:|No changes\." /tmp/tf_plan_output.txt || true
