#!/usr/bin/env python3
"""
stress_test.py — Stress test for fmc_network_groups_safe (equivalent to
test 3 in tests/network_groups_safe/run_test.sh).

All traffic — both the Python FMC API calls and Terraform's HTTP calls —
is routed through a local TLS MITM proxy on port 63323.  The proxy logs
every request and response header to /tmp/logfile so the full API dialogue
is visible in one place.

What the test does:
  1. Hard-cleanup: delete any leftover test objects in FMC.
  2. Start MITM proxy thread on port 63323 (daemon — dies with the process).
  3. Build the Terraform provider binary from the repository.
  4. Apply full config  : N fmc_network_groups_safe + N fmc_access_rules.
  5. Apply partial config: remove group-1 and rule-1.
     Expected: apply succeeds; group-1 is soft-deleted (renamed __gc_…).
  6. Verify: a __gc_ group exists in FMC.
  7. Apply again (GC pass): the __gc_ group should now be deleted.
  8. Verify: no __gc_ groups remain.
  9. Hard-cleanup: delete all test objects from FMC.

Usage:
    python3 run_test.py -u <user> -p <pass> --url https://<fmc> \\
        [--terraform /path/to/terraform] [--count N]
"""

import argparse
import io
import json
import logging
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
import socketserver

try:
    import requests
    import urllib3

    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)
except ImportError:
    sys.exit(
        "ERROR: 'requests' library is required.  "
        "Install with: pip install requests"
    )

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

PROXY_PORT = 63323
LOG_FILE = "/tmp/logfile"
GROUP_PREFIX = "stress-test-group"
RULE_PREFIX = "stress-test-rule"
ACP_NAME = "stress-test-acp"
PROXY_ADDR = f"http://127.0.0.1:{PROXY_PORT}"

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.dirname(os.path.dirname(SCRIPT_DIR))

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------


def _setup_logging() -> logging.Logger:
    """Write DEBUG+ to LOG_FILE and INFO+ to stderr."""
    fmt = logging.Formatter(
        "%(asctime)s [%(levelname)-8s] %(name)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    )
    root = logging.getLogger()
    root.setLevel(logging.DEBUG)

    fh = logging.FileHandler(LOG_FILE, mode="w", encoding="utf-8")
    fh.setLevel(logging.DEBUG)
    fh.setFormatter(fmt)
    root.addHandler(fh)

    sh = logging.StreamHandler(sys.stderr)
    sh.setLevel(logging.INFO)
    sh.setFormatter(fmt)
    root.addHandler(sh)

    return logging.getLogger("stress_test")


log = _setup_logging()
proxy_log = logging.getLogger("proxy")

# ---------------------------------------------------------------------------
# Proxy — TLS MITM recording proxy
# ---------------------------------------------------------------------------


def _generate_proxy_cert() -> tuple:
    """
    Generate a temporary self-signed cert/key pair via openssl.
    Returns (cert_path, key_path, tmpdir).
    """
    tmpdir = tempfile.mkdtemp(prefix="stress_proxy_")
    cert = os.path.join(tmpdir, "proxy.crt")
    key = os.path.join(tmpdir, "proxy.key")
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "rsa:2048",
            "-keyout", key, "-out", cert,
            "-days", "1", "-nodes",
            "-subj", "/CN=stress-test-proxy",
        ],
        check=True,
        capture_output=True,
    )
    log.debug("Proxy cert generated: %s", cert)
    return cert, key, tmpdir


def _read_http_head(rfile) -> tuple:
    """
    Read the first line and headers from a binary file object.
    Returns (first_line_str, headers_dict) or (None, None) on EOF.
    """
    first = rfile.readline()
    if not first or first in (b"\r\n", b"\n"):
        return None, None
    first_str = first.rstrip(b"\r\n").decode("latin-1")
    headers = {}
    while True:
        raw = rfile.readline()
        if raw in (b"\r\n", b"\n", b""):
            break
        line = raw.rstrip(b"\r\n").decode("latin-1")
        if ":" in line:
            k, _, v = line.partition(":")
            headers[k.strip()] = v.strip()
    return first_str, headers


