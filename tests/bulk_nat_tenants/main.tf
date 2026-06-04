variable "ftdv_name" {
  type    = string
  default = ""
}

resource "fmc_ftd_nat_policy" "shared" {
  name         = "tf-bulk-nat-test-shared"
  manage_rules = false
}

resource "fmc_security_zone" "tenant_a" {
  name           = "tf-bulk-nat-test-zone-a"
  interface_type = "ROUTED"
}

resource "fmc_security_zone" "tenant_b" {
  name           = "tf-bulk-nat-test-zone-b"
  interface_type = "ROUTED"
}

resource "fmc_host" "src_a" {
  name = "tf-bnt-src-a"
  ip   = "10.10.0.1"
}

resource "fmc_host" "trans_a" {
  name = "tf-bnt-trans-a"
  ip   = "10.10.0.2"
}

resource "fmc_host" "src_b" {
  name = "tf-bnt-src-b"
  ip   = "10.20.0.1"
}

resource "fmc_host" "trans_b" {
  name = "tf-bnt-trans-b"
  ip   = "10.20.0.2"
}

resource "fmc_mze_manual_nat_rules" "tenant_a" {
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
}

resource "fmc_mze_manual_nat_rules" "tenant_b" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.shared.id
  match_on = {
    source_interface_id = fmc_security_zone.tenant_b.id
  }
  before_auto = [
    {
      key                  = "rule_1"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.src_b.id
      translated_source_id = fmc_host.trans_b.id
    },
  ]
}

resource "fmc_mze_auto_nat_rules" "tenant_a_auto" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.shared.id
  match_on = {
    source_interface_id = fmc_security_zone.tenant_a.id
  }
  rules = {
    "obj_a" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_host.src_a.id
      translated_network_id = fmc_host.trans_a.id
    }
  }
}
