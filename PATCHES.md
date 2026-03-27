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

7. **Patch 9** — `tests/` directory not touched by upstream; survives merges.
