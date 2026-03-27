# STRESS.md — stress testing notes

This file documents the stress-testing campaign run against `fmc_network_groups_safe`
at `--count 1000` (1000 network groups + 1000 access rules). It records every
problem we hit, how we diagnosed it, and what we changed to fix it.

---

## The test

`tests/stress-test/run_test.py` is an end-to-end test equivalent to test 3 in
`tests/network_groups_safe/run_test.sh` but at a configurable scale. It:

1. Authenticates with FMC and does a hard-cleanup of any leftover test objects.
2. Starts a local TLS MITM proxy on port 63323. **Every HTTP request and
   response from both Terraform and the Python cleanup code is logged to
   `/tmp/logfile`.** This was essential for diagnosing all the issues below.
3. Builds the provider binary from source.
4. `terraform apply` — creates N network groups (`fmc_network_groups_safe`) and
   N access rules (`fmc_access_rules`) with each rule targeting one group.
5. `terraform apply` (partial config) — removes group-1 and rule-1. Expects
   the group to be soft-deleted (`__gc_…`) rather than hard-deleted.
6. Verifies a `__gc_` group exists in FMC.
7. `terraform apply` again (GC pass) — expects the `__gc_` group to be
   permanently deleted now that nothing references it.
8. Verifies no `__gc_` groups remain.
9. Hard-cleanup.

Run:
```
cd tests/stress-test
python3 run_test.py --url https://192.168.1.169 -u claude -p c1aud3 \
    --terraform /home/carfan/Documents/aci3/terraform --count 1000
```

---

## Problem 1 — Bulk POST EOF: FMC creates objects but drops the connection

### Symptom

The bulk POST of 1000 network groups took ~30 seconds. The test would then
hang for many more minutes and eventually the proxy log showed:

```
POST /object/networkgroups?bulk=true → (no response)
... long pause ...
POST /object/networkgroups → 400 {"description": "A network group with the name \"stress-test-group-42\" already exists."}
POST /object/networkgroups → 400 {"description": "A network group with the name \"stress-test-group-43\" already exists."}
...  (repeated 1000 times)
```

### Root cause

FMC successfully processed the bulk POST and created all 1000 groups, but
dropped the TCP connection before sending the HTTP response (likely a server-
side timeout on very large payloads). go-fmc saw the EOF, retried the same
request, and got HTTP 400 "already exists" on the retry since the groups were
already created. The provider's idempotency code kicked in and did one GET per
group to recover — 1000 individual paginated list scans.

### Why recovery was so slow (O(N²))

The old idempotency fallback for `fmc_network_groups` was:
1. Try POST → get 400 "already exists"
2. Paginate GET `?limit=1000&offset=0`, `?offset=1000`, … until name found
3. Record the found ID

With 1000 groups and ~1739 total groups on FMC, each scan required 2 GET
pages. 1000 groups × 2 pages × ~1s per page = ~33 minutes.

### Fix (Patch 2 — "find first" with nameOrValue filter)

Replaced the paginate-all approach with a targeted filter query. For each
group name, instead of scanning all groups, use:

```
GET /object/networkgroups?limit=1000&expanded=true&filter=nameOrValue:<name>
```

FMC's `nameOrValue` filter does substring matching and returns only groups
whose name contains the search string. For a name like `stress-test-group-1`,
this returns ~112 results (all groups containing "1" in their number) instead
of 1739. An exact-name check `v.Get("name").String() == name` finds the right
one. All results fit within limit=1000 so no pagination is needed.

Further, the fallback was changed from "try POST → fail → find" to "find first
→ POST only if not found". This avoids the wasted POST round-trip entirely.

The same fix was applied to `fmc_access_rules` using `filter=name:<name>`.

---

## Problem 2 — FMC parallel-lock: HTTP 400 "Parallel operations blocked"

### Symptom

Occasionally, a POST or DELETE would fail with:

```
HTTP 400 {"error":{"messages":[{"description":"Parallel add/update/delete
operations are blocked. Please retry the request."}]}}
```

The provider immediately returned this as a fatal error instead of retrying.

### Root cause

FMC has a write-lock mechanism. When multiple concurrent write operations are
in flight, it rejects new ones with HTTP 400 and the above body. This is a
transient advisory — the request should be retried after a delay.

go-fmc auto-retries on HTTP 429 and 5xx, and also auto-retries HTTP 400 if
the body contains `"please try again"` or `"retry the operation after
sometime"`. The phrase `"Please retry the request."` did **not** match either
pattern, so go-fmc did not retry and returned the error immediately.

We initially thought this was HTTP 429 (the CLAUDE.md said so), but the proxy
log clearly showed HTTP 400 with the "Parallel" body every time.

### Fix (Patch 4 — RetryOnParallelLock)

Added `helpers.RetryOnParallelLock(ctx, fn)` — a wrapper that retries `fn` up
to 10 times when the error is transient. The retry predicate `isRetryableError`
checks:

- `"StatusCode 429"` — standard rate limit
- `"EOF"` / `"connection reset by peer"` / `"connection refused"` — transport errors
- `"StatusCode 400"` **and** `res.Get("error.messages.0.description")` contains
  `"parallel"` (case-insensitive) — the parallel-lock advisory

A random 15–45 second delay is used between attempts so concurrent provider
instances don't all retry at the same time.

`RetryOnParallelLock` wraps the bulk POST in `networkGroupsBulk.Create` and
the bulk DELETE in `truncateRulesAt` in `resource_fmc_access_rules.go`.

---

