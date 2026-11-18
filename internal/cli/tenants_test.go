package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

const (
	uidAcme   = "0123456789abcdef01234567"
	uidGlobex = "fedcba987654321001234567"
)

// tenantServer is a single-tenant Palette install on loopback: the public org and OIDC
// endpoints plus the API calls the plugin makes, which accept only the tenant's own token.
func tenantServer(t *testing.T, org, uid string, cluster string) *httptest.Server {
	t.Helper()
	st, _ := json.Marshal(map[string]string{"actor": "a", "tenant": uid, "type": "SystemSso", "token": ""})
	redirect := "https://idp.example.com/auth?state=" + url.QueryEscape(base64.StdEncoding.EncodeToString(st))
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/v1/auth/org":
			fmt.Fprintf(w, `{"orgName":%q,"rootDomain":"127.0.0.1","ssoLogins":[{"redirectUri":%q}]}`, org, redirect)
		case p == "/v1/oidc/tenant/"+uid+"/.well-known/jwks.json":
			fmt.Fprint(w, `{"keys":[]}`)
		case p == "/v1/oidc/tenant/"+uid+"/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL + "/v1/oidc/tenant/" + uid})
		case r.Header.Get("Authorization") != "rt-"+org:
			w.WriteHeader(401)
		case p == "/v1/users/me":
			fmt.Fprintf(w, `{"status":{"tenant":{"orgName":%q,"tenantUid":%q}}}`, org, uid)
		case p == "/v1/auth/orgs":
			fmt.Fprint(w, `{"organizations":[{"name":"acme"},{"name":"globex"},{"name":"initech"}]}`)
		case p == "/v1/dashboard/spectroclusters/metadata":
			fmt.Fprintf(w, `{"items":[{"name":%q,"uid":"u-%s","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]}`, cluster, cluster)
		case p == "/v1/dashboard/projects":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"Default","uid":"p1"}}]}`)
		case strings.HasSuffix(p, "/assets/kubeconfigclient"):
			fmt.Fprint(w, oidcKubeconfigFor(srv.URL+"/v1/oidc/tenant/"+uid))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeLogin stands in for the browser flow: it returns a session whose refresh token is
// "rt-<org>", which the matching tenantServer accepts.
func fakeLogin(orgByClient map[string]string) func(context.Context, oidc.LoginOptions) (session.Session, config.Config, error) {
	return func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		org := orgByClient[o.Cfg.ClientID]
		exp := time.Now().Add(time.Hour)
		cfg := o.Cfg
		cfg.AuthURL, cfg.TokenURL = cfg.Issuer+"/auth", cfg.Issuer+"/token"
		return session.Session{IDToken: jwtExp(exp, org), RefreshToken: "rt-" + org, IDExpiry: exp.UTC()}, cfg, nil
	}
}

func (h *harness) addEntry(t *testing.T, name string, cfg config.Config, rt, idTag string) {
	t.Helper()
	// Each named entry gets its own API token, so the fake can serve cluster files that
	// trust that entry's issuer, as a real tenant does.
	if rt == "rt-0" {
		rt = "rt-0-" + name
	}
	h.f.mu.Lock()
	h.f.issuers[rt] = cfg.Issuer
	h.f.mu.Unlock()
	p := h.a.Paths.Root().ForTenant(name)
	if err := p.Save(cfg); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	m := &session.Manager{File: p.SessionFile(), LockPath: p.SessionLock()}
	if err := m.Save(session.Session{IDToken: jwtExp(exp, idTag), RefreshToken: rt, IDExpiry: exp}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) namedCfg(org string) config.Config {
	return config.Config{Issuer: "https://i/" + org, ClientID: org, APIURL: h.f.srv.URL, TokenURL: h.f.srv.URL + "/token", Org: org, Server: "https://palette.example.com"}
}

func TestPickEntryPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string // "default" and named
		flag    string
		env     string
		active  string
		want    string
		wantErr string
	}{
		{name: "flag beats env and active", entries: []string{"default", "acme", "globex"}, flag: "acme", env: "globex", active: "default", want: "acme"},
		{name: "env beats active", entries: []string{"default", "acme", "globex"}, env: "globex", active: "acme", want: "globex"},
		{name: "active beats default", entries: []string{"default", "acme"}, active: "acme", want: "acme"},
		{name: "default when no active", entries: []string{"default", "acme"}, want: "default"},
		{name: "only named entry", entries: []string{"acme"}, want: "acme"},
		{name: "unknown flag lists the known ones", entries: []string{"default", "acme"}, flag: "nope", wantErr: `no tenant entry named "nope" (saved: default, acme)`},
		{name: "unknown env names the variable", entries: []string{"acme"}, env: "nope", wantErr: "KUBECTL_PALETTE_TENANT"},
		{name: "stale active marker is ignored", entries: []string{"default", "acme"}, active: "gone", want: "default"},
		{name: "nothing saved", wantErr: "not configured: run: kubectl palette login --server <url> [--tenant <org>]"},
		{name: "several named and no default", entries: []string{"acme", "globex"}, wantErr: "no tenant selected (saved: acme, globex)"},
		{name: "path-like flag is just an unknown name", entries: []string{"default"}, flag: "../x", wantErr: "no tenant entry named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := &harness{env: map[string]string{}}
			h.a = &App{Getenv: func(k string) string { return h.env[k] },
				Paths: testPaths(dir)}
			for _, e := range tc.entries {
				if err := h.a.Paths.ForTenant(e).Save(config.Config{Issuer: "i", ClientID: "c", APIURL: "a"}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.active != "" {
				h.a.Paths.SetActive(tc.active)
			}
			h.env[config.EnvTenant] = tc.env
			got, err := h.a.pickEntry(tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestTokenNeverUsesActiveEntryOrEnv(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-acme", "acme")
	root := h.a.Paths
	defSess, _ := (&session.Manager{File: root.SessionFile()}).Load()
	acmeSess, _ := (&session.Manager{File: root.ForTenant("acme").SessionFile()}).Load()
	if defSess.IDToken == acmeSess.IDToken {
		t.Fatal("test setup: sessions must differ")
	}
	if err := root.SetActive("acme"); err != nil {
		t.Fatal(err)
	}
	h.env[config.EnvTenant] = "acme"

	tokenOf := func(args ...string) string {
		code, out, e := h.run(append([]string{"token"}, args...)...)
		if code != 0 {
			t.Fatalf("token %v: %d %s", args, code, e)
		}
		var c execCredential
		if err := json.Unmarshal([]byte(out), &c); err != nil {
			t.Fatal(err)
		}
		return c.Status.Token
	}
	if got := tokenOf(); got != defSess.IDToken {
		t.Fatal("token with no flag used a tenant other than default")
	}
	if got := tokenOf("--tenant", "acme"); got != acmeSess.IDToken {
		t.Fatal("token --tenant acme did not use acme")
	}
	if got := tokenOf("--tenant=default"); got != defSess.IDToken {
		t.Fatal("token --tenant default did not use default")
	}
}

func TestTokenErrorsNameTheFix(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-acme", "acme")
	if err := os.Remove(h.a.Paths.ForTenant("acme").SessionFile()); err != nil {
		t.Fatal(err)
	}
	if code, _, e := h.run("token", "--tenant", "acme"); code == 0 || !strings.Contains(e, "run: kubectl palette login --tenant acme") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := h.run("token", "--tenant", "ghost"); code == 0 || !strings.Contains(e, "run: kubectl palette login --tenant ghost") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := h.run("token", "--tenant", "../evil"); code == 0 || !strings.Contains(e, "invalid tenant entry name") {
		t.Fatalf("%d %s", code, e)
	}
	if err := os.Remove(h.a.Paths.SessionFile()); err != nil {
		t.Fatal(err)
	}
	if code, _, e := h.run("token"); code == 0 || !strings.Contains(e, "run: kubectl palette login") || strings.Contains(e, "--tenant") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestNamedEntryClusterFilesAndExecArgs(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	code, out, e := h.run("use", "alpha", "--tenant", "acme", "--print-env")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	want := filepath.Join(h.a.Paths.KubeDir, ".tenants", "acme", "Default", "alpha.yaml")
	if !strings.Contains(out, want) || !strings.Contains(e, "using acme: Default/alpha (oidc)") {
		t.Fatalf("out %q err %q, want path %s", out, e, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "--tenant") || !strings.Contains(string(b), "acme") || strings.Contains(string(b), "TOPSECRET") {
		t.Fatalf("rewritten file:\n%s", b)
	}
	// The default entry's layout is untouched by the named entry.
	if _, err := os.Stat(filepath.Join(h.a.Paths.KubeDir, "Default")); err == nil {
		t.Fatal("a named entry wrote into the flat layout")
	}
	if _, _, e := h.run("use", "alpha", "--print-env"); !strings.Contains(e, "using Default/alpha (oidc)") {
		t.Fatalf("default entry output changed: %s", e)
	}
	flat := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")
	fb, err := os.ReadFile(flat)
	if err != nil || strings.Contains(string(fb), "--tenant") {
		t.Fatalf("default file must keep plain token args: %v\n%s", err, fb)
	}
	// exec honors the selection too.
	h.run("exec", "alpha", "--tenant", "acme", "--", "get", "ns")
	if h.kenv[len(h.kenv)-1][0] != "KUBECONFIG="+want {
		t.Fatalf("%v", h.kenv)
	}
}

func TestCurrentBothLayouts(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	kube := h.a.Paths.KubeDir
	for _, tc := range []struct{ kc, want string }{
		{filepath.Join(kube, "Default", "alpha.yaml"), "Default/alpha (oidc)\n"},
		{filepath.Join(kube, "Default", ".admin", "alpha.yaml"), "Default/alpha (admin)\n"},
		{filepath.Join(kube, ".tenants", "acme", "Default", "alpha.yaml"), "acme: Default/alpha (oidc)\n"},
		{filepath.Join(kube, ".tenants", "acme", "Default", ".admin", "alpha.yaml"), "acme: Default/alpha (admin)\n"},
	} {
		h.env["KUBECONFIG"] = tc.kc
		if code, out, _ := h.run("current"); code != 0 || out != tc.want {
			t.Errorf("%s: %d %q, want %q", tc.kc, code, out, tc.want)
		}
	}
	h.env["KUBECONFIG"] = filepath.Join(kube, ".tenants", "acme")
	if code, out, _ := h.run("current"); code != 1 || !strings.Contains(out, "no Palette cluster") {
		t.Fatalf("%d %q", code, out)
	}
}

// Two servers, two tenants, both logged in at once. Neither login touches the other.
func TestSideBySideServerLogins(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	acme := tenantServer(t, "acme", uidAcme, "acme-web")
	globex := tenantServer(t, "globex", uidGlobex, "globex-db")
	h.a.Login = fakeLogin(map[string]string{uidAcme: "acme", uidGlobex: "globex"})
	root := h.a.Paths
	defBefore, _ := root.Load()

	if code, _, e := h.run("login", "--server", acme.URL, "--no-browser"); code != 0 {
		t.Fatalf("acme: %d %s", code, e)
	}
	code, _, e := h.run("login", "--server", globex.URL, "--no-browser")
	if code != 0 || !strings.Contains(e, "active tenant: globex") {
		t.Fatalf("globex: %d %s", code, e)
	}
	if got := root.Entries(); strings.Join(got, ",") != "default,acme,globex" {
		t.Fatalf("entries %v", got)
	}
	if root.Active() != "globex" {
		t.Fatalf("active %q", root.Active())
	}
	// Each entry keeps its own config, session and index.
	ac, _ := root.ForTenant("acme").Load()
	if ac.ClientID != uidAcme || ac.ClientSecret != "" || ac.Org != "acme" || ac.Server != acme.URL ||
		ac.Issuer != acme.URL+"/v1/oidc/tenant/"+uidAcme || ac.APIURL != acme.URL {
		t.Fatalf("acme config %+v", ac)
	}
	for _, n := range []string{"acme", "globex"} {
		s, err := (&session.Manager{File: root.ForTenant(n).SessionFile()}).Load()
		if err != nil || s.RefreshToken != "rt-"+n {
			t.Fatalf("%s session %+v %v", n, s, err)
		}
	}
	if defAfter, _ := root.Load(); defAfter != defBefore {
		t.Fatal("the default entry changed")
	}
	if _, _, e := h.run("use", "acme-web", "--tenant", "acme", "--print-env"); !strings.Contains(e, "using acme: Default/acme-web") {
		t.Fatal(e)
	}
	if _, _, e := h.run("use", "globex-db", "--print-env"); !strings.Contains(e, "using globex: Default/globex-db") {
		t.Fatalf("active entry not used: %s", e)
	}
	// Logging in to acme again is in place: same files, nothing wiped.
	acmeFile := filepath.Join(root.KubeDir, ".tenants", "acme", "Default", "acme-web.yaml")
	if code, _, e := h.run("login", "--tenant", "acme"); code != 0 {
		t.Fatalf("relogin: %d %s", code, e)
	}
	if _, err := os.Stat(acmeFile); err != nil {
		t.Fatal("relogin wiped the cluster file")
	}
	if root.Active() != "acme" {
		t.Fatalf("active %q after relogin", root.Active())
	}

	// Logout of one entry leaves the other, and the active marker follows.
	root.SetActive("globex")
	if code, _, e := h.run("logout", "--tenant", "globex"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if _, err := os.Stat(root.ForTenant("globex").Home); err == nil {
		t.Fatal("globex state survived logout")
	}
	if _, err := os.Stat(filepath.Join(root.KubeDir, ".tenants", "globex")); err == nil {
		t.Fatal("globex cluster files survived logout")
	}
	if root.Active() != "" {
		t.Fatalf("active marker %q should be cleared", root.Active())
	}
	if _, err := os.Stat(acmeFile); err != nil {
		t.Fatal("logout of globex removed acme's cluster file")
	}
	if s, err := (&session.Manager{File: root.ForTenant("acme").SessionFile()}).Load(); err != nil || s.RefreshToken != "rt-acme" {
		t.Fatalf("acme session lost: %+v %v", s, err)
	}
	if _, err := os.Stat(root.SessionFile()); err != nil {
		t.Fatal("default session lost")
	}
	// logout --all clears the rest.
	if code, _, e := h.run("logout", "--all"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if got := root.Entries(); strings.Join(got, ",") != "default" {
		t.Fatalf("entries after --all: %v", got)
	}
	if _, err := os.Stat(root.SessionFile()); err == nil {
		t.Fatal("default session survived logout --all")
	}
}

func TestServerLoginNameConflictAndOverride(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	acme := tenantServer(t, "acme", uidAcme, "acme-web")
	other := tenantServer(t, "acme", uidAcme, "acme-other") // another install that also calls its org acme
	h.a.Login = fakeLogin(map[string]string{uidAcme: "acme"})
	if code, _, e := h.run("login", "--server", acme.URL, "--no-browser"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	before, _ := h.a.Paths.ForTenant("acme").Load()
	code, _, e := h.run("login", "--server", other.URL, "--no-browser")
	if code == 0 || !strings.Contains(e, `tenant entry "acme" already holds a different login`) || !strings.Contains(e, "--name") {
		t.Fatalf("%d %s", code, e)
	}
	if after, _ := h.a.Paths.ForTenant("acme").Load(); after != before {
		t.Fatal("a refused login changed the entry")
	}
	if code, _, e := h.run("login", "--server", other.URL, "--name", "acme-two", "--no-browser"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if got := strings.Join(h.a.Paths.Entries(), ","); got != "default,acme,acme-two" {
		t.Fatal(got)
	}
	// An org name that cannot be a directory name asks for --name.
	bad := tenantServer(t, "Bad Org!", uidGlobex, "x")
	h.a.Login = fakeLogin(map[string]string{uidGlobex: "Bad Org!"})
	if code, _, e := h.run("login", "--server", bad.URL, "--no-browser"); code == 0 || !strings.Contains(e, "--name") {
		t.Fatalf("%d %s", code, e)
	}
	// A reserved name is refused.
	if code, _, e := h.run("login", "--server", other.URL, "--name", "default", "--no-browser"); code == 0 {
		t.Fatalf("default accepted as a name for another issuer: %s", e)
	}
}

func TestServerLoginTenantUIDNamesEntryFromSession(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.a.Paths.ForTenant("default") // the harness already saved a default entry
	srv := tenantServer(t, "acme", uidAcme, "acme-web")
	h.a.Login = fakeLogin(map[string]string{uidAcme: "acme"})
	if code, _, e := h.run("login", "--server", srv.URL, "--tenant", uidAcme, "--no-browser"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	got, err := h.a.Paths.ForTenant("acme").Load()
	if err != nil || got.Org != "acme" || got.ClientID != uidAcme {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestServerLoginErrors(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.a.Login = func(context.Context, oidc.LoginOptions) (session.Session, config.Config, error) {
		t.Fatal("browser login must not start when resolution fails")
		return session.Session{}, config.Config{}, nil
	}
	if code, _, e := h.run("login", "--server", "http://palette.example.com", "--tenant", "acme"); code == 0 || !strings.Contains(e, "must use https") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := h.run("login", "--server", "http://127.0.0.1:1", "--from-kubeconfig", "x"); code == 0 || !strings.Contains(e, "cannot be combined") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := h.run("login", "--tenant", "ghost"); code == 0 || !strings.Contains(e, `no saved tenant "ghost"`) || !strings.Contains(e, "--server") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestLoginEnvServer(t *testing.T) {
	srv := tenantServer(t, "acme", uidAcme, "acme-web")
	login := fakeLogin(map[string]string{uidAcme: "acme"})

	// Nothing saved: the environment's server is the default.
	dir := t.TempDir()
	h := &harness{out: &bytes.Buffer{}, errb: &bytes.Buffer{}, env: map[string]string{config.EnvServer: srv.URL}}
	h.a = &App{Stdout: h.out, Stderr: h.errb, Stdin: strings.NewReader(""), Login: login,
		Getenv: func(k string) string { return h.env[k] },
		Paths:  testPaths(dir)}
	if code, _, e := h.run("login", "--no-browser"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if got := strings.Join(h.a.Paths.Entries(), ","); got != "acme" {
		t.Fatalf("entries %q", got)
	}

	// Something saved: a bare login logs in to it again and ignores the environment.
	h2 := newHarness(t, time.Now().Add(time.Hour))
	h2.env[config.EnvServer] = "https://unreachable.invalid"
	h2.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		exp := time.Now().Add(time.Hour)
		return session.Session{IDToken: jwtExp(exp, "x"), RefreshToken: "rt-0", IDExpiry: exp}, o.Cfg, nil
	}
	if code, _, e := h2.run("login", "--no-browser"); code != 0 || !strings.Contains(e, "active tenant: default") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestTenantsListsStatusAndAvailable(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	srv := tenantServer(t, "acme", uidAcme, "acme-web")
	cfg := h.namedCfg("acme")
	cfg.APIURL, cfg.Server = srv.URL, srv.URL
	h.addEntry(t, "acme", cfg, "rt-acme", "acme")
	// A refresh token that is a JWT with a past exp is expired; one that is not a JWT counts as logged in.
	h.addEntry(t, "old", h.namedCfg("old"), jwtExp(time.Now().Add(-time.Hour), "r"), "old")
	h.a.Paths.SetActive("acme")

	code, out, e := h.run("tenants")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	want := map[string]string{"default": "logged in", "acme": "logged in", "old": "expired"}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("%q", out)
	}
	for _, ln := range lines {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(ln), "*"))
		if !strings.HasSuffix(ln, want[f[0]]) {
			t.Errorf("line %q: want status %q", ln, want[f[0]])
		}
	}
	if !strings.HasPrefix(lines[1], "*") || strings.HasPrefix(lines[0], "*") || strings.HasPrefix(lines[2], "*") {
		t.Fatalf("active marker is on the wrong line:\n%s", out)
	}
	if !strings.Contains(lines[1], strings.TrimPrefix(srv.URL, "http://")) {
		t.Fatalf("server host missing: %s", lines[1])
	}

	_, out, e = h.run("tenants", "--available")
	if !strings.Contains(out, "initech") || !strings.Contains(out, "login --server "+srv.URL+" --tenant initech") {
		t.Fatalf("%s\n%s", out, e)
	}
	// acme has an entry, globex does not.
	if strings.Contains(out, "--tenant acme") || !strings.Contains(out, "--tenant globex") {
		t.Fatalf("available list wrong:\n%s", out)
	}
}

func TestTenantCommand(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	if _, out, _ := h.run("tenant"); out != "default\n" {
		t.Fatalf("%q", out)
	}
	if code, _, e := h.run("tenant", "acme"); code != 0 || !strings.Contains(e, "active tenant: acme") {
		t.Fatalf("%d %s", code, e)
	}
	if _, out, _ := h.run("tenant"); out != "acme\n" {
		t.Fatalf("%q", out)
	}
	if code, _, e := h.run("tenant", "ghost"); code == 0 || !strings.Contains(e, "saved: default, acme") {
		t.Fatalf("%d %s", code, e)
	}
	if h.a.Paths.Active() != "acme" {
		t.Fatal("a failed set changed the active entry")
	}
	// sync follows the active entry, and --tenant beats it.
	if code, _, e := h.run("sync"); code != 0 || !strings.Contains(e, "") {
		t.Fatalf("%d %s", code, e)
	}
	if _, err := os.Stat(h.a.Paths.ForTenant("acme").IndexFile()); err != nil {
		t.Fatal("sync did not write the active entry's index")
	}
	if _, err := os.Stat(h.a.Paths.IndexFile()); err == nil {
		t.Fatal("sync wrote the default index while acme was active")
	}
	h.run("sync", "--tenant", "default")
	if _, err := os.Stat(h.a.Paths.IndexFile()); err != nil {
		t.Fatal("sync --tenant default did not write the default index")
	}
}

func TestBackgroundSyncCarriesTheTenant(t *testing.T) {
	h := newHarness(t, time.Now().Add(-time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	var got []string
	h.a.SpawnSync = func(tn string) error { got = append(got, tn); return nil }
	h.run("sync")
	h.run("sync", "--tenant", "acme")
	h.a.Now = func() time.Time { return time.Now().Add(time.Hour) }
	h.run("use", "alpha", "--tenant", "acme", "--print-env")
	h.run("use", "alpha", "--print-env")
	if strings.Join(got, "|") != "acme|" {
		t.Fatalf("spawn tenants %q", got)
	}
}

func TestLegacyFlatStateNeedsNoMigration(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	newIss := newIssuerServer(t)
	// The original bootstrap login still works, writes the flat layout and activates default.
	code, _, e := h.run("login", "--issuer", newIss.URL, "--api-url", h.f.srv.URL, "--import-kubelogin-cache", kubeloginCache(t))
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	root := h.a.Paths
	for _, f := range []string{root.ConfigFile(), root.SessionFile(), root.IndexFile(), root.ActiveFile()} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root.Home, config.TenantsDir)); err == nil {
		t.Error("legacy login created a tenants directory")
	}
	if root.Active() != "default" {
		t.Fatalf("active %q", root.Active())
	}
	h.f.mu.Lock()
	h.f.issuers["rt-0"] = newIss.URL // the clusters trust the issuer just logged in to
	h.f.mu.Unlock()
	if code, out, _ := h.run("use", "alpha", "--print-env"); code != 0 || !strings.Contains(out, filepath.Join(root.KubeDir, "Default", "alpha.yaml")) {
		t.Fatalf("%d %s", code, out)
	}
	// Flat files written before tenants existed are used as they are: a flat config with no
	// active marker selects the default entry.
	os.Remove(root.ActiveFile())
	if n, err := h.a.pickEntry(""); err != nil || n != "default" {
		t.Fatalf("%q %v", n, err)
	}
}

func TestLegacyLoginIntoNamedEntry(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	newIss := newIssuerServer(t)
	code, _, e := h.run("login", "--name", "side", "--issuer", newIss.URL, "--client-id", "c", "--api-url", h.f.srv.URL, "--import-kubelogin-cache", kubeloginCache(t))
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	got, err := h.a.Paths.ForTenant("side").Load()
	if err != nil || got.Issuer != newIss.URL || got.ClientSecret != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if def, _ := h.a.Paths.Load(); def.Issuer != "https://i" {
		t.Fatal("a legacy login to a named entry changed the default entry")
	}
	if h.a.Paths.Active() != "side" {
		t.Fatalf("active %q", h.a.Paths.Active())
	}
}

func TestValidateAllowsEmptySecret(t *testing.T) {
	if err := (config.Config{Issuer: "i", ClientID: "c", APIURL: "a"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// --- --ca-file ---

func writeCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// unrelatedCA writes a self-signed certificate that did not sign anything.
func unrelatedCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "other.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCAFileClientTrustsTestCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()
	a := &App{}
	plain, err := a.httpFor(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Get(srv.URL); err == nil {
		t.Fatal("a private CA must not be trusted without --ca-file")
	}
	withCA, err := a.httpFor(config.Config{CAFile: writeCA(t, srv)})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := withCA.Get(srv.URL)
	if err != nil {
		t.Fatalf("client did not trust the CA: %v", err)
	}
	resp.Body.Close()
	// Verification is still on for everything else.
	wrong, _ := a.httpFor(config.Config{CAFile: unrelatedCA(t)})
	if _, err := wrong.Get(srv.URL); err == nil {
		t.Fatal("the wrong CA must not verify the server")
	}
	if tr, ok := withCA.Transport.(*http.Transport); !ok || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("verification must stay on")
	}
	// A bad file is an error, not a silent fallback.
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("not pem"), 0o600)
	if _, err := a.httpFor(config.Config{CAFile: bad}); err == nil {
		t.Fatal("garbage CA file accepted")
	}
	if _, err := a.httpFor(config.Config{CAFile: bad + ".missing"}); err == nil {
		t.Fatal("missing CA file accepted")
	}
}

func TestServerLoginOverTLSNeedsCAFileAndSavesIt(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, _ := json.Marshal(map[string]string{"tenant": uidAcme, "type": "SystemSso"})
		switch {
		case r.URL.Path == "/v1/auth/org":
			fmt.Fprintf(w, `{"orgName":"acme","rootDomain":"127.0.0.1","ssoLogins":[{"redirectUri":"https://x/a?state=%s"}]}`, url.QueryEscape(base64.StdEncoding.EncodeToString(st)))
		case strings.HasSuffix(r.URL.Path, "jwks.json"):
			fmt.Fprint(w, "{}")
		case strings.HasSuffix(r.URL.Path, "openid-configuration"):
			fmt.Fprintf(w, `{"issuer":%q}`, srv.URL+"/v1/oidc/tenant/"+uidAcme)
		case r.Header.Get("Authorization") != "rt-acme":
			w.WriteHeader(401)
		case r.URL.Path == "/v1/dashboard/spectroclusters/metadata":
			fmt.Fprint(w, `{"items":[]}`)
		case r.URL.Path == "/v1/dashboard/projects":
			fmt.Fprint(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := newHarness(t, time.Now().Add(time.Hour))
	var sawCA bool
	h.a.Login = func(ctx context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		_, err := o.HTTP.Get(o.Cfg.Issuer + "/.well-known/jwks.json") // the login client must trust the CA too
		sawCA = err == nil
		return fakeLogin(map[string]string{uidAcme: "acme"})(ctx, o)
	}
	if code, _, e := h.run("login", "--server", srv.URL, "--no-browser"); code == 0 || !strings.Contains(e, "certificate") {
		t.Fatalf("login without --ca-file should fail on the certificate: %d %s", code, e)
	}
	ca := writeCA(t, srv)
	if code, _, e := h.run("login", "--server", srv.URL, "--ca-file", ca, "--no-browser"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if !sawCA {
		t.Fatal("the browser login did not use the CA")
	}
	cfg, err := h.a.Paths.ForTenant("acme").Load()
	if err != nil || cfg.CAFile != ca {
		t.Fatalf("saved CA %q %v", cfg.CAFile, err)
	}
	// The refresh and API paths build their client from the saved config, so they trust it too.
	c, err := h.a.httpFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get(srv.URL + "/v1/auth/org"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
}

func TestCompleteUsesSelectedEntryIndex(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.run("sync")
	h.f.clusters = `[{"name":"acme-only","uid":"x","annotations":{"projectUid":"p1"}}]`
	h.run("sync", "--tenant", "acme")
	before := h.f.total()
	_, out, _ := h.run("__complete", "use", "")
	if strings.Contains(out, "acme-only") || !strings.Contains(out, "alpha") {
		t.Fatalf("default selection: %q", out)
	}
	_, out, _ = h.run("__complete", "use", "--tenant", "acme", "")
	if out != "acme-only\n:4\n" {
		t.Fatalf("--tenant acme: %q", out)
	}
	h.a.Paths.SetActive("acme")
	_, out, _ = h.run("__complete", "exec", "ac")
	if out != "acme-only\n:4\n" {
		t.Fatalf("active entry: %q", out)
	}
	h.env[config.EnvTenant] = "default"
	_, out, _ = h.run("__complete", "exec", "al")
	if out != "alpha\n:4\n" {
		t.Fatalf("env entry: %q", out)
	}
	_, out, _ = h.run("__complete", "use", "--tenant", "")
	if out != "default\nacme\n:4\n" {
		t.Fatalf("tenant names: %q", out)
	}
	_, out, _ = h.run("__complete", "tenant", "a")
	if out != "acme\n:4\n" {
		t.Fatalf("%q", out)
	}
	_, out, _ = h.run("__complete", "use", "--tenant", "nope", "")
	if out != ":4\n" {
		t.Fatalf("unknown entry must complete nothing: %q", out)
	}
	if h.f.total() != before {
		t.Fatal("completion made a network call")
	}
}

func TestDefaultLogoutLeavesNamedEntries(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.run("use", "alpha", "--tenant", "acme", "--print-env")
	h.run("use", "alpha", "--admin", "--print-env")
	acmeFile := filepath.Join(h.a.Paths.KubeDir, ".tenants", "acme", "Default", "alpha.yaml")
	if code, _, e := h.run("logout", "--tenant", "default"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if _, err := os.Stat(acmeFile); err != nil {
		t.Fatal("logout of the default entry removed a named entry's file")
	}
	if _, err := os.Stat(h.a.Paths.ForTenant("acme").ConfigFile()); err != nil {
		t.Fatal("logout of the default entry removed a named entry's config")
	}
	if _, err := os.Stat(h.a.Paths.SessionFile()); err == nil {
		t.Fatal("default session survived")
	}
	if code, _, e := h.run("logout", "--tenant", "ghost"); code == 0 || !strings.Contains(e, "saved:") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, _ := h.run("logout", "--all", "--tenant", "acme"); code == 0 {
		t.Fatal("--all with --tenant must be refused")
	}
}
