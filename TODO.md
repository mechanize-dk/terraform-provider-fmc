# TODO

## Sync with upstream CiscoDevNet/terraform-provider-fmc

Rebase or merge the fork onto the latest upstream `main`. The fork diverged at
upstream commit `701b7f56` (v2.0.1).

**Steps:**
1. Fetch upstream: `git fetch origin`
2. Rebase: `git rebase origin/main` (or merge — rebase preferred for a clean
   history)
3. Resolve conflicts using `PATCHES.md` as the reference for every change we
   own.
4. Run `go generate` (with `terraform` in PATH) to regenerate all files, then
   verify `git diff` matches what `PATCHES.md` describes.
5. Build and run the stress test (`tests/stress-test/run_test.py --count 1000`)
   to confirm everything still works.
6. Push to `mechanize` remote and tag a new release if appropriate.

See `PATCHES.md` for a detailed description of every patch that must survive
the sync.

---

## Commit and test pending changes

Several changes are implemented but not yet committed or fully tested:

- `internal/provider/helpers/utils.go` — `RetryOnParallelLock` + `isRetryableError`
- `internal/provider/resource_fmc_network_groups.go` — "find first" with `nameOrValue` filter + `RetryOnParallelLock` on bulk POST
- `internal/provider/resource_fmc_access_rules.go` — "find first" with `name:` filter + `RetryOnParallelLock` on bulk POST and DELETE
- `internal/provider/provider.go` — `maxUrlParamLength` reduced from 7000 → 4500

**Next step:** Run the `--count 1000` stress test against FMC, then commit
once it passes.
