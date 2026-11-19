# Security

> Loaded when: handling tokens, kubeconfigs, file paths, TLS or logging.
> Referenced by `.specify/memory/constitution.md` as a hard gate.

kubectl-palette is a CLI that handles credentials on a user's workstation. There is no
server in this repo. The threats are leaked tokens, clobbered kubeconfigs and unsafe
file paths.

## Non-negotiables

### 1. Credentials stay out of kubeconfigs
- Never write a token or secret into a kubeconfig file. The OIDC kubeconfig points at
  `kubectl-palette token` as an exec credential plugin and holds no token.
- `kubeconfig.Rewrite` takes a `RewriteOptions` struct, so a secret can never be passed
  where the tenant goes.
- Cached cluster files live under `~/.kube/palette/`. Terminal pins live under the plugin
  home (`~/.config/kubectl-palette/terminals/`, mode 700 directory, mode 600 files) and hold
  only a `current-context`. A pin that holds anything else (another tool wrote to it) is
  never rewritten; `use` starts a new one. The user's own kubeconfig is touched only
  through `internal/kubectx` (rule 3b), never by any other code path.

### 2. File modes
- The token cache and any admin kubeconfig are mode 600, under the user's home.
- Directories that hold them are mode 700.
- Write to a temp file in the same directory, set the mode, then rename. Never create a
  file with a wider mode and narrow it later.
- `logout` removes the session and every admin kubeconfig (see rule 3).

### 3. Cache identity, admin files and logout
- A cached cluster file is keyed by the cluster UID, not the name. `<file>.uid` records the
  UID the file was fetched for; it is read without opening the credentials. On every cache
  hit the UID must equal the index entry. A mismatch, or a file with no sidecar (older
  layout), is deleted and fetched again. `use --refresh` and `exec --refresh` force it.
- Admin kubeconfigs live apart, in `<kubedir>/<project>/.admin/<cluster>.yaml`. `.admin`
  is rejected as a project or cluster name, so the two kinds can never share a path
  whatever a cluster is called.
- The admin download permission is checked on every use, cache hit or not. If the index no
  longer shows it, the cached file is deleted and the command refuses.
- `logout` takes the session lock, so an in-flight refresh finishes first and cannot save a
  session afterwards. It tries every directory, prints each path it could not remove and
  exits non-zero. It never reports success while admin files remain. It also removes the
  flat `<cluster>.admin.yaml` files an older version left behind.
- A login to a different issuer is a different Palette installation. The API URL is derived
  again from the new issuer (or taken from `--api-url`), never carried over, and the old
  session, index and every cached cluster file, OIDC and admin, are removed before the new
  config is saved. This wipe applies to the one tenant entry being logged in, and only on
  the older `--from-kubeconfig` or `--issuer` form. A `--server` login never wipes
  anything: a name that holds a different issuer is an error.

### 3a. Tenant entries
- `token` with no flag always means the `default` entry, never the active one, and never
  reads `KUBECTL_PALETTE_TENANT`. A named entry's cluster files call
  `token --tenant <name> --issuer-id <id>`.
  A cluster file must not be able to send one tenant's token to a cluster in another
  tenant because the active entry changed.
- Entry names are validated (`^[a-z0-9][a-z0-9._-]{0,62}$`, no trailing dot, not `default`)
  before they become a directory name. They are lowercase so two names can never be one
  directory on a case-insensitive filesystem. `.tenants` is rejected as a project or
  cluster name.
- Every rewritten file carries `--issuer-id <id>` (`config.IssuerID`: first 16 hex of the
  SHA-256 of the issuer, trailing slashes trimmed). `token` prints nothing and fails when
  the id differs from the entry's saved issuer, and when a named entry gives no id. A
  tenant name that is reused for another Palette therefore cannot receive tokens for the
  old clusters, even if their contexts were kept by `logout --keep-contexts`.
- `Resolve` with `--tenant <org>` requires the org host's reply to name that org
  (case-insensitive).
- A `--server` login calls only unauthenticated endpoints before the browser step. The
  issuer it reads from discovery must be https (or loopback) and its host must be the
  server host, the root domain or a subdomain of the root domain, else the login stops.
- Logout of a named entry removes its whole directory, including the config, and clears
  `<Home>/active` if it pointed there.

### 3b. The context file
`internal/kubectx` adds contexts to the user's kubeconfig (`~/.kube/config` by default).
It is the only code that writes there.

- Only exec-credential users are written. It refuses a source whose user has a client
  certificate or key, a token or token file, a username or password, an auth provider or
  impersonation. Admin kubeconfigs therefore can never land in the file, and the cli
  never even reads an admin file for this.
