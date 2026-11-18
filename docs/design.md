# kubectl-palette design

Status: prototype design, 11/18/26. Facts under "Measured" were checked against a live
Palette SaaS tenant on that date. Anything under "Open" is not known yet.

## Goal

One browser login, then name a cluster and run plain `kubectl` against it. No kube
contexts to manage. Fast for a user with one cluster and for an admin with a thousand.
First target: a platform team replacing Tanzu's `kubectl vsphere login`.

## User experience

```
kubectl palette login                          # browser opens once
kubectl palette use payments-prod              # this terminal now targets it
kubectl get pods                               # plain kubectl
kubectl palette use                            # no name: fuzzy picker over the local index
kubectl palette use payments-prod --admin      # admin kubeconfig, if the user holds the permission
kubectl palette exec payments-prod -- get pods # one-shot, no shell state (scripts, CI)
kubectl palette current | sync | logout
```

- `use` sets `KUBECONFIG` for the current terminal to a single-cluster file under
  `~/.kube/palette/<project>/<cluster>.yaml`. `~/.kube/config` is never read or written.
  Two terminals can target two clusters.
- A plugin cannot change its parent shell's environment. `use` therefore needs a shell
  hook (`eval "$(kubectl-palette shell-init zsh)"`). Without the hook, `use` opens a
  subshell. `exec` needs neither.
- Duplicate cluster names across projects resolve as `project/cluster`.
- With exactly one cluster in the index, `login` selects it.

## Measured (11/18/26)

- Palette is an OIDC provider per tenant: issuer
  `https://<console>/v1/oidc/tenant/<tenantUid>`, with `auth`, `token`, `userinfo` and
  JWKS endpoints. The discovery document advertises no device-code endpoint and no PKCE
  methods.
- Seven OIDC clusters in one tenant all carried the same issuer, the same client ID (the
  tenant UID) and the same client secret. One login token is valid on every
  Palette-IdP cluster in the tenant.
- The auth endpoint accepted `http://localhost:<port>` redirects on ports 8000, 18000
  and 9999, and accepted extra scopes `groups` and `offline_access` without an error at
  the redirect step. Whether those scopes change the token is open.
- `GET /v1/dashboard/spectroclusters/metadata` returns every cluster in one unpaginated
  call (131 items, 1.3 s, about 800 bytes each; it ignores `limit`). Each item has name,
  uid, `projectUid` and a `permissions` annotation that lists
  `cluster.adminKubeconfigDownload` when the caller holds it. Deleted clusters are
  included and must be filtered out.
- `POST /v1/dashboard/spectroclusters/search` caps at 50 per page (100 returns 422) and
  takes 1 to 2 s per page.
- `GET /v1/spectroclusters/{uid}/assets/kubeconfigclient` (the OIDC kubeconfig) took 1.2
  to 4.3 s and returns 404 on clusters without OIDC. Its user entry is an exec block for
  `kubectl oidc-login get-token`, with no certificate or token in the file.
- `GET /v1/spectroclusters/{uid}/assets/adminKubeconfig` returns a static client
  certificate.

## Speed budget

| Step | Palette API calls | Target |
|---|---|---|
| `login` | 1 (metadata index) after the browser | under 3 s after the browser returns |
| `use <cluster>`, first time | 1 (kubeconfig fetch), then cached | under 5 s |
| `use <cluster>`, cached | 0 | under 100 ms |
| completion, picker | 0, local index only | under 50 ms |
| `kubectl` after `use` | 0, cached OIDC token | no added latency |

Rules that follow from the table:

- Never fetch kubeconfigs in bulk. One fetch, on first use of a cluster.
- The index refreshes in the background when stale and never blocks a command. A name
  missing from the index triggers one synchronous refresh and one retry.
- A cached kubeconfig is refetched only after a TLS or connection failure against it.

## Auth

- Default is always OIDC. The plugin runs the authorization-code flow itself (listener on
  `localhost:8000`) and acts as the kubeconfig exec credential plugin
  (`kubectl-palette token`), so kubelogin is not a separate install.
- Admin kubeconfig is opt-in per command (`--admin`), offered only when the index shows
  `cluster.adminKubeconfigDownload` for that cluster, stored in a separate mode-600 file,
  and removed by `logout`. `current` shows when a terminal is in admin mode.
- A cluster without OIDC is listed and marked. `use` on it explains why and points at
  `--admin` when the user holds the permission.
- Group claims are required: platform teams bind RBAC to groups in the cluster profile.
- Headless hosts need `ssh -L 8000:localhost:8000` until Palette offers a device-code flow.

## Open

1. Does the Palette API accept the OIDC login token as `Authorization`? If yes, one
   browser login covers both the cluster index and cluster access. If no, the prototype
   uses an API key for the index and the gap becomes the request to engineering.
   Test: `hack/oidc-probe.py`.
2. Does the token carry groups (Palette teams), by default or with a `groups` scope?
3. Is a refresh token issued (`offline_access`), and how long does the ID token live?
4. Does the metadata call stay a single fast call at 1,000 clusters? Seen at 131 only.
5. Self-hosted Palette and VerteX: issuer host, private CA trust, no `--insecure`.

## Prior art

- `kubectl vsphere login`: SSO to a JWT, contexts written into `~/.kube/config`, 10 hour
  expiry.
- kubeswitch: per-terminal `KUBECONFIG` switching over large fleets with a search index.
  It has no Palette store. Its shell-hook and index ideas are reused here.
- Internal: PLT-1630 / PFR-925 asks for retrieving cluster credentials in the `palette` CLI.

## Packaging

Single Go binary `kubectl-palette`. goreleaser builds for macOS, Linux and Windows
(amd64, arm64). A krew manifest installs it from a private krew index until it is ready
for the public krew-index.