def _read_body(rfile, headers: dict, status_code: int = 0) -> bytes:
    """Read an HTTP message body from a binary file object."""
    # No body for certain status codes.
    if status_code in (204, 304) or 100 <= status_code < 200:
        return b""

    te = headers.get("Transfer-Encoding", "").lower()
    if te == "chunked":
        buf = io.BytesIO()
        while True:
            size_hex = rfile.readline().rstrip(b"\r\n")
            try:
                chunk_size = int(size_hex, 16)
            except ValueError:
                break
            if chunk_size == 0:
                rfile.readline()  # trailing CRLF
                break
            buf.write(rfile.read(chunk_size))
            rfile.readline()  # CRLF after chunk data
        return buf.getvalue()

    try:
        length = int(headers.get("Content-Length", "0"))
    except ValueError:
        length = 0
    return rfile.read(length) if length > 0 else b""


def _encode_headers(headers: dict) -> bytes:
    return b"".join(
        f"{k}: {v}\r\n".encode("latin-1") for k, v in headers.items()
    )


def _log_body(label: str, body: bytes, headers: dict) -> None:
    """Log a request or response body to proxy_log at DEBUG level."""
    if not body:
        return
    ct = headers.get("Content-Type", "")
    if "json" in ct or "text" in ct:
        text = body.decode("utf-8", errors="replace")
        try:
            text = json.dumps(json.loads(text), separators=(",", ":"))
        except (json.JSONDecodeError, ValueError):
            pass
    else:
        text = repr(body)
    proxy_log.debug("    %s (%d bytes): %s", label, len(body), text)


