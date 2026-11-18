# Implementation Plan

> Populated from Spec Kit `/speckit-tasks` output, then worked top-to-bottom by
> `ralph/loop.sh` — one task per iteration.

## Conventions

- `- [ ]` open, `- [x]` done, `- [!]` blocked (reason in `PROGRESS.md`)
- Keep priority order meaningful. The loop always takes the topmost open item.
- Each task must be independently completable and verifiable in one iteration. If it
  cannot be, split it.

The login probe ran on 11/18/26 and unblocked tasks 1 to 3. Results are in
`docs/design.md` under "Login test results". The API session uses the refresh token as
the `Authorization` header; the ID token is for clusters only.

---

## Tasks

- [ ] 1. Config and token store (mode 600 files under the user's home)
- [ ] 2. OIDC browser login and the `token` exec-credential command
- [ ] 3. Cluster index from the metadata endpoint, with background refresh
- [ ] 4. `use` with kubeconfig fetch-and-cache, and the shell-init hook for zsh and bash
- [ ] 5. `exec`, `current` and `logout`
- [ ] 6. `--admin`
- [ ] 7. Completion and the fuzzy picker
- [ ] 8. goreleaser and the krew manifest

---

## Out of scope

Things deliberately NOT being done, so no iteration wanders into them.

- A UI, a daemon, or any write to `~/.kube/config`.
