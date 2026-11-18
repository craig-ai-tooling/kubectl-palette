package palette

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	uidAcme   = "0123456789abcdef01234567"
	uidGlobex = "fedcba987654321001234567"
)

func state(tenant, typ string) string {
	b, _ := json.Marshal(map[string]string{"actor": "x", "tenant": tenant, "type": typ, "token": ""})
	return base64.StdEncoding.EncodeToString(b)
}

func redirect(st string) string {
	return "https://idp.example.com/auth?client_id=c&state=" + strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(st)
}

func TestUIDFromRedirect(t *testing.T) {
	std := state(uidAcme, "SystemSso")
	// A payload whose standard encoding has "+" and "/" so the URL-safe form differs.
	payload := []byte(`{"tenant":"` + uidAcme + `","pad":"~~~???>>>"}`)
	for _, tc := range []struct {
		name string
		uri  string
		want string
	}{
		{"system sso", redirect(std), uidAcme},
		{"tenant sso holds org name", redirect(state("acme", "TenantSso")), ""},
		{"sysadmin on root host", redirect(state("sysadmin", "SystemSso")), ""},
		{"uppercase hex rejected", redirect(state(strings.ToUpper(uidAcme), "SystemSso")), ""},
		{"too short", redirect(state("0123abcd", "SystemSso")), ""},
		{"not base64", "https://x/auth?state=%25%25%25", ""},
		{"base64 of non-json", "https://x/auth?state=" + base64.StdEncoding.EncodeToString([]byte("hello")), ""},
		{"no state", "https://x/auth?client_id=c", ""},
		{"not a url", "::::", ""},
		{"url-safe base64", "https://x/auth?state=" + base64.URLEncoding.EncodeToString(payload), uidAcme},
		{"std base64 with an unencoded plus", "https://x/auth?state=" + base64.StdEncoding.EncodeToString(payload), uidAcme},
		{"unpadded std base64", "https://x/auth?state=" + base64.RawStdEncoding.EncodeToString([]byte(`{"tenant":"`+uidAcme+`"}`)), uidAcme},
		{"unpadded url-safe base64", "https://x/auth?state=" + base64.RawURLEncoding.EncodeToString(payload), uidAcme},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UIDFromRedirect(tc.uri)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q %v, want %q", got, ok, tc.want)
			}
		})
	}
}

