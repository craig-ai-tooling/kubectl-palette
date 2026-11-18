# kubectl-palette design

Status: prototype design, 11/18/26. Facts under "Measured" were checked against a live
Palette SaaS tenant on that date. Anything under "Open" is not known yet. The contexts,
no-browser login and virtual cluster work below was built and tested against fakes only.

## Goal

One browser login, then name a cluster and run plain `kubectl` against it. Every cluster
the user reaches is also a context in their kubeconfig, so it works in any terminal with
no plugin command. Fast for a user with one cluster and for an admin with a thousand.
First target: a platform team replacing Tanzu's `kubectl vsphere login`.

## User experience

```
kubectl palette login --server https://palette.example.com --tenant acme   # browser opens once
kubectl palette use payments-prod              # this terminal now targets it
kubectl get pods                               # plain kubectl
kubectl palette use                            # no name: fuzzy picker over the local index
kubectl palette use payments-prod --admin      # admin kubeconfig, if the user holds the permission
kubectl palette exec payments-prod -- get pods # one-shot, no shell state (scripts, CI)
kubectl palette use --unset                    # this terminal goes back to the user's own kubeconfig
kubectl palette contexts [add | remove | prune] # clusters as contexts in ~/.kube/config
kubectl palette current | sync | logout
```

- `use` sets `KUBECONFIG` for the current terminal to a single-cluster file under
  `~/.kube/palette/<project>/<cluster>.yaml`. Two terminals can target two clusters.
  `use` also records the cluster as a context (see "Contexts").
- A plugin cannot change its parent shell's environment. `use` therefore needs a shell
  hook (`eval "$(kubectl-palette shell-init zsh)"`). Without the hook, `use` opens a
  subshell. `exec` needs neither.
- Duplicate cluster names across projects resolve as `project/cluster`.
- With exactly one cluster in the index, `login` selects it.

## Contexts

Requested in review on 11/18/26. Before that, the design left the user's kubeconfig alone
and relied on `use` alone. A user who wants `kubectl --context x` in a second terminal, in a
script or in an IDE had no way to get it without running the plugin first. Now every OIDC
cluster a user reaches is also a context in their normal kubeconfig. The package is
`internal/kubectx`; the rules it enforces are in `docs/security.md`.

What is written. Per cluster, a cluster entry and a context with the same name. Per tenant
entry, one user whose only content is an exec block that runs `kubectl-palette token`,
with `--tenant <entry>` for a named entry. The context name is the cluster name, then
`project/cluster`, then `<tenant>/project/cluster`, then a numeric suffix, whichever is
free. It is chosen once per (tenant entry, cluster UID) and kept. A registry at
`<Home>/contexts.json`, shared by all tenant entries, lists what the plugin created.
Nothing that is not in the registry is ever changed or removed.

Where. `KUBECTL_PALETTE_CONTEXT_FILE` if set. Else, when `KUBECTL_PALETTE_KUBEDIR`
redirects the state, `<KubeDir>/contexts.yaml`, so test and live-check setups never touch
the real file. Else `~/.kube/config`. `DefaultPaths` resolves it once. A `Paths` built by
hand has no context file and the feature is off, which is what the tests rely on.
`KUBECTL_PALETTE_NO_CONTEXTS=1` turns the whole feature off.

When.

- One helper records a context for `use`, `exec`, the one-cluster login selection and
  `contexts add`. It is given the tenant entry, the index cluster and the OIDC file path.
  Admin mode never calls it and never reads an admin file for this.
- Fast path: if the registry already has (tenant entry, cluster UID) and the file was not
  fetched in this call, it does nothing. A warm `use` still writes no file.
- A failure to record is a warning on stderr. The command still succeeds.
- The first time a context is added it prints `added context <name> to <file>` on stderr.
- `login` fetches every cluster's OIDC file for a tenant of 25 clusters or fewer (eight at
  a time) and records them with one `UpsertAll`. A cluster with no OIDC is skipped and
  counted. A bigger tenant prints a hint: contexts are added as clusters are used, or with
  `contexts add --all`. `--no-contexts` skips this. Nothing here fails the login.
- `contexts add`, `remove`, `prune` and `list` work on the selected tenant entry, except
  `list` and `remove --all-tenants`, which cover every entry. `prune` rebuilds the index and
  removes the entry's records whose cluster is gone.
- `logout` removes the entry's contexts, `--keep-contexts` leaves them, `logout --all`
  removes every one. The legacy issuer-change wipe removes the entry's contexts too. A
  session that only expires leaves them.

