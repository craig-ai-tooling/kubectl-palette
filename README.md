# kubectl-palette

A kubectl plugin for Spectro Cloud Palette. Log in once in a browser, name a cluster,
and plain `kubectl` in that terminal runs against it. No kube contexts to manage.

Status: prototype. Every command in the plan works against a live tenant: `login`, `token`,
`sync`, `use` (with `--admin` and a fuzzy picker), `exec`, `current`, `logout`, `shell-init`,
and tab completion of cluster names. Packaging (goreleaser archives and a krew manifest) is
in place but nothing is published. See [`docs/design.md`](docs/design.md) for the design and
the open questions.

## Install

A real `kubectl krew install palette` from a URL needs release assets that anyone can
download. This repo is private and has no release, so that path does not work today.
Two paths do.

**1. Copy the binary onto PATH.** Build it, or take one from `dist/` after a snapshot
build, and put it where kubectl finds plugins:

```
go build -o ~/bin/kubectl-palette ./cmd/kubectl-palette
```

**2. Install through krew from a local manifest and archive.** This is the same path a
published release will use, minus the download. It needs goreleaser, krew and kubectl.

```
goreleaser release --snapshot --clean
VERSION=$(jq -r .version dist/metadata.json)
hack/krew-manifest.sh "$VERSION" "https://github.com/craig-ai-tooling/kubectl-palette/releases/download/v$VERSION" > dist/palette.yaml
kubectl krew install --manifest=dist/palette.yaml --archive="dist/kubectl-palette_${VERSION}_linux_amd64.tar.gz"
kubectl palette version        # prints the stamped version, for example 0.0.1-next
kubectl krew uninstall palette
```

Use the archive that matches your OS and architecture. `hack/krew-manifest.sh` fills the
version, URIs and sha256 values in `deploy/krew/palette.yaml` from `dist/checksums.txt`;
the manifest is never edited by hand. `krew uninstall` leaves the completion link
described below; remove `kubectl_complete-palette` from krew's bin directory by hand.

When the owner publishes releases, pushing a `v*` tag runs `.github/workflows/release.yml`,
which runs goreleaser. Whether to make the repo public, publish a release or submit the
manifest to a krew index is the owner's decision. Nothing here does any of it.

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
kubectl palette logout                       # removes the session, the index, admin files
```

Headless host: add `--no-browser` and forward port 8000 with `ssh -L 8000:localhost:8000`.
Cluster files are written to `~/.kube/palette/<project>/<cluster>.yaml` (mode 600) and hold
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
