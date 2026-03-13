#!/usr/bin/env python3
"""
move_rule.py — moves the last access rule in a policy to position 1 via the FMC REST API.

Usage:
    python3 move_rule.py <policy_id>

Environment variables (required):
    FMC_URL       FMC base URL, e.g. https://192.168.1.169
    FMC_USERNAME  FMC username
    FMC_PASSWORD  FMC password
"""

import base64
import json
import os
import ssl
import sys
import urllib.error
import urllib.request


def make_ssl_ctx() -> ssl.SSLContext:
    """Return an SSL context that skips certificate verification (mirrors curl -k)."""
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
        headers={
            "Authorization": f"Basic {creds}",
            "Content-Type": "application/json",
        },
    )
    with urllib.request.urlopen(auth_req, context=ctx) as resp:
        token = resp.headers.get("X-auth-access-token")
        domain_uuid = resp.headers.get("DOMAIN_UUID")

    print(f"Authenticated  →  domain UUID: {domain_uuid}")
    hdrs = {"X-auth-access-token": token, "Content-Type": "application/json"}

    # ── Helper functions ──────────────────────────────────────────────────────
    def api_get(path: str) -> dict:
        req = urllib.request.Request(f"{fmc_url}{path}", headers=hdrs)
        with urllib.request.urlopen(req, context=ctx) as r:
            return json.loads(r.read())

    def api_put(path: str, body: dict) -> dict:
        data = json.dumps(body).encode()
        req = urllib.request.Request(f"{fmc_url}{path}", data=data, headers=hdrs, method="PUT")
        with urllib.request.urlopen(req, context=ctx) as r:
            return json.loads(r.read())

    base = (
        f"/api/fmc_config/v1/domain/{domain_uuid}"
        f"/policy/accesspolicies/{policy_id}/accessrules"
    )

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
        f"\nMoving  '{last_rule['name']}'  →  position 1"
        f"  (inserting before '{first_rule['name']}')"
    )

    # ── GET the full body of the rule we want to move ─────────────────────────
    rule_body = api_get(f"{base}/{last_rule['id']}")

    # ── PUT the rule to its new position ──────────────────────────────────────
    # FMC accepts ?insert_before=<id> on PUT to reposition a rule in the list.
    move_path = f"{base}/{last_rule['id']}?insert_before={first_rule['id']}"
    try:
        api_put(move_path, rule_body)
        print("Move PUT succeeded.")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")
        print(f"Move PUT failed (HTTP {exc.code}): {body}", file=sys.stderr)
        sys.exit(1)

    # ── Verify new order ───────────────────────────────────────────────────────
    data_after = api_get(f"{base}?limit=1000&expanded=true")
    items_after = data_after.get("items", [])

    print(f"\nRule order AFTER move ({len(items_after)} rules):")
    for i, r in enumerate(items_after):
        print(f"  {i + 1:2d}.  {r['name']:<42s}  id={r['id']}")

    if items_after[0]["id"] == last_rule["id"]:
        print(f"\n✓  '{last_rule['name']}' is now at position 1.")
    else:
        print(
            f"\n✗  Warning: '{last_rule['name']}' is NOT at position 1 after the move.",
            file=sys.stderr,
        )
        sys.exit(1)


if __name__ == "__main__":
    main()
