# Bulk NAT resources with tenant-scoped ownership

**Status:** design approved, pending implementation plan
**Date:** 2026-06-04
**Patch number (when shipped):** Patch 12
**Related:** Patch 11 (manual NAT rule duplicate-by-index recovery)

## Problem

Multiple tenants share one FTD NAT policy on the same FMC, each writing rules from their own independent Terraform state. Today the only available primitives are `fmc_ftd_manual_nat_rule` and `fmc_ftd_auto_nat_rule` (single-rule, generated). They don't express:

1. **Intra-tenant ordering.** A tenant typically has several rules in `BEFORE_AUTO` or `AFTER_AUTO` and cares that `rule_1` comes before `rule_2` — but doesn't care where their block sits relative to other tenants'.
2. **Tenant-scoped lifecycle.** A tenant should not see other tenants' rules as drift, and edits in one tenant's state should not touch another tenant's rules.
3. **Bulk apply ergonomics.** Declaring N rules today requires N HCL blocks with no shared parent.

This design adds two new fork-owned resources that solve all three.

## Resources

### `fmc_ftd_manual_nat_rules`

Bulk resource wrapping `/policy/ftdnatpolicies/{id}/manualnatrules`. Owns two ordered lists per instance — `before_auto` and `after_auto` — within the boundary expressed by `match_on`. Tenant-chosen `key` per item gives stable identity across plans; list position dictates intra-tenant order (the FMC 1-indexed position within the section).

### `fmc_ftd_auto_nat_rules`

Bulk resource wrapping `/policy/ftdnatpolicies/{id}/autonatrules`. Owns one map of rules per instance — order opaque, since FMC reorders auto-NAT rules by specificity (verified by live probe 2026-06-03 against the lab FMC: three Host-typed rules ranked ahead of one Network-typed rule even though Network was POSTed first; intra-tier order non-deterministic). The map key is the tenant's identity for the rule.

Both resources are hand-written in `internal/provider/`, outside template markers — following the `fmc_network_groups_safe` precedent (Patch 8).

## Foundational decisions

These came out of the brainstorming Q&A and frame the rest of the design.

