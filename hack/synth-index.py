#!/usr/bin/env python3
"""Write a synthetic cluster index for timing runs, into a scratch KUBECTL_PALETTE_HOME.

usage: hack/synth-index.py <home-dir> <kube-dir> [clusters=1000] [projects=50]

Every tenth cluster reuses another project's name. The first cluster also gets a dummy
cached kubeconfig (no credentials) so `use` takes the warm path.
"""
import json, os, sys, datetime

home, kube = sys.argv[1], sys.argv[2]
n = int(sys.argv[3]) if len(sys.argv) > 3 else 1000
projects = int(sys.argv[4]) if len(sys.argv) > 4 else 50
clusters = []
for k in range(n):
    p = k % projects
    name = f"cluster-{k - 1 if k % 10 == 0 and k >= projects else k:05d}"
    c = {"name": name, "uid": f"uid-{k:08d}", "project_uid": f"puid-{p:03d}", "project": f"project-{p:02d}"}
    if k % 7 == 0:
        c["permissions"] = ["cluster.adminKubeconfigDownload"]
    if k % 13 == 0:
        c["no_oidc"] = True
    clusters.append(c)
os.makedirs(home, mode=0o700, exist_ok=True)
now = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
with open(os.path.join(home, "index.json"), "w") as f:
    json.dump({"updated_at": now, "clusters": clusters}, f)
c = clusters[1]
d = os.path.join(kube, c["project"])
os.makedirs(d, mode=0o700, exist_ok=True)
with open(os.path.join(d, c["name"] + ".yaml"), "w") as f:
    f.write("apiVersion: v1\nkind: Config\n")
print(f"wrote {n} clusters; warm cluster: {c['project']}/{c['name']}")