func TestNormalizeServer(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"https://palette.example.com", "https://palette.example.com", true},
		{"https://Palette.Example.com:8443/some/path?q=1", "https://palette.example.com:8443", true},
		{"palette.example.com", "https://palette.example.com", true},
		{"http://palette.example.com", "", false},
		{"http://localhost:8080", "http://localhost:8080", true},
		{"http://127.0.0.1:9", "http://127.0.0.1:9", true},
		{"http://[::1]:9", "http://[::1]:9", true},
		{"https://u:p@palette.example.com", "", false},
		{"", "", false},
	} {
		got, err := NormalizeServer(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("%q: got %q %v, want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// fakeSaaS answers by Host header, like a Palette install that serves each org on its own
// host. Tests reach it through a transport that routes every host to the one server.
type fakeSaaS struct {
	orgs    map[string]string // host -> /v1/auth/org JSON
	uids    map[string]bool   // tenant UIDs with a JWKS
	issuers map[string]string // uid -> issuer override
	hits    []string
}

func (f *fakeSaaS) handler(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	f.hits = append(f.hits, host+r.URL.Path)
	p := r.URL.Path
	switch {
	case p == "/v1/auth/org":
		if b, ok := f.orgs[host]; ok {
			fmt.Fprint(w, b)
			return
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"code":"TenantNotFound"}`)
	case strings.HasSuffix(p, "/.well-known/jwks.json"):
		if f.uids[strings.Split(p, "/")[4]] {
			fmt.Fprint(w, `{"keys":[]}`)
			return
		}
		http.NotFound(w, r)
	case strings.HasSuffix(p, "/.well-known/openid-configuration"):
		uid := strings.Split(p, "/")[4]
		iss := f.issuers[uid]
		if iss == "" {
			iss = "https://example.com/v1/oidc/tenant/" + uid
		}
		json.NewEncoder(w).Encode(map[string]string{"issuer": iss})
	default:
		http.NotFound(w, r)
	}
}

func orgJSON(org string, uid string, typ string) string {
	tenant := uid
	if typ == "TenantSso" {
		tenant = org
	}
	if uid == "" {
		return fmt.Sprintf(`{"orgName":%q,"rootDomain":"example.com","ssoLogins":[]}`, org)
	}
	return fmt.Sprintf(`{"orgName":%q,"rootDomain":"example.com","totalTenants":3,"ssoLogins":[{"name":"s","redirectUri":%q}]}`,
		org, redirect(state(tenant, typ)))
}

// routeAll sends every request to srv, whatever the host, and trusts its certificate.
func routeAll(srv *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	addr := srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}}
}

func newSaaS(t *testing.T) (*fakeSaaS, *http.Client) {
	f := &fakeSaaS{
		orgs: map[string]string{
			"example.com":        `{"orgName":"root","rootDomain":"example.com","ssoLogins":[{"name":"s","redirectUri":"` + redirect(state("sysadmin", "SystemSso")) + `"}]}`,
			"acme.example.com":   orgJSON("acme", uidAcme, "SystemSso"),
			"globex.example.com": orgJSON("globex", uidGlobex, "TenantSso"),
			"noid.example.com":   orgJSON("noid", "", ""),
			"bad.example.com":    orgJSON("bad", "ffffffffffffffffffffffff", "SystemSso"),
			"evil.example.com":   orgJSON("evil", "aaaaaaaaaaaaaaaaaaaaaaaa", "SystemSso"),
		},
		uids:    map[string]bool{uidAcme: true, uidGlobex: true, "aaaaaaaaaaaaaaaaaaaaaaaa": true},
		issuers: map[string]string{"aaaaaaaaaaaaaaaaaaaaaaaa": "https://attacker.test/v1/oidc/tenant/aaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, routeAll(srv)
}

func TestResolve(t *testing.T) {
	f, hc := newSaaS(t)
	for _, tc := range []struct {
		name, server, tenant string
		wantUID, wantOrg     string
		wantErr              string
	}{
		{name: "root host without tenant", server: "https://example.com", wantErr: "https://example.com serves more than one tenant: pass --tenant <org name>"},
		{name: "root host with org", server: "https://example.com", tenant: "acme", wantUID: uidAcme, wantOrg: "acme"},
		{name: "org host without tenant", server: "https://acme.example.com", wantUID: uidAcme, wantOrg: "acme"},
		{name: "org host with its own name", server: "https://acme.example.com", tenant: "acme", wantUID: uidAcme, wantOrg: "acme"},
		{name: "org host, another org by name", server: "https://acme.example.com", tenant: "globex", wantErr: "does not publish the tenant UID"},
		{name: "unknown org", server: "https://example.com", tenant: "nope", wantErr: `no tenant named "nope" at https://example.com`},
		{name: "no usable SSO entry", server: "https://example.com", tenant: "noid", wantErr: "--tenant <tenant UID>"},
		{name: "tenant SSO only", server: "https://example.com", tenant: "globex", wantErr: "does not publish the tenant UID"},
		{name: "UID given", server: "https://example.com", tenant: uidAcme, wantUID: uidAcme},
		{name: "UID that does not exist", server: "https://example.com", tenant: "bbbbbbbbbbbbbbbbbbbbbbbb", wantErr: "no tenant with UID"},
		{name: "org whose published UID the JWKS rejects", server: "https://example.com", tenant: "bad", wantErr: "no tenant with UID"},
		{name: "issuer on a foreign host", server: "https://example.com", tenant: "evil", wantErr: "issuer host attacker.test"},
		{name: "plain http refused", server: "http://example.com", tenant: "acme", wantErr: "must use https"},
		{name: "bad org label", server: "https://example.com", tenant: "a/b", wantErr: "invalid tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(context.Background(), hc, tc.server, tc.tenant)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.UID != tc.wantUID || res.Org != tc.wantOrg {
				t.Fatalf("%+v", res)
			}
			if res.Issuer != "https://example.com/v1/oidc/tenant/"+tc.wantUID || res.APIURL != "https://example.com" {
				t.Fatalf("issuer/api: %+v", res)
			}
		})
	}
	// The org lookup went to the org's own host.
	found := false
	for _, h := range f.hits {
		if h == "acme.example.com/v1/auth/org" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no request to the org host: %v", f.hits)
	}
}

func TestResolveLoopbackHTTPAndAPIURLFromIssuer(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth/org":
			fmt.Fprintf(w, `{"orgName":"solo","rootDomain":"127.0.0.1","ssoLogins":[{"redirectUri":%q}]}`, redirect(state(uidAcme, "SystemSso")))
		case strings.HasSuffix(r.URL.Path, "jwks.json"):
			fmt.Fprint(w, "{}")
		default:
			fmt.Fprintf(w, `{"issuer":%q}`, srv.URL+"/v1/oidc/tenant/"+uidAcme)
		}
	}))
	defer srv.Close()
	res, err := Resolve(context.Background(), nil, srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.UID != uidAcme || res.Org != "solo" || res.APIURL != srv.URL || res.Issuer != srv.URL+"/v1/oidc/tenant/"+uidAcme {
		t.Fatalf("%+v", res)
	}
}

func TestMeAndOrgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "rt" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v1/users/me":
			fmt.Fprintf(w, `{"status":{"tenant":{"orgName":"acme","tenantUid":%q}}}`, uidAcme)
		case "/v1/auth/orgs":
			fmt.Fprint(w, `{"organizations":[{"name":"acme","authType":"sso"},{"name":"globex","authType":"password"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: func() (string, error) { return "rt", nil }}
	me, err := c.Me(context.Background())
	if err != nil || me.OrgName != "acme" || me.TenantUID != uidAcme {
		t.Fatalf("%+v %v", me, err)
	}
	orgs, err := c.Orgs(context.Background())
	if err != nil || len(orgs) != 2 || orgs[1].Name != "globex" {
		t.Fatalf("%+v %v", orgs, err)
	}
}
