# PATCHES.md — mechanize-dk fork patches over CiscoDevNet/terraform-provider-fmc

This document describes every change made in this fork relative to upstream.
The fork diverged at upstream commit **`701b7f56`** ("Add support for network
objects overrides (#378)"), which corresponds to the **v2.0.1** release.

Use this file to re-apply all patches after a sync with upstream.

---

## Overview of patches

| # | Area | File(s) touched | Conflict risk on sync |
|---|------|-----------------|-----------------------|
| 1 | Idempotency — standard resources | `gen/templates/resource.go` + generated `resource_fmc_*.go` | Low — template change is now ~20 lines |
| 2 | Idempotency — network groups bulk | `resource_fmc_network_groups_idempotency.go` (new) + 2-line call in `.go` | Near-zero — logic is in its own file |
| 3 | Idempotency — access rules bulk | `resource_fmc_access_rules_idempotency.go` (new) + 2-line call in `.go` | Near-zero — logic is in its own file |
| 4 | Retry on transient FMC errors | `internal/provider/helpers/utils.go` | None — helpers/ not generated |
| 5 | `IngestOnConflict` helper | `internal/provider/helpers/utils.go` | None — helpers/ not generated |
| 6 | URL param length fix | `gen/templates/provider.go` + generated `provider.go` | None — single constant in template |
| 7 | Code generator: preserve imports | `gen/generator.go` | None — generator not generated |
| 8 | New resource: fmc_network_groups_safe | multiple new files | None — entirely new files |
| 9 | Tests | `tests/` | None — not touched by upstream |
| 10 | `fmc_access_rules.Read` dedup by Id | `resource_fmc_access_rules.go` (Read, not template-generated, delimited by `// mechanize-dk:patch10 begin/end`) | Low — small block, file section not under template markers |
| 11 | `fmc_ftd_manual_nat_rule` duplicate-by-index recovery | `resource_fmc_ftd_manual_nat_rule.go` (Create, hand-maintained, delimited by `// mechanize-dk:patch11 begin/end`) + `helpers/dup_recovery.go` (new) | Low — hand-edit lives outside template markers; helper is a new file |

---

## Patch 1 — Idempotent create for standard (POST) resources

### Problem

When a `terraform apply` is interrupted mid-run and re-run, or when FMC
already contains an object with the same name, the provider fails with HTTP
409 Conflict or HTTP 400 "already exists" instead of importing the existing
object into state.

### Solution

The Create function in the resource template delegates conflict resolution to
`helpers.IngestOnConflict` (Patch 5). This applies to all resources that have
`data_source_query: true` on a name attribute (~69 resources). It does **not**
apply to `PutCreate`, `IsBulk`, or `IsOverride` resources.

### File: `gen/templates/resource.go`

**Location:** immediately after the `r.client.Post(...)` / `r.client.Put(...)`
call in the `Create()` function.

Replace the original `if err != nil { resp.Diagnostics.AddError ... }` block
(which was inside `{{- if and (not .PutCreate) (hasDataSourceQuery .Attributes)}}`)
with:

```go
{{- if and (not .PutCreate) (hasDataSourceQuery .Attributes)}}
{{- $dataSourceAttribute := getDataSourceQueryAttribute .}}
var ingestID string
if err != nil {
    ingestID, res, err = helpers.IngestOnConflict(ctx, r.client, plan.getPath(), err, res,
        "{{$dataSourceAttribute.TfName}}",
        func(v gjson.Result) bool {
            return plan.{{toGoName $dataSourceAttribute.TfName}}.
                {{- if eq $dataSourceAttribute.Type "Int64" -}}ValueInt64() == v.Get("{{range $dataSourceAttribute.DataPath}}{{.}}.{{end}}{{$dataSourceAttribute.ModelName}}").Int()
                {{- else -}}ValueString() == v.Get("{{range $dataSourceAttribute.DataPath}}{{.}}.{{end}}{{$dataSourceAttribute.ModelName}}").String(){{- end -}}
        },
        reqMods...)
    if err != nil {
        resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Failed to configure object (POST/PUT), got error: %s, %s", err, res.String()))
        return
    }
    plan.Id = types.StringValue(ingestID)
    plan.fromBodyUnknowns(ctx, res)
} else {
    plan.Id = types.StringValue(res.Get("id").String())
    plan.fromBodyUnknowns(ctx, res)
}
{{- else}}
if err != nil {
    resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Failed to configure object (POST/PUT), got error: %s, %s", err, res.String()))
    return
}
plan.Id = types.StringValue(res.Get("id").String())
plan.fromBodyUnknowns(ctx, res)
{{- end}}
```

**After applying:** run `go generate` to propagate to all generated
`resource_fmc_*.go` files.

---

## Patch 2 — Idempotent create for network groups bulk

### Problem

`fmc_network_groups` uses a custom `networkGroupsBulk.Create()` function (not
the code-generated template path). The bulk POST can fail with 409/400, or
succeed on FMC but drop the TCP connection before the response is read (EOF),
causing go-fmc to retry and get 400 "already exists". The provider must recover
all groups efficiently without doing a full paginated scan per group.

### Solution

Two changes:

**a) Wrap the bulk POST with `RetryOnParallelLock`** (Patch 4) inside
`networkGroupsBulk.Create()` in `resource_fmc_network_groups.go`:
```go
postURL := plan.getPath() + "?bulk=true"
res, err := helpers.RetryOnParallelLock(ctx, func() (gjson.Result, error) {
    return client.Post(postURL, bodies, reqMods...)
})
```

**b) Delegate conflict fallback to `networkGroupsFindOrCreate`:**

