# kubectl-palette Constitution

The highest-authority document in this repo. Spec Kit reads it at
`/speckit-specify`, `/speckit-plan`, `/speckit-tasks`, and `/speckit-implement`.
Where anything conflicts with it — a spec, a plan, a PR, a preference — **this wins.**

Operational detail lives in `AGENTS.md`. Principles live here.

---

## Core Principles

### I. Security is a gate, not a review comment (NON-NEGOTIABLE)

[`docs/security.md`](../../docs/security.md) is binding. Every plan and every task
list is checked against it. A feature that cannot satisfy it does not ship.

Specifically, and without exception:
- Tokens and secrets are never written to a kubeconfig and never logged.
- Names from the API are checked against path traversal before they become paths.
- TLS verification is never disabled by default.
- No secret ever enters the repository.

### II. Design is binding (NON-NEGOTIABLE)

[`docs/design.md`](../../docs/design.md) defines the commands, the auth flow and the
speed budget.

- A command that is not in the design needs a design change first.
- Open questions in the design stay open until measured. Do not guess an answer.
- Output is plain text that a human or a script can read.

### III. Unknowns are surfaced, never invented (NON-NEGOTIABLE)

If a requirement is ambiguous, the artifact must say `NEEDS CLARIFICATION` and
stop. A plausible guess that turns out wrong costs more than a question.

This applies to specs, plans, tasks, PRs, and autonomous loop iterations alike.

### IV. Every change is verifiable

"It works" is not a claim, it is a demonstration. A change is done when the
build, test, and lint commands in `AGENTS.md` pass — the same commands CI runs.
No merging on a red or skipped check.

### V. Smallest correct change

Fix the thing asked for. Do not refactor adjacent code, rename things in passing,
or "improve" what was not in scope. Unrequested churn hides real changes in review.

### VI. Standards are enforced, not documented

A rule that lives only in prose is a suggestion. Rules belong in
`.claude/hooks/` and `.github/workflows/`, which are themselves tested
(`.claude/hooks/test-hooks.sh`). Changing a guardrail requires a reviewed PR —
that is the point, not friction to be avoided.

### VII. Speed is a feature

No command may make more than one blocking Palette API call. Completion and the
picker read only the local index. Kubeconfigs are fetched on first use only and never
in bulk. The speed budget table in [`docs/design.md`](../../docs/design.md) is the
target for every command.

### VIII. OIDC by default

Cluster access goes through OIDC. An admin kubeconfig is fetched only on an explicit
`--admin` flag and only when the user holds the permission.

### IX. Leave the user's kubeconfig alone

Each terminal gets its own `KUBECONFIG` file under `~/.kube/palette/`. The plugin adds
contexts to `~/.kube/config` only for OIDC clusters, only entries that run
`kubectl-palette token`, and only entries it records in its own registry. It never edits
anything else in that file and never changes `current-context`. Admin credentials are
never written there.

---

## Development Workflow

Three lanes, one set of rules. All inherit these principles.

| Lane | When | Path |
|---|---|---|
| **Feature** | More than one moving part | constitution → `/speckit-specify` → `/speckit-plan` → `/speckit-tasks` → `/speckit-implement` |
| **Quick fix** | One-line bug, typo, copy | Fix → verify → one line in `docs/fixes-log.md` |
| **Automated** | Issue labelled `claude-fix` | Runner → bounded run → **PR only, never `main`** |

Autonomous execution (`ralph/loop.sh`) works the plan **one task per iteration**
with a fresh context each time, and commits after each. It is bound by every
principle above.

---

## Quality Gates

A change may merge only when all of these are true:

- [ ] Build, test, and lint green (the `AGENTS.md` commands)
- [ ] New behaviour has a test that fails without the change
- [ ] `docs/security.md` checklist satisfied for anything touching input, authz, or secrets
- [ ] `docs/design.md` respected, including the speed budget
- [ ] `logout` removes the token cache and admin kubeconfigs, if auth was touched
- [ ] No `NEEDS CLARIFICATION` left unanswered
- [ ] Reviewed by a code owner (`.github/CODEOWNERS`)

---

## Governance

This constitution supersedes all other practice. Amending it requires a pull
request that states what changed, why, and what it breaks — reviewed by a code
owner like any other change.

Complexity must be justified in the plan. "It might be useful later" is not a
justification.

Agents: `AGENTS.md` is your runtime guide; this document is the authority behind it.

**Version**: 1.0.0 | **Ratified**: 2026-11-18 | **Last Amended**: 2026-11-18
