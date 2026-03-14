resource "fmc_access_control_policy" "test" {
  name              = "tf-ng-safe-test-acp"
  default_action    = "BLOCK"
  manage_rules      = false
  manage_categories = false
}

# Standard bulk resource — used when use_safe = false (tests 1 & 2).
# When use_safe = true its items map is empty, making it a no-op in FMC.
resource "fmc_network_groups" "test" {
  items = var.use_safe ? {} : {
    for name, cfg in var.network_groups : name => {
      literals = [{ value = cfg.literal }]
    }
  }
}

# Safe bulk resource — used when use_safe = true (tests 3, 4 & 5).
# When use_safe = false its items map is empty, making it a no-op in FMC.
resource "fmc_network_groups_safe" "test" {
  items = var.use_safe ? {
    for name, cfg in var.network_groups : name => {
      literals = [{ value = cfg.literal }]
    }
  } : {}
}

locals {
  # Merge items from both resources. Exactly one will have entries at any time.
  ng_ids = {
    for k, v in merge(
      fmc_network_groups.test.items,
      fmc_network_groups_safe.test.items
    ) : k => v.id
  }
}

resource "fmc_access_rules" "test" {
  access_control_policy_id = fmc_access_control_policy.test.id

  items = [
    for rule_name, group_name in var.access_rule_groups : {
      name   = rule_name
      action = "ALLOW"
      destination_network_objects = [
        {
          id   = local.ng_ids[group_name]
          type = "NetworkGroup"
        }
      ]
    }
  ]
}