## Problem 3 — Proxy MITM tunnel closed between bulk-delete batches

### Symptom

The Python cleanup code (`delete_network_groups_bulk`) deletes groups in
batches of 50. The first batch would succeed but the second batch would fail
with:

```
RemoteDisconnected('Remote end closed connection without response')
```

### Root cause

Each bulk DELETE to FMC takes ~20–30 seconds (FMC processes the batch before
responding). The MITM proxy's CONNECT tunnel is kept alive for the duration of
each request. After the response arrives and the tunnel is idle, the proxy
closes the underlying socket. The `requests.Session` object tried to reuse the
stale pooled connection for the next batch, hitting the already-closed socket.

### Fix

Recreate the `requests.Session` before each batch in `delete_network_groups_bulk`:

```python
for i in range(0, len(ids), 50):
    self._session.close()
    self._session = requests.Session()
    self._session.verify = False
    self._session.proxies = {"http": PROXY_ADDR, "https": PROXY_ADDR}
    self._session.headers.update({"X-auth-access-token": self.token})
    resp = self._session.delete(...)
```

A new Session creates a fresh connection pool, forcing a new CONNECT tunnel
for each batch.

---

## Problem 4 — Bulk DELETE HTTP 400 "internal error" at 1000 rules

### Symptom

When deleting 1000 access rules, the bulk DELETE would return:

```
HTTP 400 {"error":{"messages":[{"description":"An internal error occurred."}]}}
```

### Root cause

The provider computes batch sizes using un-encoded character counts via
`maxUrlParamLength`. However, `url.QueryEscape` encodes UUID dashes
(`-` → `%2D`, +2 chars per dash, 4 dashes per UUID = +8 chars) and commas
(`,` → `%2C`, +2 chars). Each UUID goes from 37 characters to 47 encoded
characters — a ~27% inflation.

With `maxUrlParamLength = 7000`, a batch of ~189 UUIDs has an un-encoded
length of 7,000 but an encoded length of ~8,883 — exceeding FMC's URL limit.

### Fix (Patch 6 — maxUrlParamLength)

Reduced `maxUrlParamLength` from 7000 to 4500 in `gen/templates/provider.go`.
At 4500 un-encoded, the encoded length is ~5,715, well within FMC's limit.
The constant is set in the template (not just the generated file) so it
survives `go generate`.

---

## Problem 5 — `-parallelism=1` causing unnecessarily slow applies

### Symptom

Early test runs used `-parallelism=1` in all `terraform apply` and
`terraform destroy` calls. With 3 resources (ACP, network groups, access rules)
this doesn't matter much, but it was added by mistake and masked potential
concurrency issues.

### Fix

Removed all `-parallelism=1` flags from `tf()` calls in `run_test.py`. Applies
and destroys now use Terraform's default parallelism (10).

---

## Proxy architecture

The test proxy is a pure-Python MITM proxy implemented in `run_test.py`. It:

- Listens on `127.0.0.1:63323` as an HTTP proxy.
- Handles `CONNECT` requests by establishing a TLS connection to FMC and
  presenting a self-signed certificate to Terraform.
- Uses `_blind_tunnel` (a `select`-based relay loop) to pass data between
  Terraform and FMC once the CONNECT tunnel is established.
- Logs every `>>>` request line and `<<<` response status to stderr (INFO
  level) and to `/tmp/logfile` (DEBUG level, includes body snippets).

Both Terraform and the Python FMC API calls are routed through the proxy by
setting `TF_CLI_CONFIG_FILE` to a tfrc with `HTTPS_PROXY=http://127.0.0.1:63323`
and setting `proxies={"https": "http://127.0.0.1:63323"}` on the requests
Session respectively.

The proxy was the single most useful diagnostic tool — every API call visible
in one timestamped log, with request and response bodies, made all of the
above issues diagnosable within minutes of observing them.

---

## Key FMC API observations

- **nameOrValue filter** — `GET /object/networkgroups?filter=nameOrValue:<substring>`
  does substring matching. Confirmed working: `nameOrValue:stress-test-group-1`
  returns 112 groups (all whose name contains "1"), not all 1739. Exact-name
  matching must be done in code.

- **filter=name: for access rules** — `GET .../accessrules?filter=name:<name>`
  also works and is already used elsewhere in `resource_fmc_access_rules.go`.
  Not the same as `nameOrValue` — the `name:` filter is an exact-name filter
  on the rules endpoint.

- **Parallel lock is HTTP 400, not 429** — CLAUDE.md originally said HTTP 429.
  The proxy log showed every parallel-lock response was HTTP 400. go-fmc's
  auto-retry logic did not cover it.

- **EOF on large POSTs** — FMC creates all objects from a large bulk POST but
  then drops the TCP connection before responding. go-fmc retries internally
  but the retry gets 400 "already exists". Provider-level idempotency must
  handle this. The EOF retry inside go-fmc is separate from our
  `RetryOnParallelLock`.

- **Bulk DELETE URL limit** — FMC enforces an ~8KB URL limit. The
  `filter=ids:<uuid>,<uuid>,...` parameter grows quickly once URL-encoded.

---

## Current test status

The stress test at `--count 1000` was interrupted before completing a full
clean run (laptop left the local network). All unit and functional tests pass:

- `tests/idempotency/run_test.sh` — all 7 tests pass ✓
- `tests/rule_position/run_test.sh` — passes ✓
- `tests/network_groups_safe/run_test.sh --count 10` — all 5 tests pass ✓

The stress test (`--count 1000`) is tracked in TODO.md.