On bulk POST conflict, replace any inline fallback loop with:
```go
// Bulk failed due to conflict — fall back to "find first" idempotency.
tflog.Debug(ctx, fmt.Sprintf("%s: Bulk create conflict, falling back to individual creates", plan.Id.ValueString()))
return ret, networkGroupsFindOrCreate(ctx, plan, &ret, bulk, bodyParts, client, reqMods...)
```

### New file: `internal/provider/resource_fmc_network_groups_idempotency.go`

Create this file verbatim from the fork. It contains `networkGroupsFindOrCreate`,
which uses `?filter=nameOrValue:<name>` to look up each group individually and
either imports its ID or POSTs to create it. Key properties:

- Uses `nameOrValue` filter (substring match — exact-name check is done in code)
- One GET per group, no pagination of the full list
- `package provider` — same package as the generated file, no import needed

---

## Patch 3 — Idempotent create for access rules bulk

### Problem

Same as Patch 2 but for `fmc_access_rules`. The resource has its own
`createRulesAt()` in `resource_fmc_access_rules.go`.

### Solution

Three changes:

**a) Wrap the bulk POST with `RetryOnParallelLock`** (Patch 4) inside
`createRulesAt()`:
```go
postURL := plan.getPath() + urlParams
res, err := helpers.RetryOnParallelLock(ctx, func() (gjson.Result, error) {
    return r.client.Post(postURL, body, reqMods...)
})
```

**b) Delegate conflict fallback to `accessRulesFindOrCreate`:**

On bulk POST conflict, replace any inline fallback loop with:
```go
// Bulk failed due to conflict — fall back to "find first" idempotency.
tflog.Debug(ctx, "Access rules bulk create conflict, falling back to individual creates")
itemBodies := gjson.Parse(body).Array()
if err := accessRulesFindOrCreate(ctx, r.client, plan, &bulk, itemBodies, individualURLParams, reqMods...); err != nil {
    return err
}
state.Items = append(state.Items, bulk.Items...)
bulk.Items = bulk.Items[:0]
```

**c) Wrap the bulk DELETE with `RetryOnParallelLock`** inside `truncateRulesAt()`:
```go
urlPath := state.getPath() + "?bulk=true&filter=ids:" + url.QueryEscape(bulk)
res, err := helpers.RetryOnParallelLock(ctx, func() (gjson.Result, error) {
    return r.client.Delete(urlPath, reqMods...)
})
```

### New file: `internal/provider/resource_fmc_access_rules_idempotency.go`

Create this file verbatim from the fork. It contains `accessRulesFindOrCreate`,
which uses `?filter=name:<name>` (the filter parameter already used elsewhere
in the same file) to look up each rule individually. Same "find first" pattern
as Patch 2.

---

## Patch 4 — RetryOnParallelLock helper

### Problem

