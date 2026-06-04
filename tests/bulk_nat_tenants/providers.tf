terraform {
  required_providers {
    fmc = { source = "CiscoDevNet/fmc" }
  }
}

provider "fmc" {
  insecure = true
}