class _MITMProxyHandler(BaseHTTPRequestHandler):
    """
    HTTP proxy handler.  For HTTPS (CONNECT), performs TLS MITM so that
    inner HTTP headers are visible.  For plain HTTP, forwards directly.
    All headers are written to proxy_log (which writes to LOG_FILE).
    """

    # Set by start_proxy() before the server starts.
    cert_file: str = ""
    key_file: str = ""

    # ------------------------------------------------------------------
    # CONNECT — HTTPS tunnel with optional TLS MITM
    # ------------------------------------------------------------------

    def do_CONNECT(self) -> None:
        host, _, port_str = self.path.rpartition(":")
        port = int(port_str) if port_str else 443

        proxy_log.info(">>> CONNECT %s", self.path)
        for k, v in self.headers.items():
            proxy_log.debug("    %s: %s", k, v)

        try:
            upstream = socket.create_connection((host, port), timeout=30)
            upstream.settimeout(None)  # no timeout once connected; FMC bulk ops can take >30 s
        except OSError as exc:
            self.send_error(502, f"Cannot connect to {host}:{port}: {exc}")
            return

        self.send_response(200, "Connection established")
        self.end_headers()
        proxy_log.info("<<< 200 Connection established (%s)", self.path)

        if not (self.cert_file and self.key_file):
            self._blind_tunnel(self.connection, upstream)
            return

        try:
            srv_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            srv_ctx.load_cert_chain(self.cert_file, self.key_file)
            client_tls = srv_ctx.wrap_socket(
                self.connection, server_side=True
            )

            cli_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            cli_ctx.check_hostname = False
            cli_ctx.verify_mode = ssl.CERT_NONE
            upstream_tls = cli_ctx.wrap_socket(
                upstream, server_hostname=host
            )
        except ssl.SSLError as exc:
            proxy_log.warning(
                "TLS MITM setup failed for %s (%s) — blind tunnel",
                host, exc,
            )
            try:
                upstream.close()
            except OSError:
                pass
            return

        self._relay_http(client_tls, upstream_tls, host)

    # ------------------------------------------------------------------
    # Plain HTTP methods
    # ------------------------------------------------------------------

    def do_GET(self) -> None:
        self._proxy_plain("GET")

    def do_POST(self) -> None:
        self._proxy_plain("POST")

    def do_PUT(self) -> None:
        self._proxy_plain("PUT")

    def do_DELETE(self) -> None:
        self._proxy_plain("DELETE")

    def do_PATCH(self) -> None:
        self._proxy_plain("PATCH")

    def do_HEAD(self) -> None:
        self._proxy_plain("HEAD")

    def _proxy_plain(self, method: str) -> None:
        import http.client
        from urllib.parse import urlparse

        parsed = urlparse(self.path)
        host = parsed.hostname or ""
        port = parsed.port or 80
        path = (parsed.path or "/") + (
            f"?{parsed.query}" if parsed.query else ""
        )

        proxy_log.info(">>> %s %s", method, self.path)
        for k, v in self.headers.items():
            proxy_log.debug("    %s: %s", k, v)

        cl = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(cl) if cl else b""
        _log_body("request body", body, dict(self.headers))

        try:
            conn = http.client.HTTPConnection(host, port, timeout=30)
            conn.request(method, path, body, dict(self.headers))
            resp = conn.getresponse()
            resp_body = resp.read()
            resp_headers_dict = dict(resp.getheaders())

            proxy_log.info(
                "<<< %d %s (%s)", resp.status, resp.reason, self.path
            )
            for k, v in resp.getheaders():
                proxy_log.debug("    %s: %s", k, v)
            if not (200 <= resp.status < 300):
                _log_body("response body", resp_body, resp_headers_dict)

            self.send_response(resp.status)
            skip = {"transfer-encoding", "connection"}
            for k, v in resp.getheaders():
                if k.lower() not in skip:
                    self.send_header(k, v)
            self.end_headers()
            self.wfile.write(resp_body)
        except OSError as exc:
            self.send_error(502, str(exc))

    # ------------------------------------------------------------------
    # HTTP relay inside TLS MITM tunnel
    # ------------------------------------------------------------------

    def _relay_http(
        self,
        client_ssl: ssl.SSLSocket,
        upstream_ssl: ssl.SSLSocket,
        host: str,
    ) -> None:
        """Read/write HTTP between two TLS sockets, logging all headers."""
        crfile = client_ssl.makefile("rb")
        urfile = upstream_ssl.makefile("rb")
        try:
            while True:
                # ---------- request ----------
                req_line, req_headers = _read_http_head(crfile)
                if req_line is None:
                    break

                parts = req_line.split(" ", 2)
                method = parts[0] if parts else "?"
                req_path = parts[1] if len(parts) > 1 else "/"

                proxy_log.info(
                    ">>> %s https://%s%s", method, host, req_path
                )
                for k, v in req_headers.items():
                    proxy_log.debug("    %s: %s", k, v)

                req_body = _read_body(crfile, req_headers)
                _log_body("request body", req_body, req_headers)
                # We decoded any chunked body; switch to Content-Length.
                if req_headers.get(
                    "Transfer-Encoding", ""
                ).lower() == "chunked":
                    req_headers.pop("Transfer-Encoding", None)
                    req_headers["Content-Length"] = str(len(req_body))
                upstream_ssl.sendall(
                    f"{req_line}\r\n".encode("latin-1")
                    + _encode_headers(req_headers)
                    + b"\r\n"
                    + req_body
                )

                # ---------- response ----------
                resp_line, resp_headers = _read_http_head(urfile)
                if resp_line is None:
                    break

                proxy_log.info(
                    "<<< %s (https://%s%s)", resp_line, host, req_path
                )
                for k, v in resp_headers.items():
                    proxy_log.debug("    %s: %s", k, v)

                try:
                    status_code = int(resp_line.split(" ")[1])
                except (IndexError, ValueError):
                    status_code = 0

                resp_body = _read_body(urfile, resp_headers, status_code)
                if not (200 <= status_code < 300):
                    _log_body("response body", resp_body, resp_headers)
                # We decoded any chunked body; switch to Content-Length so
                # the downstream client does not try to re-decode chunks.
                if resp_headers.get(
                    "Transfer-Encoding", ""
                ).lower() == "chunked":
                    resp_headers.pop("Transfer-Encoding", None)
                    resp_headers["Content-Length"] = str(len(resp_body))
                client_ssl.sendall(
                    f"{resp_line}\r\n".encode("latin-1")
                    + _encode_headers(resp_headers)
                    + b"\r\n"
                    + resp_body
                )

                if resp_headers.get("Connection", "").lower() == "close":
                    break

        except (OSError, ssl.SSLError, ValueError, UnicodeDecodeError):
            pass
        finally:
            for rfile in (crfile, urfile):
                try:
                    rfile.close()
                except OSError:
                    pass
            for sock in (client_ssl, upstream_ssl):
                try:
                    sock.close()
                except OSError:
                    pass

    # ------------------------------------------------------------------
    # Blind bidirectional tunnel (fallback when no cert available)
    # ------------------------------------------------------------------

    def _blind_tunnel(
        self, client: socket.socket, upstream: socket.socket
    ) -> None:
        import select

        sockets = [client, upstream]
        try:
            while True:
                readable, _, errored = select.select(
                    sockets, [], sockets, 10
                )
                if errored:
                    break
                for src in readable:
                    dst = upstream if src is client else client
                    data = src.recv(65536)
                    if not data:
                        return
                    dst.sendall(data)
        except OSError:
            pass
        finally:
            for sock in sockets:
                try:
                    sock.close()
                except OSError:
                    pass

    def log_message(self, fmt, *args) -> None:  # noqa: ARG002
        """Suppress BaseHTTPRequestHandler's default stderr logging."""