FMC returns transient errors that are safe to retry:
- **HTTP 400** with body containing `"Parallel add/update/delete operations are
  blocked"` — FMC's write-lock advisory. go-fmc does not auto-retry this.
- **EOF / connection reset** — FMC processes the request but drops the TCP
  connection before sending the response (observed on large bulk POSTs).
- **HTTP 429** — rate limit.

### Solution

Add to `internal/provider/helpers/utils.go`:

```go
import (
    // add these to existing imports:
    "math/rand"
    "time"
    fmc "github.com/netascode/go-fmc"
)

func isRetryableError(err error, res gjson.Result) bool {
    msg := err.Error()
    if strings.Contains(msg, "StatusCode 429") ||
        strings.Contains(msg, "EOF") ||
        strings.Contains(msg, "connection reset by peer") ||
        strings.Contains(msg, "connection refused") {
        return true
    }
    if strings.Contains(msg, "StatusCode 400") {
        desc := strings.ToLower(res.Get("error.messages.0.description").String())
        if strings.Contains(desc, "parallel") {
            return true
        }
    }
    return false
}

// RetryOnParallelLock retries fn on transient FMC errors (parallel lock, EOF,
// rate limit). Random 15–45 s delay, up to 10 attempts.
func RetryOnParallelLock(ctx context.Context, fn func() (gjson.Result, error)) (gjson.Result, error) {
    const maxAttempts = 10
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        res, err := fn()
        if err == nil || !isRetryableError(err, res) {
            return res, err
        }
        if attempt == maxAttempts {
            return res, err
        }
        delay := time.Duration(15+rand.Intn(31)) * time.Second
        tflog.Warn(ctx, fmt.Sprintf("FMC transient error (%s) — retrying in %s (attempt %d/%d)", err.Error(), delay, attempt, maxAttempts))
        time.Sleep(delay)
    }
    return gjson.Result{}, nil
}
```

---

## Patch 5 — IngestOnConflict helper

### Problem

The template's idempotency logic (Patch 1) was previously inlined as a 40-line
block in `gen/templates/resource.go`. This made the template diff large and
merge-conflict-prone. Extracted into a reusable helper.

### Solution

Add to `internal/provider/helpers/utils.go`:

```go
import (
    // add these to existing imports:
    "net/url"
    fmc "github.com/netascode/go-fmc"
)

// IngestOnConflict handles the idempotency case where a POST returns HTTP 409
// or HTTP 400 "already exists". It paginates the GET list endpoint until it
// finds an object matching the predicate, then fetches and returns that
// object's ID and full body.
//
// If postErr is not a conflict error, it is returned unchanged (caller fails).
// If conflict resolution succeeds, (id, body, nil) is returned.
func IngestOnConflict(
    ctx context.Context,
    client *fmc.Client,
    resourcePath string,
    postErr error,
    postRes gjson.Result,
    attrLabel string,
    matches func(gjson.Result) bool,
    reqMods ...func(*fmc.Req),
) (string, gjson.Result, error) {
    if !(strings.Contains(postErr.Error(), "StatusCode 409") ||
        (strings.Contains(postErr.Error(), "StatusCode 400") && strings.Contains(postRes.String(), "already exists"))) {
        return "", postRes, postErr
    }
    tflog.Debug(ctx, fmt.Sprintf("IngestOnConflict: object already exists (409/400), searching by %s", attrLabel))
    offset, limit, id := 0, 1000, ""
    for {
        listRes, listErr := client.Get(resourcePath+fmt.Sprintf("?limit=%d&offset=%d&expanded=true", limit, offset), reqMods...)
        if listErr != nil {
            return "", listRes, fmt.Errorf("object already exists but failed to list (GET): %w", listErr)
        }
        for _, v := range listRes.Get("items").Array() {
            if matches(v) {
                id = v.Get("id").String()
                break
            }
        }
        if id != "" || !listRes.Get("paging.next.0").Exists() {
            break
        }
        offset += limit
    }
    if id == "" {
        return "", postRes, fmt.Errorf("object already exists (conflict) but could not be found by %s: %w", attrLabel, postErr)
    }
    body, err := client.Get(resourcePath+"/"+url.QueryEscape(id), reqMods...)
    if err != nil {
        return "", body, fmt.Errorf("object already exists (conflict) but failed to retrieve it (GET): %w", err)
    }
    return id, body, nil
}
```

