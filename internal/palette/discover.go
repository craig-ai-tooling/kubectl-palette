package palette

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrTenantNotFound means a Palette host answered 404 for a tenant or organization.
var ErrTenantNotFound = errors.New("tenant not found")

// RootOrg is the orgName a Palette root host reports when it serves more than one tenant.
const RootOrg = "root"

var (
	tenantUIDRE = regexp.MustCompile(`^[0-9a-f]{24}$`)
	orgLabelRE  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	domainRE    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
)

// IsTenantUID reports whether s looks like a tenant UID (24 lowercase hex characters).
func IsTenantUID(s string) bool { return tenantUIDRE.MatchString(s) }

// SSOLogin is one entry of the SSO list a Palette host publishes without authentication.
type SSOLogin struct {
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	RedirectURI string `json:"redirectUri"`
}

// Org is the unauthenticated GET /v1/auth/org reply.
type Org struct {
	AuthType    string     `json:"authType"`
	OrgName     string     `json:"orgName"`
	RedirectURL string     `json:"redirectUrl"`
	RootDomain  string     `json:"rootDomain"`
	SSOLogins   []SSOLogin `json:"ssoLogins"`
}

// IsLoopbackHost reports whether host (no port) is localhost or a loopback address.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NormalizeServer reduces a user-supplied server to scheme://host[:port]. A bare host
// means https. Plain http is accepted only for loopback hosts, which tests use.
func NormalizeServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty server URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid server URL %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(scheme == "http" && IsLoopbackHost(u.Hostname())) {
		return "", fmt.Errorf("server URL %q must use https", raw)
	}
	if u.User != nil {
		return "", errors.New("server URL must not contain credentials")
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

func (o Org) isRoot() bool { return o.OrgName == "" || o.OrgName == RootOrg }

func httpOrDefault(hc *http.Client) *http.Client {
	if hc != nil {
		return hc
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// getStatus does an unauthenticated GET and returns the status and a bounded body.
func getStatus(ctx context.Context, hc *http.Client, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := httpOrDefault(hc).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// FetchOrg reads <base>/v1/auth/org. A 404 is ErrTenantNotFound.
func FetchOrg(ctx context.Context, hc *http.Client, base string) (Org, error) {
	var o Org
	code, b, err := getStatus(ctx, hc, base+"/v1/auth/org")
	if err != nil {
		return o, fmt.Errorf("reach %s: %w", base, err)
	}
	switch {
	case code == http.StatusNotFound:
		return o, fmt.Errorf("%s: %w", base, ErrTenantNotFound)
	case code != http.StatusOK:
		return o, fmt.Errorf("%s/v1/auth/org: HTTP %d (is this a Palette server?)", base, code)
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("%s/v1/auth/org: not a Palette reply (is this a Palette server?): %w", base, err)
	}
	return o, nil
}

// UIDFromRedirect pulls the tenant UID out of an SSO redirect URI. The URI carries a
// base64 JSON "state" parameter whose "tenant" field is the UID for a system SSO login.
// Standard, URL-safe, padded and unpadded base64 are all accepted. Anything that is not a
// 24 character lowercase hex string is rejected: the field also holds org names and
// "sysadmin".
func UIDFromRedirect(redirectURI string) (string, bool) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", false
	}
	// A "+" that was never percent-encoded reads back as a space.
	st := strings.ReplaceAll(u.Query().Get("state"), " ", "+")
	if st == "" {
		return "", false
	}
	var raw []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(st); err == nil {
			raw = b
			break
		}
	}
	if raw == nil {
		return "", false
	}
	var s struct {
		Tenant string `json:"tenant"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || !IsTenantUID(s.Tenant) {
		return "", false
	}
	return s.Tenant, true
}

// TenantUID returns the first tenant UID the org's SSO entries publish.
func (o Org) TenantUID() (string, bool) {
	for _, l := range o.SSOLogins {
		if uid, ok := UIDFromRedirect(l.RedirectURI); ok {
			return uid, true
		}
	}
	return "", false
}

// VerifyTenant confirms a tenant UID exists on base: its JWKS answers 200.
func VerifyTenant(ctx context.Context, hc *http.Client, base, uid string) error {
	code, _, err := getStatus(ctx, hc, base+"/v1/oidc/tenant/"+uid+"/.well-known/jwks.json")
	if err != nil {
		return fmt.Errorf("reach %s: %w", base, err)
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("no tenant with UID %s at %s: %w", uid, base, ErrTenantNotFound)
	}
	return fmt.Errorf("%s: tenant JWKS answered HTTP %d", base, code)
}

// DiscoverIssuer reads the tenant's discovery document and returns its canonical issuer.
func DiscoverIssuer(ctx context.Context, hc *http.Client, base, uid string) (string, error) {
	code, b, err := getStatus(ctx, hc, base+"/v1/oidc/tenant/"+uid+"/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("reach %s: %w", base, err)
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("%s: discovery document answered HTTP %d", base, code)
	}
	var d struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(b, &d); err != nil || d.Issuer == "" {
		return "", fmt.Errorf("%s: discovery document has no issuer", base)
	}
	return d.Issuer, nil
}

// Resolved is what a server and tenant name turn into.
type Resolved struct {
	Server string // normalized --server
	Org    string // org name when known, else ""
	UID    string // tenant UID, also the OIDC client id
	Issuer string // canonical issuer from discovery
	APIURL string // scheme and host of the issuer
}

// hostUnder reports whether host is root or a subdomain of root. Ports are ignored.
func hostUnder(host, root string) bool {
	host, root = strings.ToLower(host), strings.ToLower(root)
	return host == root || strings.HasSuffix(host, "."+root)
}

// Resolve turns a server URL and an optional tenant (org name or UID) into the issuer,
// client id and API URL for a login. It makes only unauthenticated calls.
func Resolve(ctx context.Context, hc *http.Client, server, tenant string) (Resolved, error) {
	base, err := NormalizeServer(server)
	if err != nil {
		return Resolved{}, err
	}
	bu, _ := url.Parse(base)
	res := Resolved{Server: base}
	var uid string
	orgBase := base
	rootDomain := bu.Hostname()

	if IsTenantUID(tenant) {
		uid = tenant
		// The root domain bounds the issuer host check; learn it when the server says.
		if o, err := FetchOrg(ctx, hc, base); err == nil && o.RootDomain != "" && domainRE.MatchString(o.RootDomain) && hostUnder(bu.Hostname(), o.RootDomain) {
			rootDomain = o.RootDomain
		}
	} else {
		if tenant != "" && !orgLabelRE.MatchString(tenant) {
			return res, fmt.Errorf("invalid tenant %q: use an org name or a 24 character tenant UID", tenant)
		}
		root, err := FetchOrg(ctx, hc, base)
		if err != nil {
			if errors.Is(err, ErrTenantNotFound) {
				return res, fmt.Errorf("%s did not answer as a Palette tenant host: check --server", base)
			}
			return res, err
		}
		if root.RootDomain != "" && domainRE.MatchString(root.RootDomain) && hostUnder(bu.Hostname(), root.RootDomain) {
			rootDomain = root.RootDomain
		}
		org := root
		switch {
		case tenant == "":
			if root.isRoot() {
				return res, fmt.Errorf("%s serves more than one tenant: pass --tenant <org name>", base)
			}
			res.Org = root.OrgName
		case root.OrgName == tenant:
			res.Org = tenant
		default:
			// Palette serves each org on <org>.<rootDomain>. Keep the port.
			host := tenant + "." + rootDomain
			if p := bu.Port(); p != "" {
				host = net.JoinHostPort(host, p)
			}
			orgBase = bu.Scheme + "://" + strings.ToLower(host)
			org, err = FetchOrg(ctx, hc, orgBase)
			if err != nil {
				if errors.Is(err, ErrTenantNotFound) {
					return res, fmt.Errorf("no tenant named %q at %s", tenant, base)
				}
				return res, err
			}
			if org.isRoot() {
				return res, fmt.Errorf("no tenant named %q at %s", tenant, base)
			}
			res.Org = tenant
		}
		var ok bool
		uid, ok = org.TenantUID()
		if !ok {
			return res, fmt.Errorf("%s does not publish the tenant UID for %q: pass --tenant <tenant UID> (the last path element of the OIDC issuer URL in an OIDC kubeconfig from any cluster) or --from-kubeconfig <file>", orgBase, res.Org)
		}
	}
	res.UID = uid
	if err := VerifyTenant(ctx, hc, orgBase, uid); err != nil {
		return res, err
	}
	issuer, err := DiscoverIssuer(ctx, hc, orgBase, uid)
	if err != nil {
		return res, err
	}
	iu, err := url.Parse(issuer)
	if err != nil || iu.Hostname() == "" {
		return res, fmt.Errorf("discovery returned an unusable issuer %q", issuer)
	}
	if iu.Scheme != "https" && !(iu.Scheme == "http" && IsLoopbackHost(iu.Hostname())) {
		return res, fmt.Errorf("issuer %s must use https", issuer)
	}
	if !strings.EqualFold(iu.Hostname(), bu.Hostname()) && !hostUnder(iu.Hostname(), rootDomain) {
		return res, fmt.Errorf("issuer host %s is not %s or under %s: refusing to log in there", iu.Hostname(), bu.Hostname(), rootDomain)
	}
	res.Issuer = issuer
	res.APIURL = iu.Scheme + "://" + iu.Host
	return res, nil
}

// Me is the part of GET /v1/users/me the plugin reads.
type Me struct {
	OrgName   string
	TenantUID string
}

// Me returns the caller's tenant.
func (c *Client) Me(ctx context.Context) (Me, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/users/me", "", nil)
	if err != nil {
		return Me{}, err
	}
	var raw struct {
		Status struct {
			Tenant struct {
				OrgName   string `json:"orgName"`
				TenantUID string `json:"tenantUid"`
			} `json:"tenant"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Me{}, fmt.Errorf("parse /v1/users/me: %w", err)
	}
	return Me{OrgName: raw.Status.Tenant.OrgName, TenantUID: raw.Status.Tenant.TenantUID}, nil
}

// OrgSummary is one organization the account belongs to.
type OrgSummary struct {
	Name     string
	AuthType string
}

// Orgs lists the organizations the account belongs to.
func (c *Client) Orgs(ctx context.Context) ([]OrgSummary, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/auth/orgs", "", nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Organizations []struct {
			Name     string `json:"name"`
			AuthType string `json:"authType"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse /v1/auth/orgs: %w", err)
	}
	out := make([]OrgSummary, 0, len(raw.Organizations))
	for _, o := range raw.Organizations {
		out = append(out, OrgSummary{Name: o.Name, AuthType: o.AuthType})
	}
	return out, nil
}
