# AGENTS.md

Primary context file for **every** AI agent working in this repo (Claude Code, Copilot,
Cursor, Codex, or a human). Read this first. Everything else loads on demand.

## Why

kubectl-palette is a kubectl plugin for Spectro Cloud Palette. A user logs in once in a browser through Palette's OIDC provider, names a cluster, and plain kubectl in that terminal runs against it, with no kube contexts to manage. It keeps a local index of cluster names so it stays fast for one cluster or a thousand, and fetches a kubeconfig only the first time a cluster is used. It is a prototype to test the experience and propose it to Spectro Cloud engineering as a product feature; the first target user is a platform team replacing Tanzu's kubectl vsphere login.

---

## What — project map

| Path | What lives here |
|---|---|
| `cmd/kubectl-palette/` | CLI entry point |
| `internal/` | Application packages (index, config, token store) |
| `internal/**/*_test.go` | Test suites |
| `docs/` | Standards, loaded on demand (see Progressive disclosure) |
| `ralph/` | Autonomous loop: plan, prompt, progress notebook |
| `.specify/` | Spec Kit: constitution, templates, feature specs |
| `.claude/hooks/` | Enforced guardrails (see Enforcement) |
| `.github/workflows/` | CI + issue automation |

---

## How — the only commands that matter

Keep this table honest. A stale command here costs more than a missing one.

| Task | Command |
|---|---|
| Install | `go mod download` |
| Build | `go build ./...` |
| Test (all) | `go test ./...` |
| Test (single) | `go test ./internal/index -run TestName` |
| Lint | `test -z "$(gofmt -l .)"` |
| Format | `gofmt -w .` |
| Typecheck | `go vet ./...` |

**Definition of done** — a change is not done until build, test, and lint all pass.
CI runs exactly these commands; do not hand-wave them locally.

---

## Three lanes of work

All three inherit the same hooks, the same constitution, the same docs.

**1. Feature — Spec Kit + Ralph.** Anything with more than one moving part.
```
/speckit-constitution → /speckit-specify → /speckit-plan → /speckit-tasks → /speckit-implement
```
Templates must flag unknowns as `NEEDS CLARIFICATION` rather than guessing. If the
spec is ambiguous, stop and ask — do not invent requirements.

**2. Quick fix.** A typo, a one-line bug, a bad copy string. No spec, no ceremony.
Fix it, prove it, then append one line to `docs/fixes-log.md`:
```
- 2026-01-15 — logout left the token cache on disk; remove it in logout. (#42)
```

**3. GitHub issue → automated PR.** Label an issue `claude-fix`. A runner picks it
up, works under these same rules, and opens a PR. It never pushes to `main`.

---

## Progressive disclosure — load only what the task needs

Do not read all of these. Read the one that matches what you are about to change.

| Touching… | Read |
|---|---|
| Commands, UX, auth flow, speed budget, open questions | `docs/design.md` |
| Tokens, kubeconfigs, file paths, TLS, logging | `docs/security.md` |
| Anything at all | this file |

---

## Conventions

One example each. Match the surrounding code over the example if they conflict.

**Naming** — gofmt everything. Package names are lower-case single words. Exported identifiers have a doc comment.
```go
// Index is the local cache of cluster names read by completion and the picker.
type Index struct{ ... }
```

**Errors** — never swallow. Fail loud, with context.
Wrap with `%w`. User-facing errors say what to run next.
```go
return fmt.Errorf("fetch kubeconfig for %s: %w", name, err)
```

**Tests** — table-driven. An `httptest` server stands in for Palette. No live network in `go test`.
```go
for _, tc := range []struct{ name, in, want string }{{"plain", "a", "a"}} {
	t.Run(tc.name, func(t *testing.T) { /* assert tc.want */ })
}
```

**Comments** — explain *why*, never *what*. If the code needs a "what" comment,
rewrite the code.

---

## Enforcement — not suggestions

Standards live in `.claude/hooks/` and `.github/workflows/`, not in prose. Prose
gets skimmed; hooks do not.

| Hook | Fires on | Does |
|---|---|---|
| `danger-guard.sh` | `PreToolUse` (Bash) | **Blocks** `rm -rf /`, `rm -rf ~`, force-push to main/master, `git reset --hard origin`, `DROP`/`TRUNCATE TABLE`, `mkfs`, `dd` to a device. Exit 2 with a reason. |
| `format-and-lint.sh` | `PostToolUse` (Write/Edit) | Formats + auto-fixes the changed file. No-ops if the tool is not installed. |
| `session-start.sh` | `SessionStart` | Injects git status, open TODOs, and this checklist. |

Do not disable, bypass, or route around a hook. If a guard is wrong, fix the guard
in a PR — that is a code change with a review, which is the point.

---

## Hard rules

- **Never commit secrets.** No keys, tokens, or `.env` files. Secret scanning is on.
- **Never push to `main`.** Branch, PR, review. The automation follows this too.
- **Never invent a requirement.** Unknown → `NEEDS CLARIFICATION` → ask.
- **Never leave the tree broken.** Build + test + lint green before you commit.
- **Prefer editing over creating.** A new file needs a reason.
- **Never write a token or secret into a kubeconfig, and never touch `~/.kube/config`.** See `docs/security.md`.
