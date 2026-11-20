package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// expiredHarness has a session Palette rejects and a person at a terminal with a browser.
// The fake login counts its calls and hands back a fresh session.
func expiredHarness(t *testing.T) (*harness, *[]oidc.LoginOptions) {
	t.Helper()
	h := newHarness(t, time.Now().Add(-time.Minute))
	h.f.tokenBody = `{"error":"invalid_grant"}`
	h.a.StderrIsTTY = true
	h.a.OpenBrowser = func(string) error { return nil }
	h.env["DISPLAY"] = ":0"
	var calls []oidc.LoginOptions
	h.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		calls = append(calls, o)
		exp := time.Now().Add(time.Hour)
		return session.Session{IDToken: jwtExp(exp, "relogin"), RefreshToken: "rt-0", IDExpiry: exp}, o.Cfg, nil
	}
	return h, &calls
}

func TestTokenExpiredLogsInAgain(t *testing.T) {
	h, calls := expiredHarness(t)
	code, out, e := h.run("token")
	if code != 0 || !strings.Contains(out, `"status"`) {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if len(*calls) != 1 || !strings.Contains(e, "Opening a browser") {
		t.Fatalf("calls %d, stderr %s", len(*calls), e)
	}
	o := (*calls)[0]
	if o.OpenBrowser == nil || o.In != nil || o.Timeout != autoLoginTimeout || o.Cfg.Issuer != "https://i" {
		t.Fatalf("login options: %+v", o)
	}
	if s, _ := h.a.sessionMgr().Load(); s.IDExpiry.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("new session not saved: %+v", s)
	}
	// The next call is warm: no second browser.
	if code, _, _ := h.run("token"); code != 0 || len(*calls) != 1 {
		t.Fatalf("second call: %d, logins %d", code, len(*calls))
	}
}

func TestTokenExpiredWithoutBrowserSaysWhy(t *testing.T) {
	for name, tweak := range map[string]func(h *harness){
		"no terminal":    func(h *harness) { h.a.StderrIsTTY = false },
		"turned off":     func(h *harness) { h.env[EnvNoAutoLogin] = "1" },
		"over ssh":       func(h *harness) { h.env["SSH_CONNECTION"] = "1 2 3 4" },
		"no opener seam": func(h *harness) { h.a.OpenBrowser = nil },
	} {
		t.Run(name, func(t *testing.T) {
			h, calls := expiredHarness(t)
			tweak(h)
			code, out, e := h.run("token")
			if code == 0 || out != "" || len(*calls) != 0 {
				t.Fatalf("%d %q logins %d", code, out, len(*calls))
			}
			if !strings.Contains(e, "Palette session expired") || !strings.Contains(e, "run: kubectl palette login") {
				t.Fatalf("stderr %s", e)
			}
		})
	}
}

// After a logout there is no session at all. That was deliberate, so no browser opens.
func TestTokenAfterLogoutDoesNotOpenBrowser(t *testing.T) {
	h, calls := expiredHarness(t)
	h.a.sessionMgr().Delete()
	code, _, e := h.run("token")
	if code == 0 || len(*calls) != 0 || !strings.Contains(e, "not logged in: run: kubectl palette login") {
		t.Fatalf("%d logins %d %s", code, len(*calls), e)
	}
}