---

## Patch 6 — Reduce maxUrlParamLength (bulk-delete URL inflation)

### Problem

`url.QueryEscape` encodes UUID dashes (`-` → `%2D`) and commas (`,` → `%2C`),
inflating each UUID from 37 to 47 encoded characters (~27%). With
`maxUrlParamLength = 7000`, bulk DELETE batches exceed FMC's ~8KB URL limit,
causing HTTP 400 "internal error".

### Solution

In `gen/templates/provider.go`, change the `maxUrlParamLength` constant:

```go
// Before:
// maximum URL Param length. This is a rough estimate and does not account for the entire URL length.
maxUrlParamLength int = 7000

// After:
// maximum URL Param length (un-encoded). url.QueryEscape inflates UUIDs by ~27% (dashes become %2D,
// commas become %2C), so this must be sized to keep encoded batches well under FMC's ~8KB URL limit.
maxUrlParamLength int = 4500
```

After changing the template, run `go generate` to propagate to `provider.go`.

---

## Patch 7 — Code generator: preserve `imports` sections

### Problem

`gen/generator.go` replaces every template-marked section in generated files.
This overwrites file-specific `imports` sections (e.g. in
`resource_fmc_network_groups.go` and `resource_fmc_access_rules.go` which need
extra imports for our patches) every time `go generate` runs.

### Solution

In `gen/generator.go`, inside `renderTemplate()`, special-case the `imports`
section to copy existing file content verbatim:

```go
if currentSectionName == "imports" {
    // Preserve existing imports — do not replace with template output
    newContent += line + "\n"
    matches := endRegex.FindStringSubmatch(line)
    if len(matches) > 1 && matches[1] == "imports" {
        currentSectionName = ""
    }
} else {
    // normal end-marker / section-replace handling
}
```

See commit `4059b802` for the exact diff.

---

## Patch 8 — New resource: fmc_network_groups_safe

### What it does

`fmc_network_groups_safe` is a drop-in replacement for `fmc_network_groups`
that prevents accidental deletion of network groups still referenced by access
rules. Instead of hard-deleting a removed group it:

1. **Soft-deletes** — renames to `__gc_<original-FMC-ID>`, sets its sole
   literal to the harmless sentinel IP `127.6.6.6`, preserves original name
   in description as `"GC: was <original-name>"`.
2. **Garbage-collects** on every `Read()` — permanently deletes any `__gc_*`
   group that is no longer referenced by any network group or access rule.

### Files to create/add

All files listed below are new (not generated from YAML). Copy them verbatim
from the fork:

- `internal/provider/resource_fmc_network_groups_safe.go` — resource implementation
- `examples/resources/fmc_network_groups_safe/resource.tf` — required by tfplugindocs
- `docs/resources/network_groups_safe.md` — generated by `go generate`

Registration (must survive `go generate`):

**`gen/templates/provider.go`** — after the `{{- end}}` closing the generated
`Resources()` list:
```go
// Fork additions — manually implemented, not code-generated:
NewNetworkGroupsSafeResource,
```

**`gen/doc_category.go`** — in the `extraDocs` map:
```go
var extraDocs = map[string]string{
    "network_groups_safe": "Objects",
}
```

---

## Patch 9 — Tests

New test infrastructure under `tests/`. All tests are standalone shell/Python
scripts that build the provider locally and use a `dev_overrides` tfrc.
Not touched by upstream; all files survive merges.

- `tests/idempotency/` — verifies Patch 1: create objects, delete out-of-band, re-apply
- `tests/rule_position/` — verifies access rule position-change handling
- `tests/network_groups_safe/` — verifies `fmc_network_groups_safe` at 1000 groups + rules
- `tests/stress-test/` — end-to-end stress test with MITM proxy for full API visibility

---

## Patch 10 — `fmc_access_rules.Read` dedup by Id

### Problem

`fmc_access_rules.Read` refreshes state by issuing GET requests with
`?filter=name:<name1>,<name2>,…`, batching ≤2500 chars of names per call and
merging the responses. FMC's `filter=name:` does **prefix/substring** matching,
not exact match — so a query value of `stress-test-rule-1` also matches
`stress-test-rule-10`, `stress-test-rule-100`, … `stress-test-rule-199`. When
the rules list spans multiple batches, the same rule can land in more than
one batch's response, and the merge appends without deduping.