- **Tenant state model: independent.** Each tenant has its own Terraform state. Resources must ignore rules outside their own scope (no drift errors on other tenants' rules).
- **Ownership model: cooperative.** A resource manages only the rule IDs stored in its TF state. `match_on` is not enforced as a section-wide boundary; out-of-band rules in the same scope coexist peacefully. The trade-off (less strong drift detection) is accepted as the cost of forgiveness in a multi-tenant environment.
- **Item identity model: tenant-chosen `key` + ordered list.** A custom planmodifier diffs by `key`, so reordering doesn't thrash. Adds, removes, in-place modifies, and moves are each their own clean diff class.
- **Two resources, not one.** Manual NAT and auto-NAT have different endpoints, different schemas, and different ordering semantics. Bundling them in one HCL block hides those differences and confuses operators. Users wanting a unified surface can wrap both in a Terraform module.
- **Implementation: fully manual.** Hand-written resource files. The codegen YAML schema can't express tenant-keyed ordered lists or custom diff logic without significant framework changes, and the resources' semantics differ enough from upstream's per-rule resources that codegen would not buy us much.

## Schema: `fmc_ftd_manual_nat_rules`

```hcl
resource "fmc_ftd_manual_nat_rules" "tenant_foo" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.shared.id

  match_on = {
    source_interface_id = fmc_security_zone.tenant_foo.id
    # destination_interface_id = ...   # optional, AND semantics
  }

  before_auto = [
    {
      key                  = "ssh_inbound"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.tenant_foo_lb.id
      translated_source_id = fmc_host.tenant_foo_real.id
      # source_interface_id auto-filled from match_on
    },
    {
      key                  = "https_inbound"
      nat_type             = "STATIC"
      original_source_id   = fmc_host.tenant_foo_https.id
      translated_source_id = fmc_host.tenant_foo_real.id
    },
  ]

  after_auto = [
    # same item shape as before_auto, separate ordered sequence
  ]
}
```

### Top-level attributes

| Attribute | Type | Required | Notes |
|---|---|---|---|
| `ftd_nat_policy_id` | `string` | yes | Parent NAT policy. RequiresReplace. |
| `match_on` | `object` | yes | Tenant scope (see "match_on semantics" below). RequiresReplace. |
| `before_auto` | `list(object)` | no (defaults to `[]`) | Ordered. Position = list index + 1. |
| `after_auto` | `list(object)` | no (defaults to `[]`) | Same shape, independent ordering. |
| `id` (computed) | `string` | — | `<ftd_nat_policy_id>:<sha256(match_on)[:16]>` — deterministic, importable. |
| `before_auto_by_key` (computed) | `map(object)` | — | Populated by the resource as `{ item.key: item, … }`. Lets downstream resources reference by tenant key when position is fluid. |
| `after_auto_by_key` (computed) | `map(object)` | — | Same shape, derived from `after_auto`. |

### Per-item schema

In both `before_auto` and `after_auto`:

- `key` — `string`, required, unique within its list. Tenant-chosen identifier. Plan-only, never POSTed to FMC.
- `id` (computed) — `string`. FMC-assigned rule ID after POST.
- All other rule body fields: mirror the per-item schema of upstream `fmc_ftd_manual_nat_rule` (`nat_type`, `enabled`, `description`, `original_source_id`, `translated_source_id`, `source_interface_id`, `destination_interface_id`, original/translated source/destination port IDs, `fall_through`, `interface_in_original_destination`, `interface_in_translated_source`, `no_proxy_arp`, `unidirectional`, `net_to_net`, `ipv6`).
- The `section` field from the upstream single-rule schema is **omitted** — implicit from which list the item lives in.

## Schema: `fmc_ftd_auto_nat_rules`

```hcl
resource "fmc_ftd_auto_nat_rules" "tenant_foo" {
  ftd_nat_policy_id = fmc_ftd_nat_policy.shared.id

  match_on = {
    source_interface_id = fmc_security_zone.tenant_foo.id
  }

  rules = {
    "tenant_foo_lb_object_nat" = {
      nat_type              = "STATIC"
      original_network_id   = fmc_host.tenant_foo_lb.id
      translated_network_id = fmc_host.tenant_foo_real.id
    }
    "tenant_foo_dynamic_egress" = {
      nat_type                                    = "DYNAMIC"
      original_network_id                         = fmc_network.tenant_foo_internal.id
      translated_network_is_destination_interface = true
    }
  }
}
```

### Top-level attributes

| Attribute | Type | Required | Notes |
|---|---|---|---|
| `ftd_nat_policy_id` | `string` | yes | RequiresReplace. |
| `match_on` | `object` | yes | Same shape and semantics as the manual-NAT version. RequiresReplace. |
| `rules` | `map(object)` | no (defaults to `{}`) | Map key is the tenant-chosen identifier. **No order.** |
| `id` (computed) | `string` | — | Same scheme as manual: `<ftd_nat_policy_id>:<sha256(match_on)[:16]>`. |

### Per-item schema (map values)

- `id` (computed) — `string`. FMC rule ID after POST.
- All other body fields: mirror upstream `fmc_ftd_auto_nat_rule` (`nat_type`, `original_network_id`, `translated_network_id`, port translation fields, interface fields, `fall_through`, `net_to_net`, `no_proxy_arp`, `perform_route_lookup`, `translate_dns`, `translated_network_is_destination_interface`, `ipv6`).
- No `key` field — map key provides identity.
- No `section`, no `position` — there is no ordering surface.

## `match_on` semantics

`match_on` is a nested object with all-optional fields. At least one must be set. Multiple fields combine with AND.

### Supported fields (v1)

| Field | FMC field | Use |
|---|---|---|
| `source_interface_id` | `sourceInterface.id` | Primary tenant boundary (source-zone-per-tenant) |
| `destination_interface_id` | `destinationInterface.id` | Zone-pair scoping (egress per tenant) |

Adding more fields later is non-breaking (optional additions).

### Three roles

1. **Plan-time validation.** Every item is checked: if it explicitly sets a field also declared in `match_on`, the values must match. Otherwise the plan errors with a message naming the offending item key. Prevents accidental cross-tenant rules.
2. **Create-time auto-fill.** Items that omit a field declared in `match_on` get it injected before POST. So a tenant typically writes `source_interface_id` zero times across their config — it lives once on the parent resource.
3. **Import-time scope.** `terraform import fmc_ftd_manual_nat_rules.tenant_foo <ftd_nat_policy_id>:<match_on_hash>` scans the section, finds all rules matching `match_on` (AND semantics), GETs each, and builds state. The hash matches the synthetic resource ID so the importer doesn't need to re-pass the args.

### What `match_on` does not do

- **Not enforced after Create.** Read does not re-validate. If someone edits a rule via the GUI to remove its source zone, the rule stays in state until removed from the items list. Cooperative ownership.
- **Not a section filter during Read.** Read refreshes only the rule IDs stored in state. Other rules in the section are invisible.
- **Not a security boundary.** Two tenants accidentally configuring the same `match_on` will fight on every apply (overlapping cooperative ownerships). Documented as a deployment-time hazard.

### RequiresReplace

Changing `match_on` after Create destroys and re-creates the resource. The synthetic ID includes the `match_on` hash, items' auto-filled fields would need rewriting across the board, and tenants don't relocate to a different zone in practice. If they need to, an explicit destroy+create is the correct shape.

## Idempotency (duplicate-rule recovery)

Per the fork's core contract: when a POST is rejected as duplicate, find the existing object and adopt it instead of failing.

### Manual NAT — reuses Patch 11

Patch 11's `helpers.FindDuplicateByIndex` already parses FMC's `"Duplicate entry of ManualNatRule at rule index (N) is also present at rule index [M]"` 400 error: extract the 1-indexed position `N`, GET at `?offset=N-1&limit=1`, adopt the ID. The bulk resource calls the same helper per-item.

### Auto-NAT — error format needs a probe

Auto-NAT rules have a different natural uniqueness key — `originalNetwork.id` is exclusive per policy. FMC's duplicate-detection 400 likely cites the `originalNetwork.id` rather than a position index, but the exact format hasn't been observed yet.

**Plan prerequisite (not a TBD in this spec):** run a 5-minute focused probe against the lab FMC to capture the error body. Two recovery shapes are pre-designed:
- If the body names the existing rule's ID: parse it, GET by ID.
- If the body cites the `originalNetwork`: search the section, filter by `originalNetwork.id`, GET the match.

The implementation plan must include the probe as its first step; the recovery code is written against whichever shape the probe confirms.

### Bulk-level semantics

Per-item idempotency. On a duplicate, the affected item gets the recovered FMC ID; other items in the same apply proceed normally. The resource never aborts the whole apply because one item was already present.

### Update / Delete

- **Update:** PUT by stored ID. `404` is drift — drop from state, recreate on next apply (cooperative ownership stays consistent).
- **Delete:** DELETE by stored ID. `404` treated as success (already gone).

## CRUD details

### Plan diff (manual NAT — custom planmodifier)

The default list-by-index diff thrashes on reorder. We replace it with a key-based differ:

```
For each list (before_auto, after_auto):
  state_by_key  = { item.key: (pos, id, body) for item in state }
  config_by_key = { item.key: (pos, body)     for item in config }

  removed   = state_by_key.keys() - config_by_key.keys()
  added     = config_by_key.keys() - state_by_key.keys()
  retained  = state_by_key.keys() & config_by_key.keys()
  modified  = { k in retained : state_by_key[k].body != config_by_key[k].body }
  reordered = { k in retained : state_by_key[k].pos  != config_by_key[k].pos  }
```

### Create

For each item in `before_auto` (in list order), auto-fill `match_on` fields and POST `?section=before_auto`. On HTTP 400 / "Duplicate entry" → invoke `helpers.FindDuplicateByIndex` to GET the existing rule and adopt. Repeat for `after_auto`. Store the FMC-returned `id` per item.

Each POST is independent; one item's failure does not abort the others.

### Read

Pure ID-based refresh:

- For each item in state, GET `/policy/.../manualnatrules/<stored_id>`.
- `200` → refresh body; body changes become drift surfaced on next plan.
- `404` → drop the item from state.

No section scan. `match_on` is not a refresh predicate.

### Update

Operation classes from the planmodifier diff:

| Class | Action | Why |
|---|---|---|
| `removed` | DELETE by stored ID | Item gone from config. |
| `modified ∧ ¬reordered` | PUT by stored ID | Body change, position stable. FMC keeps the ID. |
| `reordered` (with or without body change) | DELETE then POST | FMC re-IDs on position move (CLAUDE.md confirms `?insert_before` unsupported on 7.x). |
| `added` | POST | New item, auto-fill `match_on`, idempotency recovery on duplicate. |

Execution order:

1. All DELETEs (covers `removed` + the delete half of `reordered`).
2. All PUTs (in-place modifies).
3. All POSTs (added + repost half of `reordered`) **in final list order**. FMC appends each POST to the section tail, so our POST order dictates our intra-tenant relative order.

A delete-then-immediate-repost can race if FMC hasn't propagated the deletion. Patch 11's recovery handles this transparently — the duplicate POST gets adopted by ID.

### Delete (resource-level)

Iterate stored IDs and DELETE each. `404`s are treated as success. The parent NAT policy is not touched.

### Auto-NAT CRUD

| Class | Action |
|---|---|
| `removed` (key gone from config) | DELETE by stored ID |
| `modified` | PUT by stored ID |
| `added` | POST + idempotency recovery |

No position class. PUT works cleanly — FMC retains the auto-NAT rule's ID on body change (the upstream single-rule resource already relies on this).

## Concurrency

Two tenants applying simultaneously against the same shared NAT policy is expected.

**We rely on:**

- FMC server-side serialization of writes to a single policy.
- Rule IDs being independent (no cross-tenant rule sharing on success paths).
- The fork's existing transient-error / idempotency recovery for HTTP 409s during contention.

**We do not implement:**

- Client-side coordination or distributed locks (Terraform has no cross-state lock primitive).
- A retry-with-backoff loop inside the resource (the provider's existing layer covers that).

**Documented hazard:** two tenants configuring identical `match_on` will fight on every apply. The boundary is enforced by the operator, not the resource.

## Codegen survival — PATCHES.md Patch 12

New files (never touched by `go generate`):

- `internal/provider/resource_fmc_ftd_manual_nat_rules.go`
- `internal/provider/resource_fmc_ftd_auto_nat_rules.go`
- `internal/provider/helpers/nat_match.go`
- `internal/provider/helpers/nat_match_test.go`
- `examples/resources/fmc_ftd_manual_nat_rules/resource.tf`
- `examples/resources/fmc_ftd_auto_nat_rules/resource.tf`
- `tests/bulk_nat_tenants/` (new integration-test harness)

Modifications needing re-application on upstream merges (the Patch 12 entry in PATCHES.md documents these):

- `gen/templates/provider.go` — add `NewFTDManualNatRulesResource,` and `NewFTDAutoNatRulesResource,` after the existing `NewNetworkGroupsSafeResource,` entry in the `Resources()` function.
- `gen/doc_category.go` — add both resource names with category `Policies` to `extraDocs`.

## Testing strategy

### Unit tests (Go)

- `helpers/nat_match_test.go` — AND semantics, hash determinism, validation against item bodies, auto-fill behaviour.
- `helpers/dup_recovery_test.go` — already exists for Patch 11; extended with auto-NAT recovery parser once the FMC probe pins the error format.

### Resource acceptance tests (Go, gated on `FMC_USERNAME`/`FMC_PASSWORD`/`FMC_URL`)

- Per-resource basic CRUD: create, modify, destroy.
- Reorder coverage (manual only): move item from position 3 to 1, verify FMC ID changed and our list reflects the new order.
- Cross-section move: move an item from `before_auto` to `after_auto`, verify destroy-from-one + create-in-other.

### Integration test (bash harness, the fork's pattern)

New `tests/bulk_nat_tenants/run_test.sh`, mirroring `tests/idempotency/run_test.sh`. CLI surface: `-u/-p/--url/--terraform/--ftdv`. Scenarios:

1. **Multi-tenant baseline.** Instantiate `fmc_ftd_manual_nat_rules` and `fmc_ftd_auto_nat_rules` twice (two tenants, two zones) on one shared NAT policy. Apply both, verify both sets present, verify cooperative coexistence.
2. **Idempotency.** Pre-create rules via API matching tenant A's items, run apply, verify adoption (state IDs == pre-created IDs).
3. **Reorder.** Move tenant A's `before_auto[2]` to position 1, apply, verify FMC reflects.
4. **Out-of-band rule in same zone.** Create a rule via API in tenant A's zone but not in tenant A's items. Run apply. Verify it is left untouched (cooperative ownership).
5. **Auto-NAT specificity behaviour.** Create three rules with mixed Host/Network specificities (FMC will reorder by specificity at refresh). Verify the resource refreshes each item by stored ID independent of FMC's position changes, that plan shows no spurious drift after FMC's reordering, and that state remains keyed by tenant key.

The integration test is the gate on the multi-tenant cooperative semantics that unit/acceptance tests can't easily prove.

## Out of scope (explicitly not in v1)

- **Strict-ownership mode** (option A from the ownership question). Documented as a possible v2 toggle.
- **Hybrid manual+auto in one resource.** Two separate resources is the right model.
- **Cross-policy rule migration.** Destroy+create on the user's side; not a primitive.
- **Insert-before-named-rule semantics.** FMC 7.x doesn't support it via API. Order is purely by list position.
- **Generic `match_on` field set beyond `source_interface_id` + `destination_interface_id`.** Extensions are non-breaking additions.
- **PUT with body change for `reordered ∧ modified` items.** Always DELETE+POST in v1. Possible later optimisation.
- **Per-tenant rate limiting.** Concurrency relies on FMC's own serialization.

## Open prerequisites for the implementation plan

1. FMC probe to capture the duplicate-rule error format for auto-NAT (Section "Auto-NAT — error format needs a probe" above).

That is the only known unknown. Everything else is decided.