func TestTokenExpiredLoginFails(t *testing.T) {
	h, _ := expiredHarness(t)
	h.a.Login = func(context.Context, oidc.LoginOptions) (session.Session, config.Config, error) {
		return session.Session{}, config.Config{}, errors.New("timed out waiting for the browser")
	}
	code, out, e := h.run("token")
	if code == 0 || out != "" || !strings.Contains(e, "Palette session expired") || !strings.Contains(e, "timed out") {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if s, _ := h.a.sessionMgr().Load(); s.RefreshToken != "rt-0" {
		t.Fatalf("old session lost: %+v", s)
	}
}

func TestTokenExpiredLoginFromOtherIssuerIsRefused(t *testing.T) {
	h, _ := expiredHarness(t)
	h.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		exp := time.Now().Add(time.Hour)
		o.Cfg.Issuer = "https://other"
		return session.Session{IDToken: jwtExp(exp, "x"), RefreshToken: "rt-x", IDExpiry: exp}, o.Cfg, nil
	}
	if code, out, e := h.run("token"); code == 0 || out != "" || !strings.Contains(e, "different issuer") {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if cfg, _ := h.a.Paths.Load(); cfg.Issuer != "https://i" {
		t.Fatalf("config overwritten: %+v", cfg)
	}
}

// The API rejects the session, the renewal is refused, `use` logs in again and finishes.
func TestUseExpiredLogsInAgainAndRetries(t *testing.T) {
	h, calls := expiredHarness(t)
	if err := h.a.sessionMgr().Save(session.Session{IDToken: "x.y.z", RefreshToken: "rt-dead", IDExpiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, out, e := h.run("use", "alpha", "--print-env")
	if code != 0 || h.target(t, out) != "alpha" {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if len(*calls) != 1 {
		t.Fatalf("logins %d", len(*calls))
	}
}

func TestUseExpiredWithoutTerminalFails(t *testing.T) {
	h, calls := expiredHarness(t)
	h.a.StderrIsTTY = false
	if err := h.a.sessionMgr().Save(session.Session{IDToken: "x.y.z", RefreshToken: "rt-dead", IDExpiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, _, e := h.run("use", "alpha", "--print-env")
	if code == 0 || len(*calls) != 0 || !strings.Contains(e, "Palette session expired") {
		t.Fatalf("%d logins %d %s", code, len(*calls), e)
	}
}

// A second kubectl process that waited on the login lock finds a new refresh token and
// opens no browser of its own.
func TestReloginSkipsBrowserWhenAnotherProcessLoggedIn(t *testing.T) {
	h, calls := expiredHarness(t)
	if err := h.a.relogin(context.Background(), "rt-rejected-earlier"); err != nil || len(*calls) != 0 {
		t.Fatalf("%v logins %d", err, len(*calls))
	}
}

// Review finding: Palette can reject the refresh token while the cached ID token is still
// fresh. The API commands use the refresh token, so the browser login must still run.
func TestSyncRejectedWithFreshIDTokenLogsInAgain(t *testing.T) {
	h, calls := expiredHarness(t)
	exp := time.Now().Add(40 * time.Minute)
	if err := h.a.sessionMgr().Save(session.Session{IDToken: jwtExp(exp, "fresh"), RefreshToken: "rt-dead", IDExpiry: exp}); err != nil {
		t.Fatal(err)
	}
	code, _, e := h.run("sync")
	if code != 0 || len(*calls) != 1 {
		t.Fatalf("%d logins %d %s", code, len(*calls), e)
	}
	if s, _ := h.a.sessionMgr().Load(); s.RefreshToken != "rt-0" {
		t.Fatalf("new session not saved: %+v", s)
	}
}

// Review finding: `use` of a cluster missing from the cached index refreshes the index.
// When that refresh hits the expired session, the browser login must still run.
func TestUseIndexMissWithExpiredSessionLogsInAgain(t *testing.T) {
	// Build a cached index with a working session, then expire the session.
	h2, calls2 := expiredHarness(t)
	h2.f.tokenBody = ""
	if code, _, e := h2.run("sync"); code != 0 {
		t.Fatalf("sync %d %s", code, e)
	}
	h2.f.tokenBody = `{"error":"invalid_grant"}`
	if err := h2.a.sessionMgr().Save(session.Session{IDToken: "x.y.z", RefreshToken: "rt-dead", IDExpiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h2.f.mu.Lock()
	h2.f.clusters = `[{"name":"gamma","uid":"ug","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]`
	h2.f.mu.Unlock()
	code, out, e := h2.run("use", "gamma", "--print-env")
	if code != 0 || h2.target(t, out) != "gamma" || len(*calls2) != 1 {
		t.Fatalf("%d logins %d %q %s", code, len(*calls2), out, e)
	}
}

// Review finding: a logout while the browser is open wins. Nothing is saved afterwards.
func TestReloginAfterConcurrentLogoutSavesNothing(t *testing.T) {
	for _, name := range []string{"default", "acme"} {
		t.Run(name, func(t *testing.T) {
			h, _ := expiredHarness(t)
			root := h.a.Paths
			if name != "default" {
				cfg, _ := h.a.Paths.Load()
				h.a.Paths = root.ForTenant(name)
				if err := h.a.Paths.Save(cfg); err != nil {
					t.Fatal(err)
				}
				if err := h.a.sessionMgr().Save(session.Session{IDToken: "x.y.z", RefreshToken: "rt-dead", IDExpiry: time.Now().Add(-time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			entry := h.a.Paths
			h.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
				// The user runs logout in another terminal while the browser is open.
				if _, err := h.a.logoutEntry(root, name, false); err != nil {
					t.Fatal(err)
				}
				h.a.Paths = entry
				exp := time.Now().Add(time.Hour)
				return session.Session{IDToken: jwtExp(exp, "x"), RefreshToken: "rt-new", IDExpiry: exp}, o.Cfg, nil
			}
			stored, _ := h.a.sessionMgr().Load()
			loginRan := false
			inner := h.a.Login
			h.a.Login = func(ctx context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
				loginRan = true
				return inner(ctx, o)
			}
			err := h.a.relogin(context.Background(), stored.RefreshToken)
			if !loginRan {
				t.Fatal("the browser login did not run")
			}
			if err == nil || !strings.Contains(err.Error(), "logged out") {
				t.Fatalf("relogin: %v", err)
			}
			h.a.Paths = entry
			if _, err := h.a.sessionMgr().Load(); !errors.Is(err, session.ErrNoSession) {
				t.Fatalf("session came back after logout: %v", err)
			}
			if name != "default" {
				if _, err := os.Stat(entry.Home); !os.IsNotExist(err) {
					t.Fatalf("named entry directory came back: %v", err)
				}
			}
		})
	}
}

// Review finding: another process can log in after this command's request was rejected
// and before the command returns. The rejected token travels in the error, so the
// replacement is recognised and no second browser opens.
func TestRetryUsesTheTokenTheRequestSawRejected(t *testing.T) {
	h, calls := expiredHarness(t)
	root := h.a.Paths
	runs := 0
	err := h.a.retryAfterLogin(context.Background(), root, func() error {
		runs++
		if runs == 1 {
			// The other terminal finishes its login before this command returns.
			exp := time.Now().Add(time.Hour)
			if err := h.a.sessionMgr().Save(session.Session{IDToken: jwtExp(exp, "other"), RefreshToken: "rt-other", IDExpiry: exp}); err != nil {
				t.Fatal(err)
			}
			return fmt.Errorf("fetch cluster index: %w", &session.GoneError{RefreshToken: "rt-0"})
		}
		return nil
	})
	if err != nil || runs != 2 || len(*calls) != 0 {
		t.Fatalf("err %v runs %d logins %d", err, runs, len(*calls))
	}
}

// Review finding: only a missing file means logout. Other read errors keep their cause.
func TestReloginReportsUnreadableConfig(t *testing.T) {
	h, _ := expiredHarness(t)
	h.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		if err := os.WriteFile(h.a.Paths.ConfigFile(), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(time.Hour)
		return session.Session{IDToken: jwtExp(exp, "x"), RefreshToken: "rt-new", IDExpiry: exp}, o.Cfg, nil
	}
	err := h.a.relogin(context.Background(), "rt-0")
	if err == nil || strings.Contains(err.Error(), "logged out") {
		t.Fatalf("relogin: %v", err)
	}
}