Effect:
1. `state.Items` grows beyond the configured rule count (e.g. 309 entries for
   1000 unique IDs at `--count 250`; same pattern at 1000).
2. `fromBodyPartial` resizes state.Items to match the merged response length,
   so duplicates persist in saved state.
3. Next `Update → truncateRulesAt` walks the inflated state.Items and produces
   bulk-DELETE URLs whose `filter=ids:` list contains duplicates *and* IDs
   that an earlier batch already deleted.
4. FMC returns **HTTP 404** on those batches → `terraform apply` fails.

This is a pre-existing upstream bug, not introduced by this fork; it only
surfaces at scale + with names that share a common prefix.

### Solution

Dedup `state.Items` by `Id` at the end of `Read`, immediately after
`fromBody`/`fromBodyPartial`:

```go
// internal/provider/resource_fmc_access_rules.go, end of Read()
{
    seen := make(map[string]bool, len(state.Items))
    out := state.Items[:0]
    for _, item := range state.Items {
        id := item.Id.ValueString()
        if id == "" || !seen[id] {
            seen[id] = true
            out = append(out, item)
        }
    }
    state.Items = out
}
```

Keeps the first occurrence of each Id in the slice order. No-op when there
are no duplicates.

`Read` lives outside any `//template:begin`/`//template:end` markers in
`resource_fmc_access_rules.go`, so `go generate` will not overwrite it.

### Conflict risk on sync

Low. The patch is a small block in a non-generated section of the file. If
upstream rewrites `Read` significantly, re-apply by hand.

---

## Patch 11 — `fmc_ftd_manual_nat_rule` duplicate-by-index recovery

### Problem

`fmc_ftd_manual_nat_rule` POST can fail with HTTP 400 and an error body
containing `"Duplicate entry of ManualNatRule at rule index (N) is also
present at rule index [M]"`. This is FMC's content-level duplicate
detection: the proposed rule has the same NAT tuple as the rule already
at index M. Manual NAT rules have **no `name` field**, so the existing
`IngestOnConflict` (which searches by a name attribute) cannot recover.

Typical triggers: prior partial apply, rule pre-existing in FMC GUI, or
two `for_each` keys producing identical NAT content.

### Implementation note

`fmc_ftd_manual_nat_rule`'s `Create` function is **hand-maintained** (not
inside `//template:begin create` / `//template:end create` markers). The
fork-side patch therefore lives directly in the generated file's Create
function and is preserved across `go generate` runs because the section
is outside template markers. The `imports` section is templated but
Patch 7's generator behavior preserves existing imports verbatim, so the
`"reflect"` import added by this patch also survives regeneration.

The patch block is delimited by `// mechanize-dk:patch11 begin` and
`// mechanize-dk:patch11 end` comment markers so it can be located
quickly after an upstream refactor of `Create()` — search for those
strings, re-apply if missing.

This is different from Patch 1's approach: Patch 1 modifies the resource
template (`gen/templates/resource.go`) and regenerates ~277 files. Patch
11 touches only the one resource file we actually need.

### Solution

**1) New helper** — `internal/provider/helpers/dup_recovery.go`

```go
func FindDuplicateByIndex(
    ctx context.Context,
    client *fmc.Client,
    resourcePath string,
    postErr error,
    postRes gjson.Result,
    reqMods ...func(*fmc.Req),
) (gjson.Result, error)
```

Parses the regex `Duplicate entry .* at rule index \((\d+)\)` against
both `postErr.Error()` and `postRes.String()`, capturing the **parens**
group `(N)`. FMC's error has the form

```
Duplicate entry of ManualNatRule at rule index (N) is also present at rule index [M]
```

where `(N)` is the **existing duplicate rule's 1-indexed position** in the
policy and `[M]` is the would-be position of the rejected new rule
(= total existing + 1). We want `(N)`, not `[M]`. Confirmed on a live FMC
(`fmc-tlab1-ts.virt-service.dk`, 2026-06-01) with a 5-rule + dup-R3
matrix: error reported `(3) ... [6]`, and `?offset=2&limit=1` returned R3.

