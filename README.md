[![Tests](https://github.com/mechanize-dk/terraform-provider-fmc/actions/workflows/test.yml/badge.svg)](https://github.com/mechanize-dk/terraform-provider-fmc/actions/workflows/test.yml)


# Terraform FMC Idempotent Provider

This is a fork of [CiscoDevNet/terraform-provider-fmc](https://github.com/CiscoDevNet/terraform-provider-fmc). The primary addition in this fork is **idempotent resource creation**: when Terraform attempts to create an object that already exists in FMC (e.g. because it was created manually or by a previous run whose state was lost), the provider detects the conflict (HTTP 409, or HTTP 400 with an "already exists" body), looks up the existing object by name, and imports it into state — instead of failing with an error. It solves the following issues:

- **Current FMC objects without terraform state**
  <br>Would normally break the "terraform apply". Now, these objects will trigger a conflict which is handled and imported.
- **FMC rule moves (GUI or API) that assigns a new Id to the rule**
  <br>Would normally break as the old Id would become stale in Terraform state (and the new unknown). This idempotency fix handles this gracefully by letting terraform destroy its current state, then creating without issue. The create triggers a conflict which is handled and imported.
- **Import statements in terraform**
  <br>As conflicts are handled by code, there is no need for import statements. This also fixes objects with no `import` function (like `fmc_access_rules`)

Other than the standard objects, the following bulk objects has also been fixed:
- fmc_network_groups
- fmc_access_rules

The following objects are unchanged by this fix (and are therefore **not** idempotent) mainly due to the fact that they were coded manually in the original provider:
- resource.fmc_device_vtep_policy
- resource.fmc_policy_assignment
- resource.fmc_device_cluster


## The original provider

The FMC provider provides resources to interact with a Cisco Secure Firewall Management Center (FMC) and Cloud-Delivered FMC (cdFMC) instances. It communicates with FMC via the REST API.

Resources and Data Sources have been tested with the following releases.

| Platform | Version |
| -------- | ------- |
| FMC      | 7.2.10  |
| FMC      | 7.4.5   |
| FMC      | 7.6.2   |
| FMC      | 7.7.11  |
| FMC      | 10.0.0  |
| cdFMC    |         |

Please note that Resources and Data Sources support depends on FMC version.

Documentation: <https://registry.terraform.io/providers/CiscoDevNet/fmc/latest>

## Requirements

- [Terraform](https://www.terraform.io/downloads.html) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.25

## Building The Provider

1. Clone the repository
2. Enter the repository directory
3. Build the provider using the Go `install` command:

```shell
go install
```

## Adding Dependencies

This provider uses [Go modules](https://github.com/golang/go/wiki/Modules).
Please see the Go documentation for the most up to date information about using Go modules.

To add a new dependency `github.com/author/dependency` to your Terraform provider:

```shell
go get github.com/author/dependency
go mod tidy
```

Then commit the changes to `go.mod` and `go.sum`.

## Using the provider

This Terraform Provider is available to install automatically via `terraform init`. If you're building the provider, follow the instructions to
[install it as a plugin.](https://www.terraform.io/docs/plugins/basics.html#installing-a-plugin)
After placing it into your plugins directory, run `terraform init` to initialize it.

Additional documentation, including available resources and their arguments/attributes can be found on the [Terraform documentation website](https://registry.terraform.io/providers/CiscoDevNet/fmc/latest/docs).

## Developing the Provider

If you wish to work on the provider, you'll first need [Go](http://www.golang.org) installed on your machine (see [Requirements](#requirements) above).

To compile the provider, run `go install`. This will build the provider and put the provider binary in the `$GOPATH/bin` directory.

To generate or update documentation, run `go generate`.

## Acceptance tests

Note: Acceptance tests create real resources. You'd need an FMC instance with an administrative user on the default global domain. Make sure the respective environment variables are set: `FMC_USERNAME`, `FMC_PASSWORD`, `FMC_URL`.

A number of test cases use a pre-existing device (e.g. FTDv). If you want your test to be exhaustive, it's recommended to add it manually to your FMC:

  1. SSH onto FTDv and use `configure manager add` followed by `show managers verbose`.
  2. Use FMC web interface -> Device Management -> Add Device.
  3. After the Device is registered, snatch its UUID, e.g. from the "edit" link, and set it on the environment variable `TF_VAR_device_id`.
  4. Optionally, you might want to test registering/unregistering an FTDv device by exporting the `FTD_USERNAME`, `FTD_PASSWORD`, `FTD_ADDR` (the IP address, not a URL this time). This however requires an unregistered FTDv device, so it's not possible to use the device from the above points. You'd need either two separate FTDv devices, or two separate test runs.

Depending on whether the environment is full or partial, the suite of Acceptance tests will
execute all/partial tests:

```shell
make testacc
```
