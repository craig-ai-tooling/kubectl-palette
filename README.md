# kubectl-palette

A kubectl plugin for Spectro Cloud Palette. Log in once in a browser, name a cluster,
and plain `kubectl` in that terminal runs against it. Every cluster you reach also becomes
a context in your kubeconfig, so `kubectl --context <name>` works in any terminal.

Status: prototype. The commands are `login`, `token`, `sync`, `use` (with `--admin` and a
fuzzy picker), `exec`, `contexts`, `current`, `logout`, `shell-init`, and tab completion of
cluster names. Releases are built by goreleaser and install through krew. See
[`docs/design.md`](docs/design.md) for the design, what was checked against a live tenant
and what was not.

## Install

**With krew**, from the latest release:

```
kubectl krew install --manifest-url=https://github.com/craig-ai-tooling/kubectl-palette/releases/latest/download/palette.yaml
kubectl palette version
```

**Without krew**, download the archive for your OS and architecture from the
[releases page](https://github.com/craig-ai-tooling/kubectl-palette/releases), unpack it,
and put `kubectl-palette` on your PATH.

**From source:**

```
go build -o ~/bin/kubectl-palette ./cmd/kubectl-palette
```

`kubectl krew uninstall palette` leaves two things behind: the `kubectl_complete-palette`
link in krew's bin directory and the contexts in `~/.kube/config`. Run
`kubectl palette logout --all` before uninstalling, then remove the link by hand.

### Cutting a release

Pushing a `v*` tag runs `.github/workflows/release.yml`. It runs the tests, builds the
archives and `checksums.txt` with goreleaser, then generates the krew manifest with
`hack/krew-manifest.sh` and attaches it to the release as `palette.yaml`. The manifest is
never edited by hand. To try the krew path before tagging:

```
goreleaser release --snapshot --clean
VERSION=$(jq -r .version dist/metadata.json)
hack/krew-manifest.sh "$VERSION" "https://github.com/craig-ai-tooling/kubectl-palette/releases/download/v$VERSION" > dist/palette.yaml
kubectl krew install --manifest=dist/palette.yaml --archive="dist/kubectl-palette_${VERSION}_linux_amd64.tar.gz"
```

## Quick start

Log in once with the Palette server and your tenant (org) name. Nothing else is needed.
If the server URL already identifies one tenant, `--tenant` can be left out. The client
secret is optional.

```
kubectl palette login --server <url> --tenant <org>
kubectl palette login --server https://palette.example.com --tenant acme    # for example
```

If the server does not publish the tenant UID for your org, the login says so. Pass the
UID (the last path element of the OIDC issuer URL in an OIDC file from any cluster) as
`--tenant`, or read it from such a file:

```
kubectl palette login --server https://palette.example.com --tenant 0123456789abcdef01234567
kubectl palette login --from-kubeconfig ~/Downloads/some-cluster.yaml
```

A private CA: add `--ca-file ca.pem`. It is saved with the tenant and trusted next to the
system roots. TLS verification is never turned off.

Several tenants stay logged in at once, on one server or several. Each login saves its own
entry, named after the org (or `--name`), and makes it active.

```
kubectl palette login --server https://palette.example.com --tenant globex
kubectl palette tenants                      # entries, active marker, logged in or expired
kubectl palette tenants --available          # orgs on this account with no entry yet
kubectl palette tenant acme                  # switch the active tenant
kubectl palette use web --tenant globex      # or pick per command, or set KUBECTL_PALETTE_TENANT
kubectl palette logout --tenant globex       # one tenant (--all for every tenant)
kubectl palette login --tenant acme          # log in to a saved tenant again
```

Existing single-tenant state keeps working as the `default` tenant, with no migration.
Cluster files of a named tenant are under `~/.kube/palette/.tenants/<name>/`.

Install the shell hook once (zsh or bash). Without it, `use` opens a subshell.

```
eval "$(kubectl-palette shell-init zsh)"     # put this in ~/.zshrc
```

If `kubectl` is an alias in your shell (for example `alias kubectl=kubecolor`), put the
hook line after the alias. The hook takes over the alias and still runs it for every
command other than `kubectl palette use`.

Turn on tab completion of cluster names (kubectl 1.26 or newer). This puts a
`kubectl_complete-palette` link next to the binary; kubectl runs it to complete
`kubectl palette use <TAB>` and `kubectl palette exec <TAB>` from the local index, with no
network call. Use `--dir <dir>` to put the link in another directory on PATH.

```
kubectl palette completion install
```

Then name a cluster and use plain kubectl:

```
kubectl palette use payments-prod            # or project/cluster when names collide
kubectl palette use                          # fuzzy picker over project/cluster (--no-fuzzy: numbered list)
kubectl get pods
kubectl palette use payments-prod --admin    # only if you hold the admin download permission
kubectl palette exec payments-prod -- get pods
kubectl palette current
kubectl palette sync                         # rebuild the cluster index now
kubectl palette use payments-prod --refresh  # fetch the kubeconfig again (also: exec --refresh), e.g. after the endpoint or CA changed
kubectl palette logout                       # removes the selected tenant's session, index, admin files, contexts
```

Cluster files of the default tenant are written to `~/.kube/palette/<project>/<cluster>.yaml`
(mode 600; admin files go to `<project>/.admin/<cluster>.yaml`) and hold no token or secret.

## Contexts

Every cluster you reach through OIDC also becomes a context in your normal kubeconfig
(`~/.kube/config`). `kubectl config get-contexts`, `kubectl config use-context <name>` and
`kubectl --context <name>` then work in any terminal, later, without running the plugin
yourself.

What is written, per cluster: one cluster entry and one context, both named after the
cluster (`cluster`, then `project/cluster`, then `<tenant>/project/cluster` when a name is
taken). One user entry per tenant entry runs `kubectl-palette token`, with `--tenant <name>`
for a named tenant. No token, client secret or certificate is written. Admin credentials
are never written there: `use --admin` and `exec --admin` add no context. The plugin
records what it created in `~/.config/kubectl-palette/contexts.json`, never touches an entry
that is not in that list, and never changes `current-context`.

When contexts are added:

- `login` adds every cluster of a tenant that has 25 or fewer. A larger tenant adds
  contexts as you use clusters, or all at once with `contexts add --all`.
- `use` and `exec` add the cluster they use, once. A second `use` writes nothing.
- The first time a context is added the plugin prints `added context <name> to <file>`.

```
kubectl palette contexts                      # context, tenant, project/cluster (and the file, if not the current one)
kubectl palette contexts add web api          # by name, as for use
kubectl palette contexts add --all            # every cluster of the selected tenant
kubectl palette contexts remove web           # by context name
kubectl palette contexts remove --all         # the selected tenant (--all-tenants: every tenant)
kubectl palette contexts prune                # drop clusters Palette no longer lists
kubectl --context web get pods                # any terminal, no plugin command needed
```

`use` sets the current context of one terminal only. It points `KUBECONFIG` at a small
per-terminal file that holds only `current-context`, followed by your kubeconfig:

```
KUBECONFIG=~/.config/kubectl-palette/terminals/<shell pid>-<id>.yaml:~/.kube/config
```

In that terminal `kubectl config get-contexts` lists every context with `*` on the one in
use, and `kubectl config use-context <name>` switches that terminal only. Other terminals,
tmux panes and the `current-context` in your kubeconfig do not change. A later `use` in the
same shell rewrites the same file. Other files you had in `KUBECONFIG` stay in the list.
`kubectl palette current` names the cluster behind the terminal's context.
`kubectl palette use --unset` puts the terminal back on your own kubeconfig.

kubectl writes new entries to the first file in `KUBECONFIG`. A cluster added in a pinned
terminal (`aws eks update-kubeconfig`, `kubectl config set-context`) lands in that
terminal's file and only that terminal sees it. The next `use` leaves that file as it is,
starts a new one and names what stayed behind. Run those tools after
`kubectl palette use --unset` to put them in your kubeconfig.

With `--admin`, `--no-context` or contexts turned off, `use` points `KUBECONFIG` at the
cluster's own file instead, as before. The per-terminal files hold no credentials of their
own and are not deleted automatically: kubectl skips a missing file without a word, so
deleting the file of an open terminal would move it to your global context.

`logout` removes the tenant's contexts. `logout --keep-contexts` leaves them and
`logout --all` removes every one for the current context file, also when no tenant is saved.
A session that merely expires leaves them in place.

Each context's user runs `kubectl-palette token --tenant <name> --issuer-id <id>`. The id
names the Palette login the context was made for. If a tenant entry later holds another
login, `token` prints nothing and says so, and `kubectl palette contexts prune` removes the
stale contexts. Contexts left behind by `logout --keep-contexts` are removed when a login
creates a new entry of the same name.

Limits:

- `$KUBECONFIG` is not consulted. If it is set and does not list the context file, kubectl in
  that shell will not see the contexts, and the plugin prints one note saying so.
- kubectl's own writes (`kubectl config set-context` and the like) are not coordinated with
  the plugin's. Two writes at the same moment can lose one. Run the plugin's commands and
  kubectl's config commands one at a time.
- The paste-back login has no PKCE, because Palette's discovery document advertises none.
  The code is protected only by the state check and its short life.

Turn contexts off with `KUBECTL_PALETTE_NO_CONTEXTS=1`, or per call with `login --no-contexts`,
`use --no-context` and `exec --no-context`. `KUBECTL_PALETTE_CONTEXT_FILE` writes them to
another file. With `KUBECTL_PALETTE_KUBEDIR` set and no context file named, they go to
`<kubedir>/contexts.yaml`, so a redirected setup never touches `~/.kube/config`.

A context keeps working while the login session does. Each `kubectl --context` call runs
`kubectl-palette token`, which needs the plugin on PATH and a valid session. With Palette
as the identity provider a token lasts up to 60 minutes, and renewing it needs Palette.
Contexts remove the need to look clusters up in Palette. They do not remove the need for
Palette to issue tokens. When the session is gone, run `kubectl palette login`.

## No browser

On a machine with no browser, `login` skips opening one. That happens with `--no-browser`,
in an ssh session (`SSH_CONNECTION` or `SSH_TTY`), and on Linux with no `DISPLAY` or
`WAYLAND_DISPLAY`. It prints the login URL. Two ways to finish:

- Open the URL in a browser on any machine. The browser ends on a `localhost` address that
  fails to load. Copy that address from the address bar and paste it into the terminal where
  `login` is waiting.
- Or forward the login port from your own machine and open the URL there:
  `ssh -L 8000:localhost:8000 <this host>`. Use the port shown in the URL's `redirect_uri`.

Paste-back works whenever stdin is a terminal, also when a browser was opened.

## Virtual clusters

A virtual cluster is marked `virtual on <host>` in the picker, in the numbered list and in
the picker's detail pane, or just `virtual` when its host is not in your index. To work
with the virtual clusters of one host cluster:

```
kubectl palette use --host <host cluster>            # picker limited to that host's virtual clusters
kubectl palette use web-vc --host <host cluster>     # name resolution limited the same way
kubectl palette contexts add --all --host <host cluster>
```

A host with no virtual clusters is an error. Virtual clusters with OIDC are not verified
against a live tenant.

## Develop

Go is the only requirement.

```
go mod download
go build ./...
go test ./...
go vet ./...
test -z "$(gofmt -l .)"
```

## Repo layout

| Path | What lives here |
|---|---|
| `cmd/kubectl-palette/` | CLI entry point |
| `internal/` | Application packages |
| `docs/` | Design and security rules |
| `hack/` | Probes and scratch tooling |
| `ralph/` | Autonomous loop: plan, prompt, progress notebook |
| `.specify/` | Spec Kit: constitution and templates |

## Working in this repo

Read [`AGENTS.md`](AGENTS.md) first. The rules are enforced by hooks in
`.claude/hooks/` and by CI, not by prose.

Work comes in three lanes: a Spec Kit feature, a quick fix logged in
`docs/fixes-log.md`, or an issue labelled `claude-fix` that opens a PR.