If no match, returns `(empty, postErr)` (caller fails as before). On match,
GETs `<resourcePath>?expanded=true&offset=(N-1)&limit=1` (subtract one
because FMC reports 1-indexed but the list endpoint takes 0-indexed
offsets) and returns `items.0`.

Unit-tested in `internal/provider/helpers/dup_recovery_test.go`:

- `TestExtractDuplicateIndex` — regex extraction across positive and
  negative cases, including the rejection of brackets-only `[M]` messages
  (which previously matched the buggy regex).
- `TestFindDuplicateByIndex_OffsetMapping` — gock-based round-trip:
  feeds a fake `(3)...[6]` 400 error and asserts the helper issues
  `?offset=2`. Locks in the `offset = N-1` arithmetic against future
  refactors.
- `TestFindDuplicateByIndex_NoMatch_ReturnsOriginalError` — confirms the
  helper passes the original error through when the regex doesn't match.

**2) Hand-edit in `resource_fmc_ftd_manual_nat_rule.go`**

Inside the existing manually-maintained `Create` function, replace the
POST error block with the recovery flow. Diff:

```go
 res, err := r.client.Post(plan.getPath()+"?section="+strings.ToLower(plan.Section.ValueString()), body, reqMods...)
 if err != nil {
-    resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Failed to configure object (POST/PUT), got error: %s, %s", err, res.String()))
-    return
+    foundRes, dupErr := helpers.FindDuplicateByIndex(ctx, r.client, plan.getPath(), err, res, reqMods...)
+    if dupErr != nil {
+        // Pattern didn't match (dupErr == original err) or GET failed.
+        resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Failed to configure object (POST/PUT), got error: %s, %s", dupErr, res.String()))
+        return
+    }
+    // Compare against the plan using fromBodyPartial: only fields the user
+    // explicitly set in HCL are compared against FMC's stored values.
+    // Unmanaged attributes (Bool defaults like `unidirectional`, `enabled`,
+    // computed fields like `type`) are not part of the comparison.
+    foundID := types.StringValue(foundRes.Get("id").String())
+    plan.Id = foundID
+    plan.fromBodyUnknowns(ctx, foundRes)
+    existing := plan
+    existing.fromBodyPartial(ctx, foundRes)
+    if !reflect.DeepEqual(plan, existing) {
+        resp.Diagnostics.AddError("Client Error", fmt.Sprintf("FMC reported a duplicate object, but the existing object's content differs from the proposed body. Original error: %s", err))
+        return
+    }
+    res = foundRes
 }
```

Add `"reflect"` to the `import` block above (the `template:begin imports`
section is preserved by Patch 7).

### Comparison rationale

The first cut of this patch used `reflect.DeepEqual(planCopy, existing)`
after a fresh `existing.fromBody(ctx, foundRes)`. That implementation
always reported "content differs" because:

- The fresh `existing` had FMC-emitted defaults populated for every Bool
  field (`unidirectional=false`, `enabled=false`, `interfaceIpv6=false`,
  …), while the user's `plan` had those as `types.BoolNull()`.
- The plan's computed fields (`Id`, `Type`) were `Unknown` while `existing`
  had concrete FMC values.
- The plan had `FtdNatPolicyId` set (from HCL), but `fromBody` does not
  populate that field at all, so `existing.FtdNatPolicyId` stayed `Null`.

The corrected approach narrows the comparison to fields the user actually
manages:

1. Set `plan.Id` from FMC's id and call `plan.fromBodyUnknowns` so the
   plan's `Unknown` computed fields take FMC's values.
2. Build `existing := plan` — a copy with the same user-set values and
   same computed values.
3. Call `existing.fromBodyPartial(ctx, foundRes)`, which only updates
   non-null fields from the FMC response. Null plan fields stay null on
   the existing side. User-set fields get overwritten with FMC's actual
   value — any divergence here is a real content mismatch we surface.

Mismatch → original FMC error surfaces and the user resolves the
ambiguity explicitly. Two `for_each` HCL keys producing byte-identical
content **will** both import the same FMC ID; the resulting
double-ownership becomes visible on the next destroy/apply cycle. This is
the deliberate fork philosophy: opinionated provider, no opt-in flag.

