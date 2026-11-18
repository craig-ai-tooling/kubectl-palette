#!/usr/bin/env python3
"""Probe: does a Palette-IdP OIDC login token work against the Palette API?

Runs the same browser login kubectl does for an OIDC cluster, then:
  1. decodes the token claims (groups? lifetime? refresh token?)
  2. tries the token against the Palette API in several header forms

Reads issuer / client id / client secret from ~/.config/kubectl-palette/probe.json.
Writes a redacted report to ~/.config/kubectl-palette/probe-report.txt and the raw
tokens (mode 600) to ~/.config/kubectl-palette/probe-token.json.

Usage: oidc-probe.py [--extra-scope groups] [--extra-scope offline_access] [--port 8000]
"""
import argparse
import base64
import http.server
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

CFG_DIR = os.path.expanduser("~/.config/kubectl-palette")
ap = argparse.ArgumentParser()
ap.add_argument("--extra-scope", action="append", default=[])
ap.add_argument("--port", type=int, default=8000)
args = ap.parse_args()

cfg = json.load(open(os.path.join(CFG_DIR, "probe.json")))
issuer, client_id, client_secret, api = (cfg[k] for k in ("issuer", "client_id", "client_secret", "api"))
disc = json.load(urllib.request.urlopen(issuer + "/.well-known/openid-configuration", timeout=20))
redirect = f"http://localhost:{args.port}"
state, nonce = secrets.token_urlsafe(16), secrets.token_urlsafe(16)
scopes = ["openid", "profile", "email"] + args.extra_scope
auth_url = disc["authorization_endpoint"] + "?" + urllib.parse.urlencode({
    "response_type": "code", "client_id": client_id, "redirect_uri": redirect,
    "scope": " ".join(scopes), "state": state, "nonce": nonce})

report = []


def say(line=""):
    print(line, flush=True)
    report.append(line)


def redact(text):
    return str(text).replace(client_id, "<tenant>")


result = {}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        if "code" not in q and "error" not in q:
            self.send_response(302)
            self.send_header("Location", auth_url)
            self.end_headers()
            return
        result.update({k: v[0] for k, v in q.items()})
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(b"<h2>Login captured. Go back to the terminal.</h2>")


srv = http.server.HTTPServer(("127.0.0.1", args.port), Handler)
print(f"\nListening on 127.0.0.1:{args.port}. Scopes: {' '.join(scopes)}")
print(f"Open this in your laptop browser (needs ssh -L {args.port}:localhost:{args.port}):\n")
print(f"    http://localhost:{args.port}\n")
print("Waiting for the login to come back (5 min timeout)...", flush=True)
srv.timeout = 300
deadline = time.time() + 300
while "code" not in result and "error" not in result and time.time() < deadline:
    srv.handle_request()
srv.server_close()

say(f"== Palette OIDC probe, scopes: {' '.join(scopes)}")
if "code" not in result:
    say(f"FAILED at browser step: {redact(result) if result else 'timed out, no callback received'}")
    open(os.path.join(CFG_DIR, "probe-report.txt"), "w").write("\n".join(report) + "\n")
    sys.exit(1)
if result.get("state") != state:
    say("FAILED: state mismatch")
    sys.exit(1)


def token_exchange(basic):
    body = {"grant_type": "authorization_code", "code": result["code"], "redirect_uri": redirect}
    headers = {"Content-Type": "application/x-www-form-urlencoded"}
    if basic:
        headers["Authorization"] = "Basic " + base64.b64encode(f"{client_id}:{client_secret}".encode()).decode()
    else:
        body.update({"client_id": client_id, "client_secret": client_secret})
    req = urllib.request.Request(disc["token_endpoint"], data=urllib.parse.urlencode(body).encode(), headers=headers)
    return json.load(urllib.request.urlopen(req, timeout=30))


tok = None
for basic in (True, False):
    try:
        tok = token_exchange(basic)
        say(f"token exchange: OK (client auth via {'basic header' if basic else 'post body'})")
        break
    except urllib.error.HTTPError as e:
        say(f"token exchange ({'basic' if basic else 'post'}): HTTP {e.code} {redact(e.read()[:200])}")
if not tok:
    open(os.path.join(CFG_DIR, "probe-report.txt"), "w").write("\n".join(report) + "\n")
    sys.exit(1)

fd = os.open(os.path.join(CFG_DIR, "probe-token.json"), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
os.write(fd, json.dumps(tok).encode())
os.close(fd)

say(f"token response fields: {sorted(tok.keys())}")
say(f"refresh_token returned: {'yes' if tok.get('refresh_token') else 'no'}; expires_in: {tok.get('expires_in')}")


def claims_of(jwt):
    try:
        part = jwt.split(".")[1]
        return json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)))
    except Exception:
        return None


for name in ("id_token", "access_token"):
    c = claims_of(tok.get(name, "") or "")
    if c is None:
        say(f"{name}: {'absent' if not tok.get(name) else 'opaque (not a JWT)'}")
        continue
    say(f"{name} claims: {sorted(c.keys())}")
    if "exp" in c and "iat" in c:
        say(f"  lifetime: {round((c['exp'] - c['iat']) / 60)} min")
    for k in ("iss", "aud", "email", "groups", "teams", "roles", "name", "sub"):
        if k in c:
            say(f"  {k}: {redact(c[k])}")
say(f"same string for id_token and access_token: {tok.get('id_token') == tok.get('access_token')}")

tests = [("GET", "/v1/users/me", None), ("POST", "/v1/dashboard/projects", {}),
         ("GET", "/v1/dashboard/spectroclusters/metadata", None)]
say("\n== Palette API with the OIDC token")
any_ok = False
for tname in ("id_token", "access_token"):
    t = tok.get(tname)
    if not t or (tname == "access_token" and t == tok.get("id_token")):
        continue
    for hname, hval in (("Authorization: <token>", t), ("Authorization: Bearer <token>", "Bearer " + t)):
        for method, path, body in tests:
            req = urllib.request.Request(api + path, method=method,
                                         data=json.dumps(body).encode() if body is not None else None,
                                         headers={"Authorization": hval, "Content-Type": "application/json"})
            try:
                r = urllib.request.urlopen(req, timeout=30)
                data = json.load(r)
                n = len(data.get("items", [])) if isinstance(data, dict) and "items" in data else "-"
                say(f"  {tname:12} | {hname:29} | {method:4} {path}: {r.status} items={n}")
                any_ok = True
            except urllib.error.HTTPError as e:
                say(f"  {tname:12} | {hname:29} | {method:4} {path}: {e.code} {redact(e.read()[:120])}")
            except Exception as e:
                say(f"  {tname:12} | {hname:29} | {method:4} {path}: ERR {e}")

if disc.get("userinfo_endpoint") and tok.get("access_token"):
    try:
        u = json.load(urllib.request.urlopen(urllib.request.Request(
            disc["userinfo_endpoint"], headers={"Authorization": "Bearer " + tok["access_token"]}), timeout=30))
        say(f"\nuserinfo fields: {sorted(u.keys())}" + (f"; groups: {u['groups']}" if "groups" in u else ""))
    except Exception as e:
        say(f"\nuserinfo: {e}")

say(f"\nVERDICT: Palette API {'ACCEPTS' if any_ok else 'REJECTS'} the OIDC login token.")
open(os.path.join(CFG_DIR, "probe-report.txt"), "w").write("\n".join(report) + "\n")
print(f"\nReport saved: {os.path.join(CFG_DIR, 'probe-report.txt')}")
