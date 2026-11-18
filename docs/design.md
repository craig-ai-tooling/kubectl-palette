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
| completion, picker | 0, local index only | under 50 ms completion, under 100 ms to first paint |
| `kubectl` after `use` | 0, cached OIDC token | no added latency |

Rules that follow from the table:

- Never fetch kubeconfigs in bulk. One fetch, on first use of a cluster.
- The index refreshes in the background when stale and never blocks a command. A name
  missing from the index triggers one synchronous refresh and one retry.
- One exception to the one-blocking-call rule: `use <unknown-name>` and `exec <unknown-name>`
  do a synchronous index refresh (two concurrent API calls) and then, on first use, the
  kubeconfig fetch. That is two blocking steps. It stays because the alternative is
  "no such cluster" for a cluster created a minute ago, and the user would have to run
  `sync` by hand and retry. It happens only on a miss, once per new name, and only when the
  index was not just rebuilt. A name already in the index never takes this path.
- A cached kubeconfig is refetched only after a TLS or connection failure against it.

## Completion and picker

- kubectl (1.26 and newer) completes a plugin's arguments by running an executable named
  `kubectl_complete-palette` found on PATH. It receives the words typed after the plugin
  name and prints candidates one per line, then a cobra directive line (`:4`, no file
  completion). The binary answers as that completer when started under that name, so a
  symlink is enough. `kubectl palette completion install` creates it next to the binary
  (or in `--dir`). `shell-init` prints a one-line hint on stderr when the link is missing.
- Candidates come from the index file only. A name used in two projects is offered as
  `project/cluster`. Subcommand names, `--admin` and `--no-fuzzy` are completed too.
  A missing index gives no candidates and no error.
- `use` with no name on a TTY opens the fuzzy picker over `project/cluster`, with `admin`
  and `no-oidc` shown as words after the name, so typing them filters. Palette's metadata
  call does not say whether a cluster has OIDC, so the index learns it: a kubeconfig fetch
  that returns 404 sets `no_oidc` on that entry, a later successful fetch clears it, and a
  rebuild carries it over by cluster uid. `--no-fuzzy`, a missing terminal or Windows fall
  back to the numbered list.
- The picker is written against `golang.org/x/term`. go-fuzzyfinder (tcell) was tried first
  and its package init added about 26 ms to every process start, which would land on every
  `kubectl` call through the `token` exec credential.

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

## Login test results (11/18/26)

Run with `hack/kubelogin-probe.py` and `hack/token-followup.py` against a SaaS tenant,
using kubelogin v1.36.4 and one browser login.

- The login returns an ID token (60 minute lifetime) and a refresh token (4 hours).
- The ID token carries `groups`, populated with the user's Palette teams, with only the
  `openid profile email` scopes requested. No `groups` scope is needed.
- One ID token authenticated to 7 of 8 OIDC clusters in the tenant (4 direct, 3 through
  the Spectro proxy). Each cluster's SelfSubjectReview returned the user's email as the
  username and the Palette teams as groups. The eighth failed on a TLS hostname mismatch
  at its proxy endpoint before any auth took place.
- Silent refresh works: `--force-refresh` returned a new ID token in 0.6 s with no
  browser, and a new refresh token with a fresh 4 hour expiry. The session slides for
  as long as it is used at least once every 4 hours.
- A kubelogin call with a valid cached token takes 6 ms.
- The Palette API rejects the ID token (401, raw and Bearer).
- The Palette API accepts the refresh token as a raw `Authorization` header. With it,
  `/v1/users/me`, the cluster metadata index (2.1 s) and `kubeconfigclient` (0.8 s) all
  returned 200. One browser login therefore covers the cluster index, kubeconfig
  fetches and cluster access, with no API key.
- The refresh token is a Palette-signed JWT with the same session claim as the ID token.
  Using it as an API session works today but is undocumented. See "Ask to engineering".
- A hand-rolled authorization-code exchange (`hack/oidc-probe.py`) was rejected with
  "session not found" for a freshly issued code. kubelogin's exchange succeeds. The only
  visible difference in the auth request is kubelogin's `access_type=offline`. Cause not
  confirmed; the plugin uses the same Go oauth2 library as kubelogin.

## Ask to engineering

1. Bless an API path for OIDC logins: either accept the ID token on the Palette API or
   document the refresh token as a session token. The prototype depends on behavior that
   is not documented.
2. Add a device-code flow for headless hosts.
3. Confirm the metadata index stays a single fast call at 1,000 or more clusters, or
   add a lightweight name and uid listing.
4. Ship the plugin with Palette (served from the console, as Tanzu serves its plugin).

## Open

1. Does the metadata call stay a single fast call at 1,000 clusters? Seen at 131 only.
2. Self-hosted Palette and VerteX: issuer host, private CA trust, no `--insecure`.
3. Clusters on a customer's own IdP instead of Palette as IdP: cluster access works
   through that IdP, but the API session above does not exist, so the index needs an
   API key.
4. Refresh-token behavior after 4 hours idle, and with tenant SSO or MFA enforced.

## Prior art

- `kubectl vsphere login`: SSO to a JWT, contexts written into `~/.kube/config`, 10 hour
  expiry.
- kubeswitch: per-terminal `KUBECONFIG` switching over large fleets with a search index.
  It has no Palette store. Its shell-hook and index ideas are reused here.
- Internal: PLT-1630 / PFR-925 asks for retrieving cluster credentials in the `palette` CLI.

## Packaging

Single Go binary `kubectl-palette`. goreleaser (`.goreleaser.yaml`) builds linux, darwin and
windows on amd64 and arm64, stamps `main.version` through ldflags, and writes tar.gz
archives (zip on Windows) that include `LICENSE`, plus `checksums.txt`.

The krew manifest is `deploy/krew/palette.yaml`, a template. `hack/krew-manifest.sh
<version> <base-url>` fills the version, URIs and sha256 values from `dist/checksums.txt`,
so the manifest is never edited by hand. Windows entries use `bin: kubectl-palette.exe`.
Krew installs it from a private krew index until it is ready for the public krew-index.

`.github/workflows/release.yml` runs goreleaser on a `v*` tag. A real `krew install` from a
URL needs publicly downloadable release assets; this repo is private, so today the
supported paths are a local manifest with a local archive, and copying the binary onto PATH
(see the README).
