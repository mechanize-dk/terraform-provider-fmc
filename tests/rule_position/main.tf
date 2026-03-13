resource "fmc_access_control_policy" "test" {
  name              = "tf-position-test-acp"
  default_action    = "BLOCK"
  manage_rules      = false
  manage_categories = false
}

locals {
  rules = {
    "tf-position-test-rule1" = "ALLOW"
    "tf-position-test-rule2" = "ALLOW"
    "tf-position-test-rule3" = "ALLOW"
    "tf-position-test-rule4" = "ALLOW"
    "tf-position-test-rule5" = "ALLOW"
  }
}

resource "fmc_access_rule" "test" {
  for_each = local.rules

  access_control_policy_id = fmc_access_control_policy.test.id
  name                     = each.key
  action                   = each.value
  section                  = "default"
}

output "policy_id" {
  value = fmc_access_control_policy.test.id
}