Why `use` stays. It points `KUBECONFIG` at one per-cluster file the plugin owns, so a
terminal pinned to a cluster cannot drift: no `use-context` elsewhere and no edit of the
shared file changes it. A context in a shared file can change under a running session.
Both are kept: `use` for a terminal that must not drift, contexts for everything else.
`use --unset` prints `unset KUBECONFIG`, which the shell hook evals.

What a context depends on. Each call runs `kubectl-palette token`, so it needs the plugin
on PATH and a valid login session. With Palette as the identity provider a token lasts up to
60 minutes and renewing it needs Palette. Contexts remove the need to look clusters up in
Palette, not the need for Palette to issue tokens.

## Virtual clusters

The metadata listing marks a virtual cluster with the annotation `hostClusterUid`. The index
keeps `host_uid` and, when the host is in the same listing, `host` (its `project/cluster`).
The picker, its detail pane and the numbered list show `virtual on <host>`, or `virtual`
when the host is unknown. `use --host <cluster>` limits the picker and name resolution to
the virtual clusters that cluster hosts (`Index.HostedBy`). A host with no virtual clusters
is an error. `contexts add --all --host <cluster>` uses the same filter. `exec` has no
`--host`, and `current` stays path-based and reads no index.

## Tenants

One login covers one Palette tenant. Several tenants, on one server or on different ones,
stay logged in side by side. Each is a saved tenant entry with its own config, session,
index and cluster files.

```
kubectl palette login --server https://palette.example.com --tenant acme
kubectl palette login --server https://palette.example.com --tenant globex
kubectl palette tenants               # one line per entry: active marker, name, org, server, state
kubectl palette tenants --available   # also lists orgs on this account with no entry yet
kubectl palette tenant globex         # make globex the active entry (no name: show it)
kubectl palette use web --tenant acme # one command against another entry
kubectl palette logout --tenant acme  # one entry; --all for every entry
```

Layout. The entry `default` is the original flat layout and needs no migration:
`<Home>/config.json`, `session.json`, `index.json`, and cluster files at
`<KubeDir>/<project>/<cluster>.yaml`. A named entry lives in `<Home>/tenants/<name>/` and
`<KubeDir>/.tenants/<name>/<project>/<cluster>.yaml`. `<Home>/active` holds the active
entry name. Entry names match `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$` and cannot be
`default` for a named entry. `.tenants` is reserved like `.admin`, so no project can
collide with it.

Contexts are shared across entries in one kubeconfig, so two entries that hold a cluster
with the same name get different context names, and each context's user carries its own
`--tenant`.

Selection, for every command except `token`: `--tenant`, then `KUBECTL_PALETTE_TENANT`,
then `<Home>/active`, then `default` if its config exists, then the only entry if there is
exactly one. A name that matches no entry is an error that lists the saved ones. A stale
active marker is ignored.

`token` is different. With no flag it always means `default`, never the active entry, and
`token --tenant <name>` means exactly that entry. Cluster files written for a named entry
carry `args: [token, --tenant, <name>]`. A cluster file therefore never sends one tenant's
token to a cluster in another tenant, whatever is active when kubectl runs.

### Resolving a server and tenant

`login --server <url> [--tenant <org | tenant UID>]` needs no OIDC file and no client
secret. Checked live against SaaS on 11/18/26:

- The issuer is `<scheme>://<rootDomain>/v1/oidc/tenant/<tenantUid>` and the OIDC client
  id is the tenant UID (24 lowercase hex). The client secret is optional.
- `GET <base>/v1/auth/org` needs no auth. `orgName` is `root` on the shared root host and
  the org name on `https://<org>.<rootDomain>`. An unknown org host answers 404
  `TenantNotFound`.
- The tenant UID is not a field of that reply. It is the `tenant` value inside the base64
  `state` parameter of the SSO `redirectUri` entries (accepted only if it is 24 lowercase
  hex). Some tenants publish no usable entry. Then the user passes the UID or an OIDC
  file with `--from-kubeconfig`.
- `<base>/v1/oidc/tenant/<uid>/.well-known/jwks.json` is 200 for a real UID and 404
  otherwise. The discovery document's `issuer` is canonical (on SaaS always the root host).
- With a session, `GET /v1/users/me` gives `status.tenant.orgName` and `tenantUid`, and
  `GET /v1/auth/orgs` lists the orgs the account belongs to.

Steps: normalize the server to scheme and host (https required, loopback excepted), find
the org host, read the UID, confirm it with JWKS, read the issuer from discovery, and
require the issuer host to be the server host, the root domain or a subdomain of it. The
API URL is the issuer's scheme and host unless `--api-url` is given. The entry name is
`--name`, else the org name, else the `orgName` from `/v1/users/me`. An entry that holds a
different issuer is never overwritten: the user passes `--name`. Self-hosted and dedicated
installs are assumed to use `<tenant>.<domain>` hosts the same way. That is not verified.