### Test coverage

`tests/idempotency/run_test.sh` — Test 8 — pre-creates a manual NAT rule
via the FMC REST API and runs `terraform apply` against matching HCL.

**Device-assignment prerequisite (important):** FMC's content-level dedup
validation **only fires when the NAT policy is assigned to an FTD device.**
Without an assignment, FMC accepts duplicate POSTs silently (HTTP 201) and
Patch 11's recovery path never runs. Verified on the lab FMC on
2026-06-01: a probe matrix of six body shapes (minimal SNAT through full
tuple with security zones) all returned 201 without an assignment; the
exact same matrix all returned 400 with the duplicate error once the
policy was bound to `ftd-tlab-1`.

Accordingly, Test 8 looks up any FTD device on the FMC via
`/devices/devicerecords?limit=1`, assigns the test NAT policy to it via
`/assignment/policyassignments`, then pre-creates the rule. After the
test, the policy is unassigned (PUT with empty targets; DELETE is not
allowed on the assignment endpoint — returns 405) before Terraform
destroys the policy.

Outcomes:

- Device exists + FMC fires the duplicate validation: Patch 11's recovery
  imports the existing rule → **TEST 8 PASSED**.
- Device exists + FMC silently accepts both (unusual at this point but
  guarded against): **TEST 8 SKIPPED** with a yellow ⚠ banner.
- No FTD device registered on the FMC: **TEST 8 SKIPPED** with a yellow
  ⚠ banner explaining that the helper logic is covered by
  `dup_recovery_test.go` instead.

Either skip outcome leaves FMC in a clean state.
The recovery code itself is exercised by the regex-extraction unit tests.

---

## Patch 12 — Bulk NAT tenant resources (`fmc_mze_manual_nat_rules` + `fmc_mze_auto_nat_rules`)

**Why:** Multiple tenants sharing one FTD NAT policy need tenant-scoped bulk
resources with cooperative ownership. The single-rule upstream resources
(`fmc_ftd_manual_nat_rule`, `fmc_ftd_auto_nat_rule`) don't express
intra-tenant ordering or tenant boundaries.

**Going-forward naming convention:** fork-owned resources use the
`fmc_mze_*` prefix (`mze` for Mechanize) so they're visually distinguishable
from upstream resources at the HCL level. `fmc_network_groups_safe` (Patch 8)
predates this convention and is **not** renamed — Terraform has no aliasing
for resource type names, so a rename would break every existing user.

**Files (all hand-written, all outside template markers — survive `go generate`):**
- `internal/provider/resource_fmc_mze_manual_nat_rules.go` — bulk manual-NAT
  resource: ordered `before_auto` + `after_auto` lists, tenant-chosen `key`
  per item, custom diff-by-key in `Update` (DELETE → PUT → POST in that order
  to handle reorders + body changes + adds while letting other tenants'
  rules coexist).
- `internal/provider/resource_fmc_mze_auto_nat_rules.go` — bulk auto-NAT
  resource: unordered `map(rules)` (FMC sorts auto-NAT by specificity), map
  key is the tenant identifier.
- `internal/provider/helpers/nat_match.go` — `MatchOn` evaluator: `Validate`
  (plan-time conflict check), `AutoFill` (Create-time injection), `Hash`
  (16-char prefix used in the synthetic resource ID).
- `internal/provider/helpers/nat_match_test.go` — unit tests for the above.
- `internal/provider/helpers/dup_recovery_autonat.go` — auto-NAT duplicate-
  recovery helper, **Branch B-prime**: detects FMC's "Duplicate Auto NAT
  rule" marker phrase and uses a caller-supplied `originalNetworkID` to
  search the section. (The error body doesn't carry the existing rule's
  UUID; the resource always knows what `originalNetwork.id` it just POSTed,
  so the helper takes that as a parameter.)
- `internal/provider/helpers/dup_recovery_autonat_test.go` — unit tests for
  the marker detector.

**Files modified — re-apply on upstream merges if upstream touches them:**
- `gen/templates/provider.go` — added `NewMzeManualNatRulesResource,` and
  `NewMzeAutoNatRulesResource,` after the existing `NewNetworkGroupsSafeResource,`
  line in the `Resources()` function. Same survival mechanism as Patch 8.
