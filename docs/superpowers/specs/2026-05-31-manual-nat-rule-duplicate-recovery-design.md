# Design — Manual NAT Rule Duplicate-by-Index Recovery (Patch 11)

**Status:** Approved — ready for implementation plan
**Date:** 2026-05-31
**Scope:** `internal/provider/helpers/`, `gen/templates/resource.go`, `tests/idempotency/`

---

## Problem

`fmc_ftd_manual_nat_rule` POST can fail with:

```
HTTP 400
{"error":{"category":"FRAMEWORK","messages":[{"description":
  "Validation failed due to duplicate Manual NAT rules in Policy
   Please remove the duplicate rules applicable to All Devices :
   Duplicate entry of ManualNatRule at rule index (3) is also present at rule index [5]"
}],"severity":"ERROR"}}
```

This is FMC's **content-level** duplicate detection: rule at index 3 has the same NAT tuple (sources, destinations, ports, interfaces, …) as the rule already at index 5. Unlike `fmc_access_rule`, manual NAT rules have **no `name` field**, so the existing `IngestOnConflict` (which searches by `data_source_query` attribute, typically `name`) cannot recover.

Typical triggers:
- A prior `terraform apply` partially succeeded; the rule exists in FMC but not in state, and the next apply hits the same content.
- A user created the rule via FMC GUI and is now adding it to HCL.
- Two `for_each` keys in HCL produce identical NAT content. (Genuine user bug — see "Strict comparison" below for why this is safe.)

Today this surfaces as a hard `terraform apply` failure with no fork-side recovery path.

---

## Goals

1. Recover automatically when FMC reports a content-level duplicate **and** the existing rule's content actually matches what we tried to POST.
2. Refuse to recover (surface the original error) when the existing rule's content differs from what we proposed. The user's intent in that case is ambiguous — they need to see and resolve it.
3. Keep the change minimal on the upstream-conflict surface. Specifically, **no** new YAML flag, **no** per-resource opt-in, **no** changes to the file's manually-maintained `imports` section that Patch 7 preserves.
4. Apply to Create (POST) only. Update (PUT) duplicates would imply silently re-pointing state at a different FMC object, which is destructive; out of scope.

---

## Non-goals

- Adding extra recovery logic to `fmc_access_rule` (standard template, already uses name-based `IngestOnConflict` via Patch 1) or `fmc_access_rules` (custom bulk Create with name-based fallback via Patch 3). Both already have recovery paths appropriate to their semantics.
- Diffing which specific fields mismatched on a strict-comparison failure. Original FMC error plus "content differs" is enough; richer diagnostics can come later.
- Detecting double-ownership across multiple `for_each` keys. If two HCL keys produce byte-identical NAT content, **both** will import the same FMC ID and the user will see the bug on the next destroy/apply cycle. The user has explicitly accepted this trade-off: "people who don't like it can use another provider."

---

## Architecture

Two source files change; one test gains a new case. **No YAML flag.**

```
gen/templates/resource.go                      → modify the existing POST-error `else`
                                                  branch (the "fail" branch) to first try
                                                  duplicate-by-index recovery.

internal/provider/helpers/dup_recovery.go      → new file. Generic helper that parses the
                                                  error message, GETs the existing rule by
                                                  index, returns it as a gjson.Result.

tests/idempotency/                              → new Test 8: pre-create a manual NAT
                                                  rule via FMC API, apply matching HCL,
                                                  expect import-on-duplicate.
```

At next `go generate`:
- ~277 resources without `data_source_query` regenerate with the new recovery block in their `Create` function. One-time commit; thereafter stable.
- ~69 resources with `data_source_query` continue using `IngestOnConflict` — untouched.
- Bulk resources (`access_rules`, `network_groups`) have custom non-templated `Create` functions — untouched.

For resources whose POST errors do not match the duplicate-by-index regex, the new code path is a no-op (helper returns the original error, generated code follows the existing fail path).

---

## Components

### 1. `internal/provider/helpers/dup_recovery.go` (new)