The older forms (`--from-kubeconfig`, `--issuer` with `--client-id`, `--bootstrap-json`,
`--import-kubelogin-cache`) still work. They configure the entry named by `--name`
(default `default`) and wipe that one entry if its issuer changes.

`--ca-file <pem>` saves an absolute path in the entry's config. Every request for that
entry trusts the file in addition to the system roots. There is no insecure flag.

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
| `login` | 1 (metadata index) after the browser, plus up to 25 kubeconfig fetches for contexts | under 3 s for the index; the contexts take longer, 8 at a time |
| `use <cluster>`, first time | 1 (kubeconfig fetch), then cached | under 5 s |
| `use <cluster>`, cached | 0, one registry read, no file written | under 100 ms |
| completion, picker | 0, local index only | under 50 ms completion, under 100 ms to first paint |
| `kubectl` after `use` | 0, cached OIDC token | no added latency |

Rules that follow from the table:

- Do not fetch kubeconfigs in bulk, with two exceptions the user can see and switch off:
  `login` for a tenant of 25 clusters or fewer, and `contexts add --all`. Both download
  eight at a time. Otherwise it is one fetch, on first use of a cluster.
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
  `project/cluster`. Subcommand names and flags are completed too, but nothing after `--`.
  Cluster names come from the selected tenant's index. `--tenant` and `tenant <TAB>`
  complete saved entry names.
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
- Headless hosts: `login` skips the browser with `--no-browser`, in an ssh session, or on
  Linux with no display (`oidc.Headless`). When stdin is a terminal it also reads a pasted
  redirect address, so the URL can be opened on any machine. A headless login prints a hint
  for the other route, `ssh -L 8000:localhost:8000 <this host>` (8000 is the first default
  port; the URL's `redirect_uri` shows the port in use). A device-code flow would replace
  both.

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
  Using it as an API session works today but is undocumented. See "Gaps a product version would close".
- A hand-rolled authorization-code exchange (`hack/oidc-probe.py`) was rejected with
  "session not found" for a freshly issued code. kubelogin's exchange succeeds. The only
  visible difference in the auth request is kubelogin's `access_type=offline`. Cause not
  confirmed; the plugin uses the same Go oauth2 library as kubelogin.

## Gaps a product version would close

1. Bless an API path for OIDC logins: either accept the ID token on the Palette API or
   document the refresh token as a session token. The prototype depends on behavior that
   is not documented.
2. Add a device-code flow for headless hosts.
3. Confirm the metadata index stays a single fast call at 1,000 or more clusters, or
   add a lightweight name and uid listing.
4. Ship the plugin with Palette (served from the console, as Tanzu serves its plugin).

## Open

1. Does the metadata call stay a single fast call at 1,000 clusters? Seen at 131 only.
   The login contexts limit (25) is a guess at what stays fast, not a measurement.
2. Self-hosted Palette and VerteX: the `<tenant>.<domain>` host scheme is assumed, not
   verified. Private CAs are handled by `--ca-file`; there is no `--insecure`.
3. Clusters on a customer's own IdP instead of Palette as IdP: cluster access works
   through that IdP, but the API session above does not exist, so the index needs an
   API key.
4. Refresh-token behavior after 4 hours idle, and with tenant SSO or MFA enforced.
5. Contexts and the no-browser paths were tested against fakes only. Not verified: macOS,
   Windows, zsh (the hook is tested under bash), self-hosted and dedicated installs,
   virtual clusters with OIDC, and the real browser login through `--server`.
6. A cluster that is deleted in Palette keeps its context until `contexts prune` or
   `logout`. There is no background prune.

## Prior art

- `kubectl vsphere login`: SSO to a JWT, contexts written into `~/.kube/config`, 10 hour
  expiry.
- kubeswitch: per-terminal `KUBECONFIG` switching over large fleets with a search index.
  It has no Palette store. Its shell-hook and index ideas are reused here.

## Packaging

Single Go binary `kubectl-palette`. goreleaser (`.goreleaser.yaml`) builds linux, darwin and
windows on amd64 and arm64, stamps `main.version` through ldflags, and writes tar.gz
archives (zip on Windows) that include `LICENSE`, plus `checksums.txt`.

The krew manifest is `deploy/krew/palette.yaml`, a template. `hack/krew-manifest.sh
<version> <base-url>` fills the version, URIs and sha256 values from `dist/checksums.txt`,
so the manifest is never edited by hand. Windows entries use `bin: kubectl-palette.exe`.
Each release carries a generated `palette.yaml`, so krew installs it with
`--manifest-url` until it is submitted to the public krew-index.

`.github/workflows/release.yml` runs goreleaser on a `v*` tag and attaches the generated
krew manifest to the release (see the README).
