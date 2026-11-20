package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// EnvNoAutoLogin turns off the browser login that starts when a session has expired.
const EnvNoAutoLogin = "KUBECTL_PALETTE_NO_AUTO_LOGIN"

// autoLoginTimeout bounds how long a kubectl call waits for the browser login.
const autoLoginTimeout = 2 * time.Minute

// canAutoLogin reports whether an expired session may be renewed by opening a browser:
// someone is at a terminal (stderr, since kubectl does not pass stdin to a credential
// plugin and `use` captures stdout), a browser can be opened, and it is not turned off.
func (a *App) canAutoLogin() bool {
	return a.StderrIsTTY && a.OpenBrowser != nil && a.getenv(EnvNoAutoLogin) == "" &&
		!oidc.Headless(runtime.GOOS, a.getenv)
}

// sessionExpired is the message for a session Palette no longer accepts.
func (a *App) sessionExpired() error {
	return fmt.Errorf("Palette session expired (unused for too long): run: %s", a.loginHint())
}

// relogin runs the browser login again for the entry a.Paths points at, after Palette
// rejected its session. rejected is the refresh token the failed call used. It saves only the new session and the saved config's endpoints:
// the index, cluster files and contexts stay as they are. Logins are serialized across
// processes, so several kubectl calls that expire at once open one browser between them.
func (a *App) relogin(ctx context.Context, rejected string) error {
	cfg, err := a.Paths.Load()
	if err != nil {
		return err
	}
	m := a.sessionMgr()
	lk, err := session.LockFile(filepath.Join(a.Paths.Home, "login.lock"))
	if err != nil {
		return fmt.Errorf("lock login: %w", err)
	}
	defer lk.Unlock()
	// Another process may have logged in while this one waited for the lock. Only a new
	// refresh token counts: a cached ID token can still be fresh after Palette rejected the
	// refresh token, which the API commands use.
	if now, err := m.Load(); err != nil {
		return err
	} else if now.RefreshToken != "" && now.RefreshToken != rejected {
		return nil
	}
	hc, err := a.httpFor(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "kubectl-palette: the Palette session for tenant %s expired. Opening a browser to log in again (Ctrl-C to cancel).\n", a.Paths.Entry())
	login := a.Login
	if login == nil {
		login = oidc.Login
	}
	sess, got, err := login(ctx, oidc.LoginOptions{
		Cfg: cfg, HTTP: hc, Ports: a.LoginPorts, Out: a.Stderr,
		OpenBrowser: a.OpenBrowser, Timeout: autoLoginTimeout,
	})
	if err != nil {
		return fmt.Errorf("log in again: %w", err)
	}
	if !sameIssuer(got.Issuer, cfg.Issuer) {
		return errors.New("log in again: the login came back from a different issuer; run: " + a.loginHint())
	}
	// A logout while the browser was open wins. logout deletes the session under the
	// session lock, so check and save under it too. Check the config first as well, so a
	// removed named entry does not get its directory back just for a lock file.
	loggedOut := errors.New("log in again: the tenant was logged out meanwhile; nothing saved")
	if _, err := a.Paths.Load(); err != nil {
		return loggedOut
	}
	slk, err := session.LockFile(a.Paths.SessionLock())
	if err != nil {
		return fmt.Errorf("lock session: %w", err)
	}
	defer slk.Unlock()
	if _, err := a.Paths.Load(); err != nil {
		return loggedOut
	}
	if _, err := m.Load(); errors.Is(err, session.ErrNoSession) {
		return loggedOut
	}
	if err := a.Paths.Save(got); err != nil {
		return err
	}
	if err := m.Save(sess); err != nil {
		return err
	}
	fmt.Fprintln(a.Stderr, "kubectl-palette: logged in again")
	return nil
}

// retryAfterLogin runs cmd, and when it fails because Palette rejected the session and
// someone is at a terminal, logs in again and runs it once more. root is the Paths cmd
// starts from; cmd selects its own tenant entry from there.
func (a *App) retryAfterLogin(ctx context.Context, root config.Paths, cmd func() error) error {
	err := cmd()
	if !errors.Is(err, session.ErrSessionGone) || !a.canAutoLogin() {
		return err
	}
	// cmd left a.Paths on the entry whose session was rejected. A rejected renewal leaves
	// the stored pair as it was, so the stored refresh token is the rejected one.
	s, lerr := a.sessionMgr().Load()
	if lerr == nil {
		lerr = a.relogin(ctx, s.RefreshToken)
	}
	if lerr != nil {
		return fmt.Errorf("%v (%w)", err, lerr)
	}
	a.Paths = root
	return cmd()
}