```go
// FindDuplicateByIndex inspects an FMC POST error. If the error body matches
// FMC's "duplicate at index" pattern (currently emitted only for
// fmc_ftd_manual_nat_rule), it extracts the existing rule's index, GETs the
// collection at that index, and returns the existing object's JSON.
//
// Returns (foundRes, nil) when the pattern matched and the GET succeeded —
// foundRes is items[0] of the response.
// Returns (gjson.Result{}, err) in two scenarios that the caller handles
// identically (surface the error and fail):
//   - the error pattern did NOT match → err is the original postErr unchanged
//   - the pattern matched but the GET or response parsing failed → err wraps
//     both the original POST error and the GET error
func FindDuplicateByIndex(
    ctx context.Context,
    client *fmc.Client,
    resourcePath string,
    postErr error,
    postRes gjson.Result,
    reqMods ...func(*fmc.Req),
) (gjson.Result, error)
```

**Regex:** `Duplicate entry .* at rule index \[(\d+)\]`. Anchored on both "Duplicate entry" and "rule index [N]" so it does not false-positive on unrelated error messages that happen to mention "rule index". The capture group is the existing-rule index.

**GET request:** `<resourcePath>?expanded=true&offset=<N>&limit=1`. `resourcePath` is whatever `plan.getPath()` returns for the calling resource — for manual NAT rule, that resolves to `…/ftdnatpolicies/<policy_id>/manualnatrules`.

**Index semantics:** FMC's error message uses 0-indexed positions, matching the `?offset=N` parameter. Validated against a live FMC during implementation.

**No side effects** beyond the optional GET. No state mutation; the caller decides what to do with the returned `gjson.Result`.

### 2. `gen/templates/resource.go` template change

The existing `{{- else}}` branch (the "fail" branch for resources without `data_source_query`) is modified as follows. The other branch (`{{- if and (not .PutCreate) (hasDataSourceQuery .Attributes)}}` → `IngestOnConflict`) is untouched.

```go
{{- else}}
if err != nil {
    foundRes, dupErr := helpers.FindDuplicateByIndex(ctx, r.client, plan.getPath(), err, res, reqMods...)
    if dupErr != nil {
        // Pattern didn't match (dupErr == original err) or GET failed.
        resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Failed to configure object (POST/PUT), got error: %s, %s", dupErr, res.String()))
        return
    }
    // FMC says we'd duplicate an existing object. Parse it and compare to the plan.
    var existing {{camelCase .Name}}
    existing.fromBody(ctx, foundRes)
    planCopy := plan
    planCopy.Id = existing.Id
    if !reflect.DeepEqual(planCopy, existing) {
        resp.Diagnostics.AddError("Client Error", fmt.Sprintf("FMC reported a duplicate object, but the existing object's content differs from the proposed body. Original error: %s", err))
        return
    }
    res = foundRes
}
plan.Id = types.StringValue(res.Get("id").String())
plan.fromBodyUnknowns(ctx, res)
{{- end}}
```

**`reflect` import:** added automatically by `goimports -w internal/provider/`, which already runs as part of `go generate` (see `CLAUDE.md`). Patch 7's imports-preserving behavior in `gen/generator.go` does not interfere: it preserves whatever the file already has, and `goimports` then adds any missing-but-used imports.

**Strict comparison rationale:** `planCopy := plan` is a value copy. Only `planCopy.Id` is rewritten (to `existing.Id`) so the unset-vs-set-ID difference does not cause a false mismatch. Every other model field (including `Domain`, all NAT-tuple fields, etc.) compares as-is via `reflect.DeepEqual`. If anything differs, we surface the error and the user resolves it explicitly. This is the user-requested behavior.

### 3. `tests/idempotency/` extension

**`main.tf` additions** (small; reuses existing test objects where possible):

```hcl
# ── Prerequisite for Test 8 ────────────────────────────────────────
resource "fmc_ftd_nat_policy" "test_natp" {
  name = "tf-idempotency-test-natp"
}

resource "fmc_host" "manual_nat_translated" {
  name = "tf-idempotency-test-nat-trans"
  ip   = "203.0.113.5"
}

# ── Test 8: fmc_ftd_manual_nat_rule ─────────────────────────────────
resource "fmc_ftd_manual_nat_rule" "idempotency_test" {
  ftd_nat_policy_id     = fmc_ftd_nat_policy.test_natp.id
  nat_type              = "STATIC"
  original_source_id    = fmc_host.idempotency_test.id
  translated_source_id  = fmc_host.manual_nat_translated.id
}
```

