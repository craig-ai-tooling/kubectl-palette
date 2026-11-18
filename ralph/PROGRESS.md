# Progress

> Append-only notebook. Newest at the bottom. Never edit or delete past entries —
> this is the loop's only memory across fresh contexts.

Each iteration appends:

```
## Iteration N — YYYY-MM-DD HH:MM
**Did:** one or two lines.
**Learned:** anything that would otherwise be rediscovered the hard way.
**Left:** what remains.
**Findings:** problems noticed but not fixed (ad-hoc bugs, smells, risks).
```

Findings are the point of this file as much as progress is. An observation that
dies in a context window is a bug you will pay for twice. Real ad-hoc fixes also
get a one-liner in `docs/fixes-log.md`.

When the whole plan is done and green, append a line containing exactly
`RALPH_COMPLETE` — `loop.sh` stops on it.

---

## Iteration 0 — template initialized
**Did:** Scaffolded the repo. No project work yet.
**Learned:** n/a
**Left:** Everything in `ralph/IMPLEMENTATION_PLAN.md`.
**Findings:** none

## Iteration 1 (11/18/26): tasks 1 to 6, core commands
**Did:** config and atomic file helpers, session store with file-locked refresh, OIDC browser
login and refresh (x/oauth2 + go-oidc), Palette API client, cluster index with background
refresh, kubeconfig rewrite, shell hook, and the commands `login`, `token`, `sync`, `use`
(`--admin` gated on the index permission), `exec`, `current`, `logout`, `shell-init`,
`version`. Table-driven tests on httptest servers, no live network.
**Learned:**
- Sending `Accept: application/json` to the `assets/kubeconfigclient` endpoint returns a
  document without the oidc-login exec user. Send no Accept header.
- The tenant has 132 clusters in the metadata call, 80 after filtering deleted ones.
- A refresh with the saved token endpoint works first try with oauth2 auth-style
  autodetect, rotates the refresh token, and the new refresh token works as the API session.
- fsync on the lab VM disk is slow (0.3 to 0.5 s per atomic write in tests). `token` on the
  warm path writes nothing, so the hot path is unaffected.
- `go build` in a worktree under another git repo needs `-buildvcs=false` (go stamps VCS
  info from the wrong root and fails). Not an issue in CI.

**Live timings (SaaS tenant, 80 clusters after filtering):**

| Command | Time |
|---|---|
| `sync` (2 concurrent calls) | 1.9 to 6.2 s (one 14.5 s outlier) |
| `use loyalty-dev` cold (1 kubeconfig fetch) | 1.1 to 1.8 s |
| `use loyalty-dev` warm (0 API calls) | 4 ms |
| `token`, valid cached ID token (0 network) | 2 to 4 ms |
| `token`, silent refresh | 0.8 s |
| `exec loyalty-dev -- get ns` (cached kubeconfig) | 87 ms |
| `exec voc-tenant-1 -- auth whoami` (first use, fetch included) | 1.8 s |
| bash hook: eval, `kubectl palette use`, `kubectl get ns`, `current` | 148 ms |

`auth whoami` showed the user's email and the Palette team groups.
**Left:** task 7 (completion and fuzzy picker), task 8 (goreleaser, krew). The browser login
was not exercised live (no browser on the lab VM); it is covered by the fake-issuer tests
and by `login --import-kubelogin-cache`. Custom CA file for self-hosted Palette is not
done. Windows builds have no file lock, so concurrent refreshes are not serialized there.
**Findings:** a miss on `use` costs a synchronous index refresh (2 concurrent calls) plus the
kubeconfig fetch, which exceeds the one-blocking-call rule on that path. The spec asked for
the refresh-and-retry, so it stays. The admin kubeconfig is cached after the first fetch and
never refetched; its certificate lifetime is unknown.
