resource "fmc_mze_auto_nat_rules" "example" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.example.id

  match_on = {
    source_interface_id = fmc_security_zone.tenant_example.id
  }

  rules = {
    "example_object_nat" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_host.example_lb.id
      translated_network_id = fmc_host.example_real.id
    }
  }
}
