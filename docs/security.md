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
- `logout` removes the token cache and every admin kubeconfig.

### 3. Never log secrets
- Never log tokens, client secrets or API keys, at any log level.
- Never print a full HTTP request or response body that could contain one.
- Error messages name the cluster and the step. They do not echo credentials.

### 4. TLS
- TLS verification is never disabled by default.
- There is no `--insecure` flag. Self-hosted Palette with a private CA is handled by
  trusting an explicit CA file.

### 5. API names are untrusted input
Cluster and project names come from the Palette API. Treat them as untrusted when
building file paths.

- Reject names that are empty, `.` or `..`, or that contain `/`, `\` or a NUL byte.
- After joining, confirm the result is still inside the base directory.

```go
func safeJoin(base, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("unsafe name %q: refusing to build a path", name)
	}
	return filepath.Join(base, name), nil
}
```

### 6. Secret hygiene in the repo
- Secrets come from the environment or a secret manager. Never a source file.
- `.env` is gitignored.
- Rotate anything that has ever been committed. Assume it is public forever.
- GitHub secret scanning and push protection are enabled on this repo.

### 7. Dependencies
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
- [ ] No new dependency without a reason