**`run_test.sh` additions:**

```bash
fmc_create_nat_policy() {
  # args: $1=token $2=domain_uuid $3=name
  # POSTs to /api/fmc_config/v1/domain/$2/policy/ftdnatpolicies
}

fmc_create_manual_nat_rule() {
  # args: $1=token $2=domain_uuid $3=natp_id $4=orig_source_id $5=trans_source_id
  # Body content matches the HCL above bit-for-bit so FMC's duplicate detection fires
  # on the subsequent terraform-driven POST.
}
```

**Test 8 flow** (uses the existing `run_idempotency_test` harness verbatim):

1. Pre-create the two `fmc_host` objects via `fmc_create_host` (already in `main.tf`).
2. Pre-create the NAT policy via `fmc_create_nat_policy`. Capture its ID for HCL state-ID verification.
3. Pre-create the manual NAT rule via `fmc_create_manual_nat_rule` with body content identical to the HCL.
4. `terraform apply -target=fmc_ftd_nat_policy.test_natp -target=fmc_ftd_manual_nat_rule.idempotency_test -target=fmc_host.idempotency_test -target=fmc_host.manual_nat_translated -auto-approve`.
5. **Expected behavior:** the NAT policy POST returns 409 → `IngestOnConflict` imports it (Patch 1). The NAT rule POST returns 400 "Duplicate entry … at rule index [N]" → `FindDuplicateByIndex` GETs index N → `fromBody` + `reflect.DeepEqual` matches → state imports the existing rule's ID.
6. Verify Terraform state ID matches the pre-created rule ID.
7. `terraform plan` shows no changes.
8. `terraform destroy` cleans up.

**EMERGENCY_CLEANUPS** registers DELETE paths for both pre-created objects so a script failure mid-test does not leave debris on FMC.

A negative-path test (existing rule's content differs → expect failure) is **intentionally not** included: writing a stable diff-by-one HCL is fragile, and the strict comparison logic is small enough to read by eye.

---

## Error-handling matrix

| Stage | Outcome | Caller behavior |
|---|---|---|
| Regex no-match on POST error | `FindDuplicateByIndex` returns `(empty, postErr)` | Surface original FMC error (today's behavior) |
| Regex match, GET fails | Returns `(empty, wrappedErr)` | Surface wrapped error including both POST and GET errors |
| Regex match, GET succeeds, `fromBody` parses, `DeepEqual` matches | Returns `(foundRes, nil)` and template code imports | `terraform apply` succeeds; state ID = existing rule's UUID |
| Regex match, GET succeeds, `DeepEqual` mismatches | Template code calls `AddError("FMC reported a duplicate object, but the existing object's content differs…")` | `terraform apply` fails; user resolves the content difference |

---

## Documentation updates after implementation

- `PATCHES.md` — new "Patch 11" entry describing the template branch, helper file, and test. Re-apply checklist updated.
- `STRESS.md` — no change needed (this is not a stress-test problem).

---

## Out-of-scope future work

- Mirroring this recovery onto Update (PUT). Requires a destruction-safety design we explicitly opted out of.
- Extending the regex to additional FMC error patterns if future FMC versions report content-duplicates for other resource types using different wording.
- Detection of double-ownership: warning when two `for_each` keys produce byte-identical content. Would require Terraform state introspection, which is out of provider scope.

---

## Sync-impact summary

| File | Conflict risk on upstream sync |
|---|---|
| `gen/templates/resource.go` | **Medium.** Adjacent to Patch 1's existing modification. Both should be re-applied together. |
| `internal/provider/helpers/dup_recovery.go` | **Zero.** New file, upstream cannot touch it. |
| `gen/definitions/*.yaml` | **Zero.** Untouched. |
| Generated resource files | **Indirect.** Regenerate after sync; one-time large diff is reverted/regenerated cleanly because the template owns the content. |
| `tests/idempotency/` | **Zero.** Not touched by upstream. |
