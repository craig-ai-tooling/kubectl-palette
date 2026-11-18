# kubectl-palette

A kubectl plugin for Spectro Cloud Palette. Log in once in a browser, name a cluster,
and plain `kubectl` in that terminal runs against it. No kube contexts to manage.

Status: prototype. Every command in the plan works against a live tenant: `login`, `token`,
`sync`, `use` (with `--admin` and a fuzzy picker), `exec`, `current`, `logout`, `shell-init`,
and tab completion of cluster names. Releases are built by goreleaser and install through krew. See [`docs/design.md`](docs/design.md) for the design and
the open questions.

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

`krew uninstall palette` leaves the completion link described below; remove
`kubectl_complete-palette` from krew's bin directory by hand.

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

Log in once. The first time, point it at any Palette OIDC kubeconfig (download one from the
console) so it can read the issuer, client id and secret. They are saved to
`~/.config/kubectl-palette/config.json`, so later logins need no flags.

```
kubectl palette login --from-kubeconfig ~/Downloads/some-cluster.yaml
```

Install the shell hook once (zsh or bash). Without it, `use` opens a subshell.

```
eval "$(kubectl-palette shell-init zsh)"     # put this in ~/.zshrc
```

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
kubectl palette logout                       # removes the session, the index, admin files
```

Headless host: add `--no-browser` and forward port 8000 with `ssh -L 8000:localhost:8000`.
Cluster files are written to `~/.kube/palette/<project>/<cluster>.yaml` (mode 600; admin files go to `<project>/.admin/<cluster>.yaml`) and hold
no token or secret. `~/.kube/config` is never touched.

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