- The exec block must run exactly `kubectl-palette` (no path), with no env, and args
  `token [--tenant <name>] [--issuer-id <id>]` and nothing else. A source with any other
  argument is refused. The tenant must equal the entry the cluster belongs to, and a
  non-empty issuer id on the entry must equal the one in the args.
- A user entry is reused only when its args name the same tenant and issuer id, and no
  record of another tenant or issuer uses it. A context and cluster name of an existing
  record is reused only while the context still points at our cluster and user, or neither
  exists. Otherwise the record is dropped and a fresh name is chosen.
- A cluster entry is deleted only when no remaining context references it, like a user entry.
- Records store the absolute path of the kubeconfig they were written to and apply only to
  that file.
- The registry is written before the kubeconfig. If the kubeconfig write fails the previous
  registry is put back.
- The plugin records what it created in `<Home>/contexts.json`. It never changes or removes
  a cluster, context or user that is not in that registry. A context the user re-pointed is
  left alone on removal and only dropped from the registry. A user entry that is not a
  plain `kubectl-palette token` exec user is never overwritten or removed.
- `current-context` is never set. It is cleared only when the context it names is removed.
- A kubeconfig that does not parse is never overwritten. The command warns and carries on.
- Every change holds a lock on the registry, loads the file, edits in memory and writes
  the registry and the kubeconfig through a mode-600 temp file and a rename. A symlinked
  kubeconfig is written through to its target.
- A failure to record never fails the command that triggered it.
- Known limits. `$KUBECONFIG` is not consulted when choosing the context file or free names;
  when it is set and does not list the file, the plugin prints one note, because kubectl in
  that shell will not see the contexts. kubectl's own config writes are not coordinated with
  the plugin's registry lock, so a concurrent `kubectl config set-context` can lose an
  update. The paste-back login has no PKCE because Palette's discovery document advertises
  none, so the code is protected only by the state check and its short life.
- `token --tenant <name> --issuer-id <id>` is what pins a context to its tenant and login. A
  context from tenant A cannot ask for tenant B's token, and plain `token` is always the
  default entry.
- `KUBECTL_PALETTE_NO_CONTEXTS=1` turns it all off. Tests must keep the context file in
  `t.TempDir()`: a `Paths` with no context file disables the feature, and the cli tests
  run with `HOME` in a throwaway directory and fail if anything appears under it.

### 4. Never log secrets
- Never log tokens, client secrets or API keys, at any log level.
- Never print a full HTTP request or response body that could contain one.
- Error messages name the cluster and the step. They do not echo credentials.

### 5. TLS
- TLS verification is never disabled by default.
- There is no `--insecure` flag. Self-hosted Palette with a private CA is handled by
  `login --ca-file <pem>`. The absolute path is saved in the entry's config. Every request
  for that entry (resolution, login, refresh, API) trusts that file in addition to the
  system roots.

### 6. API names are untrusted input
Cluster and project names come from the Palette API. Treat them as untrusted when
building file paths.

- Reject names that are empty, `.`, `..` or `.admin`, or that contain `/`, `\` or a NUL byte.
- After joining, confirm the result is still inside the base directory.

```go
func safeJoin(base, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("unsafe name %q: refusing to build a path", name)
	}
	return filepath.Join(base, name), nil
}
```

### 7. Secret hygiene in the repo
- Secrets come from the environment or a secret manager. Never a source file.
- `.env` is gitignored.
- Rotate anything that has ever been committed. Assume it is public forever.
- GitHub secret scanning and push protection are enabled on this repo.

### 8. Dependencies
- Dependabot is enabled. Security updates get merged promptly.
- Every GitHub Action is pinned to a full 40-character commit SHA with a `# vX.Y.Z`
  comment. CI enforces this with `bash .github/scripts/check-action-pins.sh`.
- A new dependency needs a reason: what does it do that the standard library cannot?

## Pre-merge security checklist

- [ ] No secret added to the repo
- [ ] No token or secret written to a kubeconfig
- [ ] `~/.kube/config` is written only through `internal/kubectx`, only exec-credential users, and never in admin mode
- [ ] New files holding credentials are mode 600
- [ ] No token, client secret or API key reaches a log or error message
- [ ] No TLS verification bypass added
- [ ] Names from the API are validated before they become file paths
- [ ] Cached files are checked against the cluster UID; admin files stay under `.admin/` and are permission-checked on every use
- [ ] No new dependency without a reason
