#!/usr/bin/env python3
"""Probe 2: log in with the real kubelogin binary, then test the token.

kubelogin is what kubectl runs for an OIDC cluster, so this is the login the cluster
path already uses. After it returns a token this script:
  1. decodes the claims (groups? lifetime?) and checks for a refresh token
  2. tries the token against the Palette API in several header forms

Reads ~/.config/kubectl-palette/probe.json. Writes a redacted report to
~/.config/kubectl-palette/probe-report.txt. Tokens stay in a mode-700 cache dir.

Usage: kubelogin-probe.py [--extra-scope groups] [--extra-scope offline_access] [--pkce auto|no|S256]
"""
import argparse
import base64
import glob
import json
import os
import shutil
import subprocess
import sys
import time
import traceback
import urllib.error
import urllib.request

CFG_DIR = os.path.expanduser("~/.config/kubectl-palette")
REPORT = os.path.join(CFG_DIR, "probe-report.txt")
CACHE = os.path.join(CFG_DIR, "probe-token-cache")
KUBELOGIN = os.path.join(CFG_DIR, "bin", "kubelogin")
ap = argparse.ArgumentParser()
ap.add_argument("--extra-scope", action="append", default=[])
ap.add_argument("--pkce", default="auto")
args = ap.parse_args()

cfg = json.load(open(os.path.join(CFG_DIR, "probe.json")))
issuer, client_id, client_secret, api = (cfg[k] for k in ("issuer", "client_id", "client_secret", "api"))
report = []


def redact(text):
    return str(text).replace(client_id, "<tenant>").replace(client_secret, "<secret>")


def say(line=""):
    line = redact(line)
    print(line, flush=True)
    report.append(line)
    with open(REPORT, "w") as f:
        f.write("\n".join(report) + "\n")


def claims_of(jwt):
    try:
        part = jwt.split(".")[1]
        return json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)))
    except Exception:
        return None


def main():
    scopes = ["profile", "email"] + args.extra_scope
    say(f"== kubelogin probe started {time.strftime('%Y-%m-%d %H:%M:%S')} UTC, extra scopes: {' '.join(scopes)}, pkce: {args.pkce}")
    shutil.rmtree(CACHE, ignore_errors=True)
    os.makedirs(CACHE, mode=0o700)
    cmd = [KUBELOGIN, "get-token", f"--oidc-issuer-url={issuer}", f"--oidc-client-id={client_id}",
           f"--oidc-client-secret={client_secret}", f"--token-cache-dir={CACHE}",
           "--skip-open-browser", "--listen-address=127.0.0.1:8000", f"--oidc-pkce-method={args.pkce}",
           "--authentication-timeout-sec=300"] + [f"--oidc-extra-scope={s}" for s in scopes]
    print("\nWhen kubelogin prints a URL below, open http://localhost:8000 in your laptop browser")
    print("(needs ssh -L 8000:localhost:8000) and log in.\n", flush=True)
    t0 = time.time()
    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    for line in p.stderr.splitlines():
        say("  kubelogin: " + line)
    say(f"kubelogin exit code: {p.returncode} after {round(time.time() - t0)} s")
    if p.returncode != 0:
        say("FAILED: kubelogin could not log in. That means the standard kubectl OIDC login fails here too.")
        return 1
    cred = json.loads(p.stdout)
    token = cred["status"]["token"]
    say(f"kubelogin returned an ExecCredential ({cred.get('apiVersion')}), expires {cred['status'].get('expirationTimestamp')}")

    cached = {}
    for f in glob.glob(os.path.join(CACHE, "*")):
        try:
            cached = json.load(open(f))
        except Exception:
            pass
    say(f"token cache fields: {sorted(cached.keys())}; refresh_token cached: {'yes' if cached.get('refresh_token') else 'no'}")

    c = claims_of(token) or {}
    say(f"id_token claims: {sorted(c.keys())}")
    if "exp" in c and "iat" in c:
        say(f"  lifetime: {round((c['exp'] - c['iat']) / 60)} min")
    for k in ("iss", "aud", "email", "groups", "teams", "roles", "name", "sub"):
        if k in c:
            say(f"  {k}: {c[k]}")
    other = [k for k in c if k not in ("iss", "aud", "email", "groups", "teams", "roles", "name", "sub", "exp", "iat", "nbf", "nonce", "at_hash", "jti")]
    for k in other:
        v = c[k]
        say(f"  {k}: {v if isinstance(v, (bool, int, float, list)) or len(str(v)) < 60 else str(v)[:40] + '...'}")

    tests = [("GET", "/v1/users/me", None), ("POST", "/v1/dashboard/projects", {}),
             ("GET", "/v1/dashboard/spectroclusters/metadata", None)]
    say("")
    say("== Palette API with the OIDC id_token")
    any_ok = False
    for hname, hval in (("Authorization: <token>", token), ("Authorization: Bearer <token>", "Bearer " + token)):
        for method, path, body in tests:
            req = urllib.request.Request(api + path, method=method,
                                         data=json.dumps(body).encode() if body is not None else None,
                                         headers={"Authorization": hval, "Content-Type": "application/json"})
            try:
                r = urllib.request.urlopen(req, timeout=30)
                data = json.load(r)
                n = len(data.get("items", [])) if isinstance(data, dict) and "items" in data else "-"
                say(f"  {hname:29} | {method:4} {path}: {r.status} items={n}")
                any_ok = True
            except urllib.error.HTTPError as e:
                say(f"  {hname:29} | {method:4} {path}: {e.code} {e.read()[:140].decode(errors='replace')}")
            except Exception as e:
                say(f"  {hname:29} | {method:4} {path}: ERR {e}")
    say("")
    say(f"VERDICT: Palette API {'ACCEPTS' if any_ok else 'REJECTS'} the OIDC login token.")
    return 0


try:
    code = main()
except KeyboardInterrupt:
    say("INTERRUPTED (Ctrl-C) before finishing.")
    code = 130
except Exception:
    say("CRASHED:\n" + traceback.format_exc())
    code = 1
print(f"\nReport saved: {REPORT}")
sys.exit(code)