class _ThreadingHTTPServer(socketserver.ThreadingMixIn, HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def start_proxy(cert_file: str = "", key_file: str = "") -> threading.Thread:
    """
    Start the MITM proxy on PROXY_PORT in a daemon thread.
    The thread dies automatically when the main process exits.
    """
    _MITMProxyHandler.cert_file = cert_file
    _MITMProxyHandler.key_file = key_file
    server = _ThreadingHTTPServer(
        ("127.0.0.1", PROXY_PORT), _MITMProxyHandler
    )
    thread = threading.Thread(
        target=server.serve_forever,
        name="proxy",
        daemon=True,
    )
    thread.start()
    log.info(
        "Proxy listening on 127.0.0.1:%d — headers logged to %s",
        PROXY_PORT,
        LOG_FILE,
    )
    return thread


# ---------------------------------------------------------------------------
# FMC REST API client
# ---------------------------------------------------------------------------


class FMCClient:
    """
    Minimal FMC REST API client.
    All requests go through the recording proxy so headers appear in LOG_FILE.
    """

    def __init__(self, url: str, username: str, password: str) -> None:
        self.base = url.rstrip("/")
        self.username = username
        self.password = password
        self.token: str = ""
        self.domain_uuid: str = ""
        self._session = requests.Session()
        self._session.verify = False
        self._session.proxies = {
            "http": PROXY_ADDR,
            "https": PROXY_ADDR,
        }

    def authenticate(self) -> None:
        """Obtain a short-lived auth token and Global domain UUID."""
        # Close any stale pooled connections (e.g. MITM tunnels from a prior
        # terraform subprocess) before opening a fresh connection.
        self._session.close()
        self._session = requests.Session()
        self._session.verify = False
        self._session.proxies = {
            "http": PROXY_ADDR,
            "https": PROXY_ADDR,
        }
        log.info("FMC: authenticating as %s", self.username)
        resp = self._session.post(
            f"{self.base}/api/fmc_platform/v1/auth/generatetoken",
            auth=(self.username, self.password),
        )
        resp.raise_for_status()
        self.token = resp.headers["X-auth-access-token"]
        self._session.headers.update(
            {"X-auth-access-token": self.token}
        )

        info = self._session.get(
            f"{self.base}/api/fmc_platform/v1/info/domain"
        )
        info.raise_for_status()
        for domain in info.json().get("items", []):
            if domain["name"] == "Global":
                self.domain_uuid = domain["uuid"]
                break

        if not self.domain_uuid:
            raise RuntimeError("Global domain UUID not found")
        log.info("FMC: authenticated (domain=%s)", self.domain_uuid)

    def _url(self, path: str) -> str:
        return (
            f"{self.base}/api/fmc_config/v1"
            f"/domain/{self.domain_uuid}{path}"
        )

    # ---------- network groups ----------

    def list_network_groups(self, name_filter: str = "") -> list:
        """Return all network groups matching name_filter (paginated)."""
        items = []
        offset = 0
        while True:
            params: dict = {
                "limit": 1000,
                "offset": offset,
                "expanded": "false",
            }
            if name_filter:
                params["filter"] = f"nameOrValue:{name_filter}"
            resp = self._session.get(
                self._url("/object/networkgroups"), params=params
            )
            resp.raise_for_status()
            page = resp.json().get("items", [])
            items.extend(page)
            if len(page) < 1000:
                break
            offset += 1000
        return items

    def delete_network_groups_bulk(self, ids: list) -> None:
        """Delete network groups in batches of 50."""
        for i in range(0, len(ids), 50):
            batch = ",".join(ids[i: i + 50])
            # Recreate the session before each batch: the proxy closes the MITM
            # tunnel after each long-running request, so pooled connections are
            # stale by the time the next batch starts.
            self._session.close()
            self._session = requests.Session()
            self._session.verify = False
            self._session.proxies = {"http": PROXY_ADDR, "https": PROXY_ADDR}
            self._session.headers.update({"X-auth-access-token": self.token})
            resp = self._session.delete(
                self._url("/object/networkgroups"),
                params={"bulk": "true", "filter": f"ids:{batch}"},
            )
            if resp.status_code not in (200, 204):
                log.warning(
                    "Bulk group delete HTTP %d: %s",
                    resp.status_code,
                    resp.text[:200],
                )

    # ---------- access control policies ----------

    def list_acps(self) -> list:
        resp = self._session.get(
            self._url("/policy/accesspolicies"), params={"limit": 25}
        )
        resp.raise_for_status()
        return resp.json().get("items", [])

    def delete_acp(self, acp_id: str) -> None:
        """Delete an ACP (cascade-deletes all its rules) with retry."""
        deadline = time.time() + 120
        while time.time() < deadline:
            resp = self._session.delete(
                self._url(f"/policy/accesspolicies/{acp_id}")
            )
            if resp.status_code in (200, 204, 404):
                return
            log.debug(
                "ACP delete HTTP %d — retrying in 5 s", resp.status_code
            )
            time.sleep(5)
        log.warning("Timed out deleting ACP %s", acp_id)

    # ---------- cleanup ----------

    def hard_cleanup(self) -> None:
        """
        Delete all test-related ACPs, rules, and network groups from FMC.
        Equivalent to fmc_hard_cleanup() in the bash script.
        """
        log.info("FMC hard cleanup: removing leftover test objects...")
        self.authenticate()

        # Pass 1: delete test ACPs (FMC cascade-deletes their rules).
        test_acps = [a for a in self.list_acps() if ACP_NAME in a["name"]]
        for acp in test_acps:
            log.info("  deleting ACP: %s", acp["name"])
            self.delete_acp(acp["id"])
        if test_acps:
            time.sleep(3)

        # Pass 2a: delete network groups matching test prefix.
        groups = self.list_network_groups(GROUP_PREFIX)
        ids = [g["id"] for g in groups]
        if ids:
            log.info("  deleting %d test network groups", len(ids))
            self.delete_network_groups_bulk(ids)
            time.sleep(2)

        # Pass 2b: delete any remaining __gc_ groups.
        gc_groups = [
            g for g in self.list_network_groups("__gc_")
            if g["name"].startswith("__gc_")
        ]
        gc_ids = [g["id"] for g in gc_groups]
        if gc_ids:
            log.info("  deleting %d __gc_ groups", len(gc_ids))
            self.delete_network_groups_bulk(gc_ids)
            time.sleep(2)

        # Verify FMC is clean.
        remaining_groups = self.list_network_groups(GROUP_PREFIX)
        remaining_acps = [
            a for a in self.list_acps() if ACP_NAME in a["name"]
        ]
        if remaining_groups or remaining_acps:
            raise RuntimeError(
                f"FMC not clean after hard cleanup: "
                f"{len(remaining_groups)} group(s), "
                f"{len(remaining_acps)} ACP(s) still present"
            )
        log.info("FMC hard cleanup complete — FMC is clean")

    def list_gc_groups(self) -> list:
        """Return all __gc_* network groups currently in FMC."""
        return [
            g for g in self.list_network_groups("__gc_")
            if g["name"].startswith("__gc_")
        ]


# ---------------------------------------------------------------------------
# Terraform workspace helpers
# ---------------------------------------------------------------------------


def _write_tf_workspace(tf_dir: str, provider_dir: str) -> str:
    """
    Write providers.tf, variables.tf, and main.tf into tf_dir.
    Returns the path to the .tfrc dev-overrides file.
    """
    tfrc = os.path.join(tf_dir, "dev.tfrc")
    with open(tfrc, "w") as fh:
        fh.write(
            f'provider_installation {{\n'
            f'  dev_overrides {{ "CiscoDevNet/fmc" = "{provider_dir}" }}\n'
            f'  direct {{}}\n'
            f'}}\n'
        )

    with open(os.path.join(tf_dir, "providers.tf"), "w") as fh:
        fh.write(
            textwrap.dedent("""\
                terraform {
                  required_providers {
                    fmc = { source = "CiscoDevNet/fmc" }
                  }
                }
                provider "fmc" {
                  insecure = true
                }
            """)
        )

    with open(os.path.join(tf_dir, "variables.tf"), "w") as fh:
        fh.write(
            textwrap.dedent("""\
                variable "network_groups" {
                  type    = map(object({ literal = string }))
                  default = {}
                }
                variable "access_rule_groups" {
                  type    = map(string)
                  default = {}
                }
            """)
        )

    with open(os.path.join(tf_dir, "main.tf"), "w") as fh:
        fh.write(
            textwrap.dedent(f"""\
                resource "fmc_access_control_policy" "test" {{
                  name              = "{ACP_NAME}"
                  default_action    = "BLOCK"
                  manage_rules      = false
                  manage_categories = false
                }}

                resource "fmc_network_groups_safe" "test" {{
                  items = {{
                    for name, cfg in var.network_groups : name => {{
                      literals = [{{ value = cfg.literal }}]
                    }}
                  }}
                }}

                resource "fmc_access_rules" "test" {{
                  access_control_policy_id = fmc_access_control_policy.test.id
                  items = [
                    for rule_name, group_name in var.access_rule_groups : {{
                      name   = rule_name
                      action = "ALLOW"
                      destination_network_objects = [{{
                        id   = fmc_network_groups_safe.test.items[group_name].id
                        type = "NetworkGroup"
                      }}]
                    }}
                  ]
                }}
            """)
        )

    return tfrc


def _make_tfvars(start: int, end: int) -> dict:
    """
    Build a tfvars dict with groups/rules from start to end inclusive.
    Group i gets the CIDR 10.<(i-1)//256>.<(i-1)%256>.0/24.
    """
    groups = {}
    for i in range(start, end + 1):
        oct2 = (i - 1) // 256
        oct3 = (i - 1) % 256
        groups[f"{GROUP_PREFIX}-{i}"] = {
            "literal": f"10.{oct2}.{oct3}.0/24"
        }
    rules = {
        f"{RULE_PREFIX}-{i}": f"{GROUP_PREFIX}-{i}"
        for i in range(start, end + 1)
    }
    return {"network_groups": groups, "access_rule_groups": rules}


def _run_terraform(
    terraform_bin: str,
    tf_dir: str,
    tfrc: str,
    command: list,
    fmc_env: dict,
) -> subprocess.CompletedProcess:
    """
    Run a Terraform command in tf_dir.
    Sets HTTPS_PROXY/HTTP_PROXY so Terraform's HTTP traffic goes through
    the recording proxy.
    """
    env = os.environ.copy()
    env["TF_CLI_CONFIG_FILE"] = tfrc
    # Route Terraform's HTTP calls through the recording proxy.
    env["HTTPS_PROXY"] = PROXY_ADDR
    env["HTTP_PROXY"] = PROXY_ADDR
    env["https_proxy"] = PROXY_ADDR
    env["http_proxy"] = PROXY_ADDR
    env["NO_PROXY"] = ""
    env.update(fmc_env)

    log.info("terraform %s", " ".join(command))
    result = subprocess.run(
        [terraform_bin] + command,
        cwd=tf_dir,
        env=env,
        capture_output=True,
        text=True,
    )
    # Always write full output to the log file.
    if result.stdout:
        log.debug("terraform stdout:\n%s", result.stdout[-6000:])
    if result.returncode != 0 and result.stderr:
        log.debug("terraform stderr:\n%s", result.stderr[-3000:])
    return result


# ---------------------------------------------------------------------------
# Test 3 — fmc_network_groups_safe: remove one group (expect SUCCESS + GC)
# ---------------------------------------------------------------------------


def run_test_3(
    fmc: FMCClient,
    terraform_bin: str,
    provider_dir: str,
    count: int,
) -> bool:
    """
    Equivalent to test 3 in run_test.sh:
      - full apply  (count groups + count rules via fmc_network_groups_safe)
      - partial apply (remove group-1 and rule-1)
      - verify soft-delete (__gc_ group appears in FMC)
      - GC apply (second apply)
      - verify GC (__gc_ group removed)

    Returns True if the test passes, False otherwise.
    """
    sep = "=" * 60
    log.info(sep)
    log.info(
        "TEST 3 — fmc_network_groups_safe: remove one group "
        "(expect SUCCESS + GC)"
    )
    log.info(sep)

    tf_dir = tempfile.mkdtemp(prefix="stress_test_tf_")
    log.debug("Terraform workspace: %s", tf_dir)
    tfrc = _write_tf_workspace(tf_dir, provider_dir)
    tfvars_path = os.path.join(tf_dir, "test.auto.tfvars.json")

    fmc_env = {
        "FMC_USERNAME": fmc.username,
        "FMC_PASSWORD": fmc.password,
        "FMC_URL": fmc.base,
        "FMC_INSECURE": "true",
    }

    def tf(cmd: list) -> subprocess.CompletedProcess:
        return _run_terraform(terraform_bin, tf_dir, tfrc, cmd, fmc_env)

    passed = False
    try:
        # ── Step 1: full apply ────────────────────────────────────────
        log.info(
            "Step 1: applying full config (%d groups, %d rules)...",
            count, count,
        )
        with open(tfvars_path, "w") as fh:
            json.dump(_make_tfvars(1, count), fh)

        result = tf(["apply", "-auto-approve"])
        if result.returncode != 0:
            log.error(
                "FAIL — full apply failed:\n%s", result.stdout[-3000:]
            )
            return False
        log.info("Step 1: full apply succeeded")

        # ── Step 2: partial apply (remove group-1 + rule-1) ──────────
        log.info(
            "Step 2: applying partial config (removing group-1 and rule-1)..."
        )
        with open(tfvars_path, "w") as fh:
            json.dump(_make_tfvars(2, count), fh)

        result = tf(["apply", "-auto-approve"])
        if result.returncode != 0:
            log.error(
                "FAIL — partial apply failed "
                "(fmc_network_groups_safe should have soft-deleted group-1)"
                ":\n%s",
                result.stdout[-3000:],
            )
            return False
        log.info("Step 2: partial apply succeeded")

        # ── Step 3: verify __gc_ group exists ────────────────────────
        log.info("Step 3: verifying soft-deleted group in FMC...")
        fmc.authenticate()
        gc_groups = fmc.list_gc_groups()
        if not gc_groups:
            log.error(
                "FAIL — no __gc_ group found in FMC after soft-delete"
            )
            return False
        log.info(
            "Step 3: soft-deleted group(s) found: %s",
            [g["name"] for g in gc_groups],
        )

        # ── Step 4: second apply (GC pass) ────────────────────────────
        log.info("Step 4: running GC apply (second terraform apply)...")
        result = tf(["apply", "-auto-approve"])
        if result.returncode != 0:
            log.error(
                "FAIL — GC apply failed:\n%s", result.stdout[-3000:]
            )
            return False
        log.info("Step 4: GC apply succeeded")

        # ── Step 5: verify __gc_ group is gone ───────────────────────
        log.info("Step 5: verifying __gc_ group was removed by GC...")
        fmc.authenticate()
        gc_after = fmc.list_gc_groups()
        if gc_after:
            log.error(
                "FAIL — __gc_ group(s) still present after GC: %s",
                [g["name"] for g in gc_after],
            )
            return False

        log.info(
            "TEST 3 PASSED — soft-delete and GC work correctly"
        )
        passed = True
        return True

    finally:
        log.info("Cleaning up terraform state for test 3...")
        tf(["destroy", "-auto-approve"])
        shutil.rmtree(tf_dir, ignore_errors=True)
        log.info(
            "Test 3 result: %s", "PASSED" if passed else "FAILED"
        )


# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------


def _build_provider(output_path: str) -> None:
    """Build the Terraform provider binary from REPO_DIR."""
    log.info("Building provider binary: %s", output_path)
    result = subprocess.run(
        ["go", "build", "-o", output_path, "."],
        cwd=REPO_DIR,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"go build failed:\n{result.stderr}"
        )
    log.info("Provider built successfully")


# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Stress test for fmc_network_groups_safe (test 3). "
            "Routes all traffic through a local MITM proxy on "
            f"port {PROXY_PORT} and logs headers to {LOG_FILE}."
        )
    )
    parser.add_argument(
        "-u", "--username", required=True, help="FMC username"
    )
    parser.add_argument(
        "-p", "--password", required=True, help="FMC password"
    )
    parser.add_argument(
        "--url", required=True, help="FMC base URL (https://…)"
    )
    parser.add_argument(
        "--terraform",
        default=shutil.which("terraform") or "",
        help="Path to terraform binary (default: from PATH)",
    )
    parser.add_argument(
        "--count",
        type=int,
        default=1000,
        help="Number of groups/rules to create (default: 1000)",
    )
    return parser.parse_args()


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> int:
    args = _parse_args()

    if not args.terraform:
        log.error("terraform not found — pass --terraform /path/to/terraform")
        return 1

    log.info(
        "Stress test starting  count=%d  url=%s  logfile=%s",
        args.count, args.url, LOG_FILE,
    )

    # ── Start MITM proxy ─────────────────────────────────────────────────
    cert_file = key_file = cert_tmpdir = ""
    try:
        cert_file, key_file, cert_tmpdir = _generate_proxy_cert()
    except (subprocess.CalledProcessError, FileNotFoundError) as exc:
        log.warning(
            "openssl unavailable (%s) — proxy will tunnel without MITM", exc
        )
    start_proxy(cert_file, key_file)
    time.sleep(0.2)  # allow the server socket to bind

    # ── FMC client ───────────────────────────────────────────────────────
    fmc = FMCClient(args.url, args.username, args.password)

    # ── Build provider ───────────────────────────────────────────────────
    provider_bin = os.path.join(SCRIPT_DIR, "terraform-provider-fmc")
    try:
        _build_provider(provider_bin)
    except RuntimeError as exc:
        log.error("Build failed: %s", exc)
        return 1

    # ── Pre-run cleanup ──────────────────────────────────────────────────
    log.info("Pre-run: cleaning up any leftover test objects in FMC...")
    try:
        fmc.hard_cleanup()
    except Exception as exc:
        log.error("Pre-run cleanup failed: %s", exc)
        return 1

    # ── Run test 3 ───────────────────────────────────────────────────────
    passed = False
    try:
        passed = run_test_3(fmc, args.terraform, SCRIPT_DIR, args.count)
    except Exception as exc:
        log.exception("Test 3 raised an unexpected exception: %s", exc)

    # ── Post-run cleanup ─────────────────────────────────────────────────
    log.info("Post-run: cleaning up FMC...")
    try:
        fmc.hard_cleanup()
    except Exception as exc:
        log.warning("Post-run cleanup failed: %s", exc)

    # ── Tidy up temp files ───────────────────────────────────────────────
    if cert_tmpdir:
        shutil.rmtree(cert_tmpdir, ignore_errors=True)
    try:
        os.unlink(provider_bin)
    except OSError:
        pass

    # ── Final result ─────────────────────────────────────────────────────
    if passed:
        log.info("RESULT: PASSED")
        return 0
    log.error("RESULT: FAILED")
    return 1


if __name__ == "__main__":
    sys.exit(main())