- `gen/doc_category.go` — added two entries to `extraDocs`:
  ```
  "mze_manual_nat_rules": "Policies",
  "mze_auto_nat_rules":   "Policies",
  ```

**Examples (required for tfplugindocs):**
- `examples/resources/fmc_mze_manual_nat_rules/resource.tf`
- `examples/resources/fmc_mze_auto_nat_rules/resource.tf`

**Tests (survive merges; live entirely under `tests/`):**
- `tests/bulk_nat_tenants/run_test.sh` and supporting `.tf` files
- `tests/bulk_nat_tenants/probe_autonat_dup.sh` — one-shot probe used to
  determine the auto-NAT duplicate-error shape
- `tests/bulk_nat_tenants/PROBE_AUTONAT.md` — captured probe result and
  recovery-shape decision (Branch B-prime)

**Reuses Patch 11:** `helpers.FindDuplicateByIndex` is called from
`fmc_mze_manual_nat_rules` Create/Update to recover from duplicate-by-index
errors. Manual-NAT and auto-NAT duplicate-recovery helpers live in separate
files so future changes to one don't disturb the other.

**Spec and plan:**
- `docs/superpowers/specs/2026-06-04-bulk-nat-tenant-resources-design.md`
- `docs/superpowers/plans/2026-06-04-bulk-nat-tenant-resources.md`

---

## Re-applying after upstream sync

After `git merge origin/main` (or rebase):

1. **Patch 1** — verify `gen/templates/resource.go` still has the
   `helpers.IngestOnConflict` call block. If upstream changed the Create
   function in the template, re-apply the ~20-line block. Then run
   `go generate`.

2. **Patches 2 & 3** — the `_idempotency.go` files are new, they survive
   merges. Only check that the 2-line call sites remain in
   `resource_fmc_network_groups.go` and `resource_fmc_access_rules.go`.
   These sites are outside template markers so they survive `go generate`,
   but a merge might remove them if upstream reworked the same lines.

3. **Patches 4 & 5** — `helpers/utils.go` is not generated; survives merges.

4. **Patch 6** — verify `gen/templates/provider.go` still has
   `maxUrlParamLength int = 4500`. Then run `go generate`.

5. **Patch 7** — `gen/generator.go` is not generated; survives merges.

6. **Patch 8** — `resource_fmc_network_groups_safe.go` is not generated;
   survives merges. Check `gen/templates/provider.go` and `gen/doc_category.go`
   after any upstream change to those files.

7. **Patch 10** — `resource_fmc_access_rules.go` `Read()` is not under template
   markers; survives `go generate`. After an upstream merge, check that the
   dedup block at the end of `Read()` (just before `resp.State.Set`) still
   exists. If upstream reworked `Read`, re-apply the ~12-line block from the
   "Patch 10" section above.

8. **Patch 11** — `resource_fmc_ftd_manual_nat_rule.go`'s `Create()` is
   hand-maintained (not inside `template:begin create` markers); the edit
   survives `go generate`. After an upstream merge, verify the recovery block
   in Create is intact and that `"reflect"` is still listed in the imports
   block. If upstream reworked Create, re-apply the diff from the "Patch 11"
   section above. `internal/provider/helpers/dup_recovery.go` and
   `dup_recovery_test.go` are new files and survive merges.
   `tests/idempotency/main.tf` and `run_test.sh` additions survive merges too.

7. **Patch 9** — `tests/` directory not touched by upstream; survives merges.

9. **Patch 12** — `resource_fmc_mze_manual_nat_rules.go`,
   `resource_fmc_mze_auto_nat_rules.go`, `helpers/nat_match.go`, and
   `helpers/dup_recovery_autonat.go` are all hand-written and never under
   template markers — they survive `go generate`. After an upstream merge,
   check that `gen/templates/provider.go` still has the two
   `NewMze…Resource,` entries after `NewNetworkGroupsSafeResource,` and that
   `gen/doc_category.go` still has both `mze_*_nat_rules` entries in
   `extraDocs`. If upstream reworked either file, re-apply the changes from
   the "Patch 12" section above. `tests/bulk_nat_tenants/` is in `tests/`
   and survives by virtue of Patch 9.
