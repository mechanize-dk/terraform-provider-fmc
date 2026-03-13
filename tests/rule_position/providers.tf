terraform {
  required_providers {
    fmc = {
      source = "CiscoDevNet/fmc"
    }
  }
}

# Credentials and URL are read from environment variables:
#   FMC_USERNAME, FMC_PASSWORD, FMC_URL
# Set by run_test.sh before invoking terraform.
provider "fmc" {
  insecure = true
}
