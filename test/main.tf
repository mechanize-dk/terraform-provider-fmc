# ── Tests 1 & 2: fmc_host ────────────────────────────────────────────────────
resource "fmc_host" "idempotency_test" {
  name = "tf-idempotency-test-host"
  ip   = "192.0.2.111"
}

# ── Test 3: fmc_network_group ─────────────────────────────────────────────────
resource "fmc_network_group" "idempotency_test" {
  name = "tf-idempotency-test-netgrp"
  literals = [
    { value = "10.99.0.0/24" }
  ]
}

# ── Prerequisite for Tests 4 & 5 ─────────────────────────────────────────────
resource "fmc_access_control_policy" "test_acp" {
  name              = "tf-idempotency-test-acp"
  default_action    = "BLOCK"
  manage_rules      = false
  manage_categories = false
}

# ── Test 4: fmc_access_category ───────────────────────────────────────────────
resource "fmc_access_category" "idempotency_test" {
  access_control_policy_id = fmc_access_control_policy.test_acp.id
  name                     = "tf-idempotency-test-cat"
}

# ── Test 5: fmc_access_rule ───────────────────────────────────────────────────
resource "fmc_access_rule" "idempotency_test" {
  access_control_policy_id = fmc_access_control_policy.test_acp.id
  name                     = "tf-idempotency-test-rule"
  action                   = "ALLOW"
}
