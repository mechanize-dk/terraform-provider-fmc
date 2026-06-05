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
5. Build and run **all** test suites to confirm every patch still works:
   - `tests/idempotency/run_test.sh` (covers Patches 1, 5; bulk variants of 2, 3)
   - `tests/rule_position/run_test.sh` (access rule position-change handling)
   - `tests/network_groups_safe/run_test.sh --count 10` (Patch 8, fast)
   - `tests/network_groups_safe/run_test.sh --count 1000` (Patch 8 at scale)
   - `tests/stress-test/run_test.py --count 1000` (Patches 2, 3, 4, 6, 8, 10
     end-to-end with MITM proxy)

   Each must reach a clean RESULT: PASSED. The stress test is the strongest
   signal because it exercises bulk paths at scale through the proxy.
6. Push to `mechanize` remote and tag a new release if appropriate.

See `PATCHES.md` for a detailed description of every patch that must survive
the sync.
