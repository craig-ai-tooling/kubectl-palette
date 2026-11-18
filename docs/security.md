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
- Never read or write `~/.kube/config`. Per-terminal files live under `~/.kube/palette/`.

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
  reads `KUBECTL_PALETTE_TENANT`. A named entry's cluster files call `token --tenant <name>`.
  A cluster file must not be able to send one tenant's token to a cluster in another
  tenant because the active entry changed.
- Entry names are validated (`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`, not `default`) before
  they become a directory name. `.tenants` is rejected as a project or cluster name.
- A `--server` login calls only unauthenticated endpoints before the browser step. The
  issuer it reads from discovery must be https (or loopback) and its host must be the
  server host, the root domain or a subdomain of the root domain, else the login stops.
- Logout of a named entry removes its whole directory, including the config, and clears
  `<Home>/active` if it pointed there.

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
- [ ] `~/.kube/config` is not read or written
- [ ] New files holding credentials are mode 600
- [ ] No token, client secret or API key reaches a log or error message
- [ ] No TLS verification bypass added
- [ ] Names from the API are validated before they become file paths
- [ ] Cached files are checked against the cluster UID; admin files stay under `.admin/` and are permission-checked on every use
- [ ] No new dependency without a reason
