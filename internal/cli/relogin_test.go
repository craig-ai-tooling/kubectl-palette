package cli

import (
	"context"
	"errors"
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

// A second kubectl process that waited on the login lock finds the session renewed and
// opens no browser of its own.
func TestReloginSkipsBrowserWhenSessionIsFresh(t *testing.T) {
	h, calls := expiredHarness(t)
	exp := time.Now().Add(time.Hour)
	if err := h.a.sessionMgr().Save(session.Session{IDToken: jwtExp(exp, "other"), RefreshToken: "rt-0", IDExpiry: exp}); err != nil {
		t.Fatal(err)
	}
	if err := h.a.relogin(context.Background()); err != nil || len(*calls) != 0 {
		t.Fatalf("%v logins %d", err, len(*calls))
	}
}
