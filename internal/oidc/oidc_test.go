package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// fakeIssuer is a minimal OIDC provider: discovery, JWKS and a token endpoint.
type fakeIssuer struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	s256     bool
	mu       sync.Mutex
	nonce    string
	challeng string
	refresh  string // the refresh token currently valid
	n        int
	// tokenMode: "" normal, "200error" returns {"error":..} with HTTP 200, "500" fails
	tokenMode string
	gotForm   url.Values
}

func newIssuer(t *testing.T) *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, key: key, refresh: "rt-0"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		d := map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if f.s256 {
			d["code_challenge_methods_supported"] = []string{"S256"}
		}
		json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", f.token)
	return f
}

func (f *fakeIssuer) idToken(nonce string, life time.Duration) string {
	sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: &jose.JSONWebKey{Key: f.key, KeyID: "k"}}, (&jose.SignerOptions{}).WithType("JWT"))
	c := map[string]any{"iss": f.srv.URL, "aud": "client-1", "sub": "u", "email": "a@b.c", "exp": time.Now().Add(life).Unix(), "iat": time.Now().Unix()}
	if nonce != "" {
		c["nonce"] = nonce
	}
	s, err := jwt.Signed(sig).Claims(c).Serialize()
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	f.gotForm = r.PostForm
	w.Header().Set("Content-Type", "application/json")
	switch f.tokenMode {
	case "200error":
		fmt.Fprint(w, `{"error":"session not found","error_description":"nope"}`)
		return
	case "500":
		http.Error(w, "boom", 500)
		return
	}
	user, pass, basic := r.BasicAuth()
	id, sec := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if basic {
		id, sec = user, pass
	}
	if id != "client-1" || sec != "secret-1" {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"invalid_client"}`)
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if f.s256 && r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		if r.PostForm.Get("code") != "good-code" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != f.refresh {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
	}
	f.n++
	f.refresh = fmt.Sprintf("rt-%d", f.n)
	n := f.nonce
	if r.PostForm.Get("grant_type") == "refresh_token" {
		n = ""
	}
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at", "token_type": "Bearer", "refresh_token": f.refresh, "expires_in": 3600,
		"id_token": f.idToken(n, time.Hour),
	})
}

func (f *fakeIssuer) cfg() config.Config {
	return config.Config{Issuer: f.srv.URL, ClientID: "client-1", ClientSecret: "secret-1", APIURL: "http://api"}
}

// browser plays the user: it follows the auth URL's redirect with a code.
func (f *fakeIssuer) browser(code, stateOverride string) func(string) error {
	return func(authURL string) error {
		u, _ := url.Parse(authURL)
		q := u.Query()
		f.mu.Lock()
		f.nonce = q.Get("nonce")
		f.mu.Unlock()
		if q.Get("access_type") != "offline" || q.Get("scope") != "openid profile email" {
			f.t.Errorf("auth params: %v", q)
		}
		if f.s256 != (q.Get("code_challenge_method") == "S256") {
			f.t.Errorf("PKCE mismatch: s256=%v params=%v", f.s256, q)
		}
		st := q.Get("state")
		if stateOverride != "" {
			st = stateOverride
		}
		go func() {
			resp, err := http.Get(q.Get("redirect_uri") + "/?code=" + code + "&state=" + url.QueryEscape(st))
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func TestLogin(t *testing.T) {
	for _, tc := range []struct {
		name, code, state string
		s256              bool
		wantErr           string
	}{
		{"no pkce", "good-code", "", false, ""},
		{"pkce when advertised", "good-code", "", true, ""},
		{"wrong state", "good-code", "evil", false, "wrong state"},
		{"bad code", "bad-code", "", false, "exchange login code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssuer(t)
			f.s256 = tc.s256
			var out strings.Builder
			s, cfg, err := Login(context.Background(), LoginOptions{
				Cfg: f.cfg(), Out: &out, Ports: []int{0}, OpenBrowser: f.browser(tc.code, tc.state), Timeout: 10 * time.Second,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.RefreshToken != "rt-1" || s.IDToken == "" || cfg.TokenURL != f.srv.URL+"/token" || cfg.AuthURL == "" {
				t.Fatalf("%+v %+v", s, cfg)
			}
			if exp, _ := session.Expiry(s.IDToken); !exp.Equal(s.IDExpiry) {
				t.Fatalf("expiry %v vs %v", s.IDExpiry, exp)
			}
			if !strings.Contains(out.String(), "/auth?") {
				t.Fatalf("URL not printed: %s", out.String())
			}
		})
	}
}

func TestLoginNonceMismatch(t *testing.T) {
	f := newIssuer(t)
	br := f.browser("good-code", "")
	_, _, err := Login(context.Background(), LoginOptions{Cfg: f.cfg(), Out: io.Discard, Ports: []int{0}, Timeout: 10 * time.Second,
		OpenBrowser: func(u string) error {
			err := br(u)
			f.mu.Lock()
			f.nonce = "attacker-nonce"
			f.mu.Unlock()
			return err
		}})
	if err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginTimeout(t *testing.T) {
	f := newIssuer(t)
	_, _, err := Login(context.Background(), LoginOptions{Cfg: f.cfg(), Out: io.Discard, Ports: []int{0}, Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		rt      string
		wantErr error
		wantMsg string
	}{
		{"rotates", "", "rt-0", nil, ""},
		{"200 with error body", "200error", "rt-0", session.ErrSessionGone, "session not found"},
		{"rejected token", "", "stale", session.ErrSessionGone, "invalid_grant"},
		{"server error is transient", "500", "rt-0", nil, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssuer(t)
			f.tokenMode = tc.mode
			cfg := f.cfg()
			cfg.TokenURL = f.srv.URL + "/token"
			id, rt, err := Refresher{Cfg: cfg}.Refresh(context.Background(), tc.rt)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("err = %v", err)
				}
			case tc.wantMsg != "":
				if err == nil || errors.Is(err, session.ErrSessionGone) {
					t.Fatalf("5xx must stay transient, got %v", err)
				}
			default:
				if err != nil || rt != "rt-1" || id == "" {
					t.Fatalf("id %q rt %q err %v", id, rt, err)
				}
			}
		})
	}
}

func TestRefresherNeedsTokenURL(t *testing.T) {
	if _, _, err := (Refresher{}).Refresh(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}

// pasteLogin runs Login with a pipe as input. grab captures the auth URL, and the test
// then feeds lines through feed.
func pasteLogin(t *testing.T, f *fakeIssuer, script func(authURL string, redirect, state string, feed func(string)), hint string) (session.Session, string, error) {
	t.Helper()
	pr, pw := io.Pipe()
	defer pw.Close()
	var out strings.Builder
	var mu sync.Mutex
	feed := func(s string) {
		if _, err := io.WriteString(pw, s+"\n"); err != nil {
			t.Errorf("feed: %v", err)
		}
	}
	s, _, err := Login(context.Background(), LoginOptions{
		Cfg: f.cfg(), Out: &syncWriter{&mu, &out}, Ports: []int{0}, Timeout: 10 * time.Second, In: pr, Hint: hint,
		OpenBrowser: func(authURL string) error {
			u, _ := url.Parse(authURL)
			q := u.Query()
			f.mu.Lock()
			f.nonce = q.Get("nonce")
			f.mu.Unlock()
			go script(authURL, q.Get("redirect_uri"), q.Get("state"), feed)
			return nil
		},
	})
	pr.Close()
	mu.Lock()
	defer mu.Unlock()
	return s, out.String(), err
}

type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestLoginPaste(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  func(authURL, redirect, state string, feed func(string))
		wantErr string
		wantOut string
	}{
		{"full redirected url", func(_, r, st string, feed func(string)) {
			feed(r + "/?code=good-code&state=" + url.QueryEscape(st))
		}, "", "paste it here"},
		{"query string only", func(_, _, st string, feed func(string)) {
			feed("?code=good-code&state=" + url.QueryEscape(st))
		}, "", ""},
		{"wrong state then correct", func(_, r, st string, feed func(string)) {
			feed(r + "/?code=good-code&state=evil")
			feed(r + "/?code=good-code&state=" + url.QueryEscape(st))
		}, "", "different login attempt"},
		{"garbage then callback", func(_, r, st string, feed func(string)) {
			feed("hello there")
			feed("")
			resp, err := http.Get(r + "/?code=good-code&state=" + url.QueryEscape(st))
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}, "", "did not look like"},
		{"access denied paste", func(_, r, st string, feed func(string)) {
			feed(r + "/?error=access_denied&state=" + url.QueryEscape(st))
		}, "access_denied", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssuer(t)
			s, out, err := pasteLogin(t, f, tc.script, "ssh -L 8000:localhost:8000 jump")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.RefreshToken != "rt-1" || s.IDToken == "" {
				t.Fatalf("%+v", s)
			}
			if !strings.Contains(out, "ssh -L 8000") || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("output missing text %q: %s", tc.wantOut, out)
			}
		})
	}
}

func TestLoginNoPasteHintsWhenInNil(t *testing.T) {
	f := newIssuer(t)
	var out strings.Builder
	_, _, err := Login(context.Background(), LoginOptions{Cfg: f.cfg(), Out: &out, Ports: []int{0}, Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if strings.Contains(out.String(), "paste it here") {
		t.Fatalf("paste instructions printed without In: %s", out.String())
	}
}

func TestHeadless(t *testing.T) {
	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		want bool
	}{
		{"linux desktop X11", "linux", map[string]string{"DISPLAY": ":0"}, false},
		{"linux desktop wayland", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, false},
		{"linux no display", "linux", nil, true},
		{"linux display over ssh", "linux", map[string]string{"DISPLAY": "localhost:10", "SSH_CONNECTION": "a b c d"}, true},
		{"linux ssh tty", "linux", map[string]string{"DISPLAY": ":0", "SSH_TTY": "/dev/pts/1"}, true},
		{"darwin local", "darwin", nil, false},
		{"darwin ssh", "darwin", map[string]string{"SSH_CONNECTION": "x"}, true},
		{"windows local", "windows", nil, false},
		{"windows ssh tty", "windows", map[string]string{"SSH_TTY": "x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Headless(tc.goos, func(k string) string { return tc.env[k] }); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
