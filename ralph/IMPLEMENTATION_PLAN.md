# Implementation Plan

> Populated from Spec Kit `/speckit-tasks` output, then worked top-to-bottom by
> `ralph/loop.sh` — one task per iteration.

## Conventions

- `- [ ]` open, `- [x]` done, `- [!]` blocked (reason in `PROGRESS.md`)
- Keep priority order meaningful. The loop always takes the topmost open item.
- Each task must be independently completable and verifiable in one iteration. If it
  cannot be, split it.

Tasks 1 to 3 are blocked on the result of `hack/oidc-probe.py` (open question 1 in
`docs/design.md`). Do not start them until that result is recorded in `PROGRESS.md`.

---

## Tasks

- [!] 1. Config and token store (mode 600 files under the user's home)
- [!] 2. OIDC browser login and the `token` exec-credential command
- [!] 3. Cluster index from the metadata endpoint, with background refresh
- [ ] 4. `use` with kubeconfig fetch-and-cache, and the shell-init hook for zsh and bash
- [ ] 5. `exec`, `current` and `logout`
- [ ] 6. `--admin`
- [ ] 7. Completion and the fuzzy picker
- [ ] 8. goreleaser and the krew manifest

---

## Out of scope

Things deliberately NOT being done, so no iteration wanders into them.

- A UI, a daemon, or any write to `~/.kube/config`.
