# kubectl-palette

A kubectl plugin for Spectro Cloud Palette. Log in once in a browser, name a cluster,
and plain `kubectl` in that terminal runs against it. No kube contexts to manage.

Status: prototype. Most commands print "not implemented yet". See
[`docs/design.md`](docs/design.md) for the design and the open questions.

## Planned usage

```
kubectl palette login
kubectl palette use payments-prod
kubectl get pods
kubectl palette exec payments-prod -- get pods
kubectl palette current | sync | logout
```

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
