// Package oidc runs the browser login against the Palette tenant issuer and refreshes
// sessions with the refresh token.
package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// DefaultPorts are the loopback ports the Palette auth endpoint accepted in testing.
var DefaultPorts = []int{8000, 18000}

// Scopes requested at login. The ID token carries groups without any extra scope.
var Scopes = []string{gooidc.ScopeOpenID, "profile", "email"}

// errorBodyTransport turns Palette's "HTTP 200 with an error JSON body" token replies
// into HTTP 400, so the oauth2 library reports them as errors with the server's message.
type errorBodyTransport struct{ base http.RoundTripper }

func (t errorBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var probe struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != "" && probe.AccessToken == "" && probe.IDToken == "" {
		resp.StatusCode = http.StatusBadRequest
		resp.Status = "400 Bad Request"
	}
	return resp, nil
}

// WrapHTTP returns a client that applies the 200-with-error rule on top of hc.
func WrapHTTP(hc *http.Client) *http.Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c := *hc
	c.Transport = errorBodyTransport{base: base}
	return &c
}

// Endpoints are what discovery tells us.
type Endpoints struct {
	AuthURL  string
	TokenURL string
	S256     bool // the issuer advertises PKCE S256
}

// Discover reads the issuer's discovery document.
func Discover(ctx context.Context, issuer string, hc *http.Client) (*gooidc.Provider, Endpoints, error) {
	ctx = gooidc.ClientContext(ctx, WrapHTTP(hc))
	p, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, Endpoints{}, fmt.Errorf("discover %s: %w", issuer, err)
	}
	var claims struct {
		Methods []string `json:"code_challenge_methods_supported"`
	}
	_ = p.Claims(&claims)
	ep := p.Endpoint()
	e := Endpoints{AuthURL: ep.AuthURL, TokenURL: ep.TokenURL}
	for _, m := range claims.Methods {
		if m == "S256" {
			e.S256 = true
		}
	}
	return p, e, nil
}

func oauthConfig(c config.Config, redirect string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: c.AuthURL, TokenURL: c.TokenURL},
		RedirectURL:  redirect,
		Scopes:       Scopes,
	}
}

// Refresher refreshes sessions at the saved token endpoint without discovery.
type Refresher struct {
	Cfg  config.Config
	HTTP *http.Client
}

// Refresh implements session.Refresher.
func (r Refresher) Refresh(ctx context.Context, refreshToken string) (string, string, error) {
	if r.Cfg.TokenURL == "" {
		return "", "", errors.New("no token endpoint saved: run: kubectl palette login")
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, WrapHTTP(r.HTTP))
	oc := oauthConfig(r.Cfg, "")
	tok, err := oc.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken, Expiry: time.Now().Add(-time.Hour)}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode < 500 {
			if re.ErrorCode != "" {
				return "", "", fmt.Errorf("%w (issuer said %q)", session.ErrSessionGone, re.ErrorCode)
			}
			return "", "", session.ErrSessionGone
		}
		return "", "", err
	}
	id, _ := tok.Extra("id_token").(string)
	if id == "" {
		return "", "", errors.New("refresh reply had no id_token")
	}
	return id, tok.RefreshToken, nil
}

// LoginOptions drive the browser flow.
type LoginOptions struct {
	Cfg         config.Config
	HTTP        *http.Client
	Ports       []int
	Out         io.Writer
	OpenBrowser func(url string) error // nil prints only
	Timeout     time.Duration
}

func randomString() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func listen(ports []int) (net.Listener, int, error) {
	if len(ports) == 0 {
		ports = DefaultPorts
	}
	var last error
	for _, p := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return l, l.Addr().(*net.TCPAddr).Port, nil // port 0 asks the OS, used by tests
		}
		last = err
	}
	return nil, 0, fmt.Errorf("no free login port among %v: %w", ports, last)
}

// Login runs the authorization-code flow and returns the new session and the config with
// the discovered endpoints filled in.
func Login(ctx context.Context, o LoginOptions) (session.Session, config.Config, error) {
	cfg := o.Cfg
	hc := WrapHTTP(o.HTTP)
	prov, ep, err := Discover(ctx, cfg.Issuer, o.HTTP)
	if err != nil {
		return session.Session{}, cfg, err
	}
	cfg.AuthURL, cfg.TokenURL = ep.AuthURL, ep.TokenURL

	ln, port, err := listen(o.Ports)
	if err != nil {
		return session.Session{}, cfg, err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://localhost:%d", port)
	oc := oauthConfig(cfg, redirect)

	state, err := randomString()
	if err != nil {
		return session.Session{}, cfg, err
	}
	nonce, err := randomString()
	if err != nil {
		return session.Session{}, cfg, err
	}
	opts := []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("access_type", "offline"), gooidc.Nonce(nonce)}
	var exchange []oauth2.AuthCodeOption
	if ep.S256 {
		v := oauth2.GenerateVerifier()
		opts = append(opts, oauth2.S256ChallengeOption(v))
		exchange = append(exchange, oauth2.VerifierOption(v))
	}
	authURL := oc.AuthCodeURL(state, opts...)

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") == "" && q.Get("code") == "" && q.Get("error") == "" {
			http.NotFound(w, r) // favicon and probes
			return
		}
		fail := func(msg string) {
			http.Error(w, "Login failed. You can close this tab.", http.StatusBadRequest)
			select {
			case resCh <- result{err: errors.New(msg)}:
			default:
			}
		}
		if q.Get("state") != state {
			fail("login reply had the wrong state value")
			return
		}
		if e := q.Get("error"); e != "" {
			fail("issuer returned error " + e)
			return
		}
		if q.Get("code") == "" {
			fail("login reply had no code")
			return
		}
		fmt.Fprint(w, "<html><body>Logged in to Palette. You can close this tab.</body></html>")
		select {
		case resCh <- result{code: q.Get("code")}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	fmt.Fprintf(o.Out, "Open this URL to log in:\n%s\n", authURL)
	if o.OpenBrowser != nil {
		if err := o.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(o.Out, "Could not open a browser (%v). Open the URL yourself.\n", err)
		}
	}

	timeout := o.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	var res result
	select {
	case res = <-resCh:
	case <-time.After(timeout):
		return session.Session{}, cfg, errors.New("login timed out waiting for the browser")
	case <-ctx.Done():
		return session.Session{}, cfg, ctx.Err()
	}
	if res.err != nil {
		return session.Session{}, cfg, res.err
	}

	xctx := context.WithValue(ctx, oauth2.HTTPClient, hc)
	tok, err := oc.Exchange(xctx, res.code, exchange...)
	if err != nil {
		return session.Session{}, cfg, fmt.Errorf("exchange login code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return session.Session{}, cfg, errors.New("login reply had no id_token")
	}
	vctx := gooidc.ClientContext(ctx, hc)
	idt, err := prov.Verifier(&gooidc.Config{ClientID: cfg.ClientID}).Verify(vctx, raw)
	if err != nil {
		return session.Session{}, cfg, fmt.Errorf("verify id_token: %w", err)
	}
	if idt.Nonce != nonce {
		return session.Session{}, cfg, errors.New("id_token nonce does not match the login request")
	}
	if tok.RefreshToken == "" {
		return session.Session{}, cfg, errors.New("issuer returned no refresh token, so the session could not be renewed")
	}
	return session.Session{IDToken: raw, RefreshToken: tok.RefreshToken, IDExpiry: idt.Expiry.UTC()}, cfg, nil
}
