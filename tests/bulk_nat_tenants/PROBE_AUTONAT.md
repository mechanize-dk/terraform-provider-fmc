# Auto-NAT duplicate-error probe — observed result

Probe date: 2026-06-04
Probe target: https://fmc-tlab1-ts.virt-service.dk
Probe script: `tests/bulk_nat_tenants/probe_autonat_dup.sh`

## Captured second-POST response (HTTP 400)

```
HTTP/2 400
content-type: application/json
...

{"error":{"category":"FRAMEWORK","messages":[{"description":"The Auto NAT rule with the original source already exists Duplicate Auto NAT rule is not allowed"}],"severity":"ERROR"}}
```

## Analysis

The error body **does not** name:
- (a) The existing rule's UUID — no UUID anywhere in the body.
- (b) The original-network object's name or ID — only the generic phrase "with the original source".

So neither the writing-plans Branch A (parse UUID) nor a direct Branch B (parse originalNetwork.id from the error) applies.

**However:** the recovery still works cleanly. The caller (the resource's Create/Update method) **already knows** the `originalNetwork.id` it just POSTed — that value is in the request payload. So the duplicate-detection helper only needs to do two things:

1. Detect that the response is a duplicate (a yes/no test on the error body).
2. The caller then searches the section for a rule whose `originalNetwork.id` equals the value it tried to POST, and adopts it.

This pushes the "what to look up" into the caller (where the request context lives) and keeps the helper minimal.

## Decision

**Selected branch: B-prime (modified Branch B)** — duplicate detection via the error body's marker phrase; lookup by `originalNetwork.id` supplied by the caller from its POST payload.

**Marker phrase to match (regex-friendly):** `Duplicate Auto NAT rule` (uppercase D, present in the captured response). Also matches `"already exists"` for redundancy; pin on the more specific phrase first.

**Recovery code path to implement in Task 1.5:**
- `extractAutoNatDuplicateMarker(body string) bool` — returns true iff the response is a duplicate.
- `FindDuplicateAutoNatRule(ctx, client, resourcePath, originalNetworkID, postErr, postRes, reqMods...)` — when the marker matches, GETs `resourcePath?expanded=true&limit=1000` and returns the first item whose `originalNetwork.id == originalNetworkID`. If the marker doesn't match, returns the original `postErr` unchanged.

The plan's Task 1.5 Step 4 needs to be adapted from "Branch A" to this shape — the helper signature gains an `originalNetworkID string` parameter, and the parser is a boolean detector rather than an ID extractor.
