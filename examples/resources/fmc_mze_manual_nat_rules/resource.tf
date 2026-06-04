resource "fmc_mze_manual_nat_rules" "example" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.example.id

  match_on = {
    source_interface_id = fmc_security_zone.tenant_example.id
  }

  before_auto = [
    {
      key                  = "ssh_inbound"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.example_lb.id
      translated_source_id = fmc_host.example_real.id
    },
  ]
}
