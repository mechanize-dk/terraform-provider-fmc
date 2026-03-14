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

At current the following resources has been added:
- **fmc_network_groups_safe**
  <br>Is a drop-in replacement for `fmc_network_groups` that handles a dependency problem the FMC has with network group deletion.
  When Terraform destroys or replaces a network group that is still referenced by an access-rule (or another group), the FMC rejects the DELETE with HTTP 400. The standard `fmc_network_groups` resource fails with an error, and the network group is left behind in the FMC — requiring manual cleanup.
  This happens in a common Terraform pattern: `fmc_network_groups` has `depends_on = [fmc_access_rules]` to ensure rules are created before groups. The dependency also controls the destroy order — but destroy order is the reverse of create order, so Terraform destroys (or updates) `fmc_network_groups` *before* it updates `fmc_access_rules`. If a rule still holds a reference to a group being deleted, the FMC rejects it.
  Instead of failing, `fmc_network_groups_safe` performs a **soft delete**:
    1. The group is renamed to `__gc_<original-fmc-id>` and its description is set to `GC: was <original-name>` so it remains identifiable in the FMC UI.
    2. Its content is replaced with a single harmless literal (`127.6.6.6`) as the FMC requires at least one member, and a loopback address ensures any access-rule still pointing at it becomes effectively inactive.
    3. Terraform state is updated as if the group was deleted — the apply succeeds.
    4. The renamed group is cleaned up automatically. Every time `fmc_network_groups_safe` is read (during `terraform plan` or `terraform apply`), it scans all network groups in the FMC for names starting with `__gc_` and attempts to delete them. By that point Terraform has already updated the access-rules, so the reference is gone and the FMC accepts the delete.

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
