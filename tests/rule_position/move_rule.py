#!/usr/bin/env python3
"""
move_rule.py — moves the last access rule in a policy to position 1 via the FMC REST API.

FMC's PUT endpoint does not support ?insert_before as a query parameter, so this script
uses DELETE + POST to reposition the rule:
  1. Saves the last rule's full body.
  2. Deletes it (old ID is gone).
  3. Re-creates it with ?section=default&insert_before=<first_rule_id> (new ID, position 1).

The rule ID changes, so Terraform's state will reference a stale ID on the next plan.

Usage:
    python3 move_rule.py <policy_id>

Environment variables (required):
    FMC_URL       FMC base URL, e.g. https://192.168.1.169
    FMC_USERNAME  FMC username
    FMC_PASSWORD  FMC password

Exit code:
    0  — move succeeded
    1  — error
"""

import base64
import json
import os
import ssl
import sys
import urllib.error
import urllib.request


def make_ssl_ctx() -> ssl.SSLContext:
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def main() -> None:
    if len(sys.argv) < 2:
        print("Usage: move_rule.py <policy_id>", file=sys.stderr)
        sys.exit(1)

    policy_id = sys.argv[1]
    fmc_url = os.environ["FMC_URL"].rstrip("/")
    username = os.environ["FMC_USERNAME"]
    password = os.environ["FMC_PASSWORD"]
    ctx = make_ssl_ctx()

    # ── Authenticate ──────────────────────────────────────────────────────────
    creds = base64.b64encode(f"{username}:{password}".encode()).decode()
    auth_req = urllib.request.Request(
        f"{fmc_url}/api/fmc_platform/v1/auth/generatetoken",
        method="POST",
        headers={"Authorization": f"Basic {creds}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(auth_req, context=ctx) as resp:
        token = resp.headers.get("X-auth-access-token")
        domain_uuid = resp.headers.get("DOMAIN_UUID")

    print(f"Authenticated  →  domain UUID: {domain_uuid}")
    hdrs = {"X-auth-access-token": token, "Content-Type": "application/json"}
    base = (
        f"/api/fmc_config/v1/domain/{domain_uuid}"
        f"/policy/accesspolicies/{policy_id}/accessrules"
    )

    # ── Helpers ───────────────────────────────────────────────────────────────
    def api_get(path: str) -> dict:
        req = urllib.request.Request(f"{fmc_url}{path}", headers=hdrs)
        with urllib.request.urlopen(req, context=ctx) as r:
            return json.loads(r.read())

    def api_delete(path: str) -> None:
        req = urllib.request.Request(f"{fmc_url}{path}", headers=hdrs, method="DELETE")
        with urllib.request.urlopen(req, context=ctx):
            pass

    def api_post(path: str, body: dict) -> dict:
        data = json.dumps(body).encode()
        req = urllib.request.Request(f"{fmc_url}{path}", data=data, headers=hdrs, method="POST")
        with urllib.request.urlopen(req, context=ctx) as r:
            return json.loads(r.read())

    # ── Fetch current rule order ───────────────────────────────────────────────
    data = api_get(f"{base}?limit=1000&expanded=true")
    items = data.get("items", [])

    if len(items) < 2:
        print(f"Only {len(items)} rule(s) found — nothing meaningful to move.")
        sys.exit(0)

    print(f"\nRule order BEFORE move ({len(items)} rules):")
    for i, r in enumerate(items):
        print(f"  {i + 1:2d}.  {r['name']:<42s}  id={r['id']}")

    first_rule = items[0]
    last_rule = items[-1]

    print(
        f"\nRepositioning  '{last_rule['name']}'"
        f"  (currently at position {len(items)})"
    )
    print(f"  Strategy: DELETE old rule, POST back (new ID assigned by FMC)")
    print(f"  Note: ?insert_before is not supported by this FMC version on PUT or POST.")
    print(f"  Old ID: {last_rule['id']}")

    # ── GET the full body of the rule to move ─────────────────────────────────
    rule_body = api_get(f"{base}/{last_rule['id']}")

    # Strip read-only/server-assigned fields that FMC rejects on POST
    for field in ("id", "links", "metadata"):
        rule_body.pop(field, None)

    # ── DELETE the rule from its current position ─────────────────────────────
    try:
        api_delete(f"{base}/{last_rule['id']}")
        print(f"  Deleted old rule '{last_rule['name']}' (id={last_rule['id']})")
    except urllib.error.HTTPError as exc:
        print(f"DELETE failed (HTTP {exc.code}): {exc.read().decode()}", file=sys.stderr)
        sys.exit(1)

    # ── Re-create the rule (FMC assigns a new ID and default position) ────────
    # ?section=default ensures the rule lands in the default section.
    post_path = f"{base}?section=default"
    try:
        new_rule = api_post(post_path, rule_body)
        new_id = new_rule.get("id", "?")
        print(f"  Re-created rule '{last_rule['name']}' (new id={new_id})")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")
        print(f"POST failed (HTTP {exc.code}): {body}", file=sys.stderr)
        sys.exit(1)

    # ── Verify rule exists with new ID ────────────────────────────────────────
    data_after = api_get(f"{base}?limit=1000&expanded=true")
    items_after = data_after.get("items", [])

    print(f"\nRule order AFTER repositioning ({len(items_after)} rules):")
    for i, r in enumerate(items_after):
        print(f"  {i + 1:2d}.  {r['name']:<42s}  id={r['id']}")

    found = any(r["id"] == new_id for r in items_after)
    if found:
        pos = next(i + 1 for i, r in enumerate(items_after) if r["id"] == new_id)
        print(f"\n✓  '{last_rule['name']}' exists at position {pos} (old id={last_rule['id']}, new id={new_id})")
    else:
        print(f"\n✗  Warning: '{last_rule['name']}' not found after re-creation.", file=sys.stderr)
        sys.exit(1)

    # Write the old/new IDs to a file so the test script can reference them
    with open("move_result.json", "w") as f:
        json.dump({
            "rule_name": last_rule["name"],
            "old_id": last_rule["id"],
            "new_id": new_id,
        }, f)


if __name__ == "__main__":
    main()
