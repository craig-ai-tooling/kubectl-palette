# Fixes Log

> The **quick-fix lane**. Append-only. Newest at the bottom.

For changes too small to deserve a spec: a typo, a one-line bug, a wrong copy
string, a missing null check. Fix it, prove it, log it here in one line.

**Format** — one line, no essays:

```
- YYYY-MM-DD — What was wrong; what you changed. (#issue-or-PR)
```

Anything that needs more than one line of explanation is not a quick fix. Run it
through Spec Kit instead (`/speckit-specify`).

Why this exists: without a log, ad-hoc fixes are invisible. A recurring entry here
is a signal that something needs a real design change rather than a fifth patch.

---

<!-- Append below. Do not edit or reorder existing entries. -->

- 2026-11-18 — kubectl-palette initialized from ai-project-template.
- 2026-11-20 — an expired session failed every kubectl call with a bare "run: kubectl palette login"; token, use and sync now say the session expired and open the browser login when stderr is a terminal (KUBECTL_PALETTE_NO_AUTO_LOGIN=1 turns it off).
