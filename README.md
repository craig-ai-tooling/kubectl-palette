# kubectl-palette

A kubectl plugin for Spectro Cloud Palette. Log in once in a browser, name a cluster,
and plain `kubectl` in that terminal runs against it. No kube contexts to manage.

Status: prototype. Tasks 1 to 6 of the plan work against a live tenant: `login`, `token`,
`sync`, `use` (with `--admin`), `exec`, `current`, `logout`, `shell-init`. The fuzzy picker
(a numbered list for now), shell completion and packaging are not done. See
[`docs/design.md`](docs/design.md) for the design and the open questions.

## Quick start

Build and put the binary on your PATH under its plugin name:

```
go build -o ~/bin/kubectl-palette ./cmd/kubectl-palette
```

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

Then name a cluster and use plain kubectl:

```
kubectl palette use payments-prod            # or project/cluster when names collide
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
