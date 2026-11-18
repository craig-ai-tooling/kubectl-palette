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
| `use cluster-a` cold (1 kubeconfig fetch) | 1.1 to 1.8 s |
| `use cluster-a` warm (0 API calls) | 4 ms |
| `token`, valid cached ID token (0 network) | 2 to 4 ms |
| `token`, silent refresh | 0.8 s |
| `exec cluster-a -- get ns` (cached kubeconfig) | 87 ms |
| `exec cluster-b -- auth whoami` (first use, fetch included) | 1.8 s |
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

## Iteration 2 (11/18/26): tasks 7 and 8, completion, picker, packaging
**Verified:** the plugin's own browser login (`kubectl palette login`) was run live by the user at
17:15 UTC on 11/18/26 and worked. That closes the gap noted in iteration 1. The session it
created was used for the read-only live checks below.
**Did:**
- Completion: hidden `__complete` command, and the binary answers as the completer when started
  as `kubectl_complete-palette` (argv[0]). `completion install [--dir]` makes the symlink next to
  the binary as a relative link to its sibling, so a krew bin link survives upgrades. `shell-init`
  prints a hint on stderr when the completer is not on PATH. Completes subcommands, `--admin`,
  `--no-fuzzy`, and cluster names (`project/cluster` for names used in two projects). Index only,
  no network. Checked through real kubectl 1.36 (`kubectl __complete palette use cluster-0000`).
- Picker: `use` with no name on a TTY opens an incremental fuzzy finder; `--no-fuzzy`, no TTY
  control, or Windows fall back to the numbered list. Shows `admin` and `no-oidc` after the name.
  The index now learns `no_oidc` from a 404 on the kubeconfig fetch (the metadata call has no
  OIDC field; keys checked live) and carries it over on rebuild.
- Packaging: `.goreleaser.yaml`, `deploy/krew/palette.yaml` (template) filled by
  `hack/krew-manifest.sh`, `.github/workflows/release.yml`, README "Install".
- Documented in `docs/design.md`: the `use <unknown-name>` exception to the one-blocking-call rule.
**Learned:**
- go-fuzzyfinder (tcell) adds about 26 ms of package init to every process start (`version`
  went from 2.5 ms to 30 ms). `kubectl-palette token` runs on every kubectl call, so it was
  dropped for a 300 line finder on golang.org/x/term. Check with `GODEBUG=inittrace=1`.
- A cluster with no OIDC cannot be known from the metadata listing, only from the fetch.
- krew uninstall leaves the `kubectl_complete-palette` link in krew's bin dir.
**Scale (synthetic, 50 projects, a tenth of names colliding):**

| In-process (go test -bench, 12 core) | 1,000 | 5,000 |
|---|---|---|
| load index.json | 1.4 ms | 8.4 ms |
| completion over the loaded index | 0.2 to 0.3 ms | 1.1 to 1.2 ms |
| `Resolve` (ref, name, prefix, miss) | 27 to 55 us | 114 to 448 us |
| picker filter, one keystroke | 0.14 ms | 0.66 ms |

| Real binary, scratch KUBECTL_PALETTE_HOME (median, p95 of 30 runs) | 1,000 | 5,000 |
|---|---|---|
| completer `use cluster-005` | 14.5 ms, 25.4 ms | 17.4 ms, 23.7 ms |
| completer `use ""` (every name) | 11.7 ms, 16.6 ms | 17.0 ms, 24.1 ms |
| `use` warm, cached kubeconfig | 9.0 ms, 17.9 ms | 14.5 ms, 27.1 ms |
| process floor (`version`) | 5.3 ms | 3.3 ms |
| picker, first paint on a pty (median of 15) | 10.8 ms (p95 13.0) | 21.1 ms (p95 25.2) |

Index files were made with `hack/synth-index.py`. The live tenant (80 clusters): picker first
paint 8 ms, `__complete use loy` 4 ms, `exec cluster-a -- get ns` 91 ms.
**Packaging proof:** `goreleaser release --snapshot --clean` (v2.20.2) built all six targets and
the archives; the linux binary prints the stamped version. krew v0.5.0 (sha256 checked against the
published file) in a scratch KREW_ROOT: `kubectl krew install --manifest=... --archive=...`, then
`kubectl palette version` with only krew's bin dir on PATH printed `0.0.1-next`, then
`kubectl krew uninstall palette`. Exact commands are in README "Install".
**Left:** nothing in the plan. The release workflow has not run: Actions is blocked on the org by
a billing problem. Nothing was published and no release exists. Not done or not checked: the
picker on macOS and in Windows terminals (Windows uses the numbered list); darwin and windows
archives were built but not run; the krew manifest was installed on linux/amd64 only; completion
was tested with bash-style `kubectl __complete`, not in an interactive zsh or fish; custom CA
file for self-hosted Palette and Windows refresh locking are still open from iteration 1.
**Findings:** `completion install` cannot know a krew upgrade moved the store path, which is why
it links by sibling name. A background `sync` started by `use` can overwrite a `no_oidc` flag set
a moment earlier (rare, the flag is a hint and the next failed fetch sets it again).

