#!/usr/bin/env python3
"""Follow-up checks on a token obtained by hack/kubelogin-probe.py.

1. Shape of the id_token's nested `token` claim (keys and value shapes only).
2. Does anything inside it work against the Palette API?
3. Does kubelogin refresh silently (no browser) with the cached refresh token?
4. Does the one id_token authenticate to every OIDC cluster? (SelfSubjectReview)

Prints no token values. Needs the Palette API key in PALETTE_API_KEY for step 4.
"""
import base64
import glob
import json
import os
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

import yaml

CFG_DIR = os.path.expanduser("~/.config/kubectl-palette")
CACHE = os.path.join(CFG_DIR, "probe-token-cache")
cfg = json.load(open(os.path.join(CFG_DIR, "probe.json")))
issuer, client_id, client_secret, api = (cfg[k] for k in ("issuer", "client_id", "client_secret", "api"))


def red(s):
    return str(s).replace(client_id, "<tenant>").replace(client_secret, "<secret>")


def claims_of(jwt):
    try:
        part = jwt.split(".")[1]
        return json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)))
    except Exception:
        return None


def cached():
    for f in glob.glob(os.path.join(CACHE, "*")):
        if not f.endswith(".lock"):
            return json.load(open(f))
    return {}


def shape(v):
    if isinstance(v, dict):
        return {k: shape(x) for k, x in v.items()}
    if isinstance(v, list):
        return [shape(x) for x in v[:3]]
    if isinstance(v, str):
        kind = "jwt" if v.count(".") == 2 and len(v) > 100 else "str"
        return f"<{kind} {len(v)} chars>"
    return v


def http(url, method="GET", headers=None, body=None, ctx=None, timeout=15):
    req = urllib.request.Request(url, method=method, headers=headers or {},
                                 data=json.dumps(body).encode() if body is not None else None)
    try:
        r = urllib.request.urlopen(req, timeout=timeout, context=ctx)
        return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return None, str(e).encode()


tok = cached()
idt = tok["id_token"]
c = claims_of(idt)
print("== 1. nested `token` claim shape")
print("  ", json.dumps(shape(c.get("token")), indent=2)[:1500])
rc = claims_of(tok.get("refresh_token", ""))
print("   refresh_token:", "JWT, claims " + str(sorted(rc.keys())) + (f", lifetime {round((rc['exp']-rc['iat'])/3600, 1)} h" if rc and 'exp' in rc and 'iat' in rc else "") if rc else f"opaque, {len(tok.get('refresh_token', ''))} chars")

print("\n== 2. candidates against the Palette API (GET /v1/users/me)")
cands = {}


def walk(prefix, v):
    if isinstance(v, dict):
        for k, x in v.items():
            walk(f"{prefix}.{k}", x)
    elif isinstance(v, str) and len(v) > 60:
        cands[prefix] = v


walk("token", c.get("token"))
cands["refresh_token"] = tok.get("refresh_token", "")
for name, val in cands.items():
    for hname, hv in (("Authorization", val), ("Authorization", "Bearer " + val)):
        s, b = http(api + "/v1/users/me", headers={hname: hv})
        print(f"   {name:28} {'bearer' if hv.startswith('Bearer ') else 'raw   '} -> {s}")
for hname, hv in (("Cookie", "Authorization=" + idt), ("IdToken", idt), ("X-Id-Token", idt)):
    s, b = http(api + "/v1/users/me", headers={hname: hv})
    print(f"   id_token via {hname:14} -> {s}")
s, b = http(issuer + "/userinfo", headers={"Authorization": "Bearer " + idt})
print(f"   userinfo with id_token -> {s} {red(b[:160])}")

print("\n== 3. silent refresh with kubelogin --force-refresh")
t0 = time.time()
p = subprocess.run([os.path.join(CFG_DIR, "bin", "kubelogin"), "get-token", f"--oidc-issuer-url={issuer}",
                    f"--oidc-client-id={client_id}", f"--oidc-client-secret={client_secret}",
                    f"--token-cache-dir={CACHE}", "--skip-open-browser", "--force-refresh",
                    "--oidc-extra-scope=profile", "--oidc-extra-scope=email",
                    "--authentication-timeout-sec=8"], capture_output=True, text=True)
print(f"   exit {p.returncode} in {round(time.time()-t0, 1)} s; stderr: {red(p.stderr.strip()[:200]) or '(none)'}")
if p.returncode == 0:
    new = json.loads(p.stdout)["status"]["token"]
    nc = claims_of(new)
    print(f"   new token issued: {new != idt}; iat moved by {nc['iat'] - c['iat']} s; lifetime {round((nc['exp']-nc['iat'])/60)} min")
    idt, c = new, nc
t0 = time.time()
p = subprocess.run([os.path.join(CFG_DIR, "bin", "kubelogin"), "get-token", f"--oidc-issuer-url={issuer}",
                    f"--oidc-client-id={client_id}", f"--oidc-client-secret={client_secret}",
                    f"--token-cache-dir={CACHE}", "--skip-open-browser",
                    "--oidc-extra-scope=profile", "--oidc-extra-scope=email",
                    "--authentication-timeout-sec=8"], capture_output=True, text=True)
print(f"   cached-token call (what every kubectl command pays): exit {p.returncode} in {round((time.time()-t0)*1000)} ms")

print("\n== 4. one id_token against every OIDC cluster")
key = os.environ.get("PALETTE_API_KEY")
if not key:
    print("   skipped: PALETTE_API_KEY not set")
    raise SystemExit
H = {"ApiKey": key, "Content-Type": "application/json"}
items, cont = [], ""
while True:
    s, b = http(api + "/v1/dashboard/spectroclusters/search?limit=50" + (f"&continue={cont}" if cont else ""),
                method="POST", headers=H, body={"filter": {}, "sort": []}, timeout=30)
    d = json.loads(b)
    items += d["items"]
    cont = (d.get("listmeta") or {}).get("continue") or ""
    if not cont:
        break
for cl in [i for i in items if i["status"].get("state") == "Running"]:
    proj = cl["metadata"]["annotations"]["projectUid"]
    s, b = http(f"{api}/v1/spectroclusters/{cl['metadata']['uid']}/assets/kubeconfigclient",
                headers={**H, "ProjectUid": proj}, timeout=30)
    if s != 200:
        continue
    k = yaml.safe_load(b)["clusters"][0]["cluster"]
    ctx = ssl.create_default_context()
    if k.get("certificate-authority-data"):
        with tempfile.NamedTemporaryFile("wb", delete=False) as f:
            f.write(base64.b64decode(k["certificate-authority-data"]))
        ctx.load_verify_locations(f.name)
        os.unlink(f.name)
    server = k["server"]
    kind = "proxy" if ":443" in server or "proxy" in server else "direct"
    s, b = http(server + "/apis/authentication.k8s.io/v1/selfsubjectreviews", method="POST", ctx=ctx,
                headers={"Authorization": "Bearer " + idt, "Content-Type": "application/json"},
                body={"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}, timeout=10)
    name = cl["metadata"]["name"]
    if s in (200, 201):
        u = json.loads(b)["status"]["userInfo"]
        print(f"   {name:22} {kind:6} -> {s} user={u.get('username')} groups={u.get('groups')}")
    else:
        print(f"   {name:22} {kind:6} -> {s} {red(b[:110].decode(errors='replace'))}")
