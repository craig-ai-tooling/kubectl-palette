package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubeconfig"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/palette"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// loginFlags are the parsed flags of `login`.
type loginFlags struct {
	fromKC, issuer, clientID, clientSecret, apiURL string
	importDir, bootstrap                           string
	server, tenant, name, caFile                   string
	noBrowser, noContexts                          bool
}

// legacy reports whether the flags are the original bootstrap form. That form configures
// one entry directly from an issuer and client id, with no discovery.
func (f loginFlags) legacy() bool {
	return f.fromKC != "" || f.issuer != "" || f.clientID != "" || f.bootstrap != "" || f.importDir != ""
}

func (a *App) login(ctx context.Context, args []string) error {
	fs := newFlags("login", a.Stderr)
	var f loginFlags
	fs.StringVar(&f.fromKC, "from-kubeconfig", "", "read issuer, client id and secret from a Palette OIDC kubeconfig")
	fs.StringVar(&f.issuer, "issuer", "", "OIDC issuer URL")
	fs.StringVar(&f.clientID, "client-id", "", "OIDC client id")
	fs.StringVar(&f.clientSecret, "client-secret", "", "OIDC client secret (optional)")
	fs.StringVar(&f.apiURL, "api-url", "", "Palette API URL (default: the issuer's scheme and host)")
	fs.BoolVar(&f.noBrowser, "no-browser", false, "print the login URL without opening a browser")
	fs.BoolVar(&f.noContexts, "no-contexts", false, "do not add the tenant's clusters as contexts in your kubeconfig")
	fs.StringVar(&f.importDir, "import-kubelogin-cache", "", "")
	fs.StringVar(&f.bootstrap, "bootstrap-json", "", "")
	fs.StringVar(&f.server, "server", "", "Palette server URL, for example https://palette.example.com ($"+config.EnvServer+")")
	fs.StringVar(&f.tenant, "tenant", "", "org name or tenant UID to log in to, or a saved tenant entry to log in to again")
	fs.StringVar(&f.name, "name", "", "name of the saved tenant entry (default: the org name)")
	fs.StringVar(&f.caFile, "ca-file", "", "PEM file with an extra CA to trust for this tenant")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	}
	root := a.Paths.Root()
	entries := root.Entries()
	if f.caFile != "" {
		abs, err := absPath(f.caFile)
		if err != nil {
			return err
		}
		f.caFile = abs
	}
	legacy := f.legacy()
	if legacy && (f.server != "" || f.tenant != "") {
		return errors.New("--server and --tenant cannot be combined with --from-kubeconfig, --issuer, --client-id, --bootstrap-json or --import-kubelogin-cache")
	}
	if f.server == "" && !legacy {
		// The environment supplies a server only when there is nothing saved to log in to again.
		if env := a.getenv(config.EnvServer); env != "" && (len(entries) == 0 || (f.tenant != "" && !contains(entries, f.tenant))) {
			f.server = env
		}
	}
	if f.server != "" {
		return a.loginServer(ctx, root, f)
	}

	name := f.name
	switch {
	case legacy:
		if name == "" {
			name = config.DefaultEntry
		}
		if name != config.DefaultEntry {
			if err := config.ValidEntryName(name); err != nil {
				return err
			}
		}
	case f.tenant != "":
		if !contains(entries, f.tenant) {
			return fmt.Errorf("no saved tenant %q (saved: %s). To add it, run: kubectl palette login --server <url> --tenant %s", f.tenant, strings.Join(entries, ", "), f.tenant)
		}
		name = f.tenant
	case name != "":
		if !contains(entries, name) {
			return unknownEntryErr(name, entries)
		}
	default:
		n, err := a.pickEntry("")
		if err != nil {
			return err
		}
		name = n
	}
	a.Paths = root.ForTenant(name)
	return a.loginSaved(ctx, root, f)
}

// loginSaved logs in to the entry a.Paths points at, from its saved config plus whatever
// the flags add. This is the original bootstrap login. A different issuer means a different
// Palette installation, so the entry's old session, index and cluster files are wiped.
func (a *App) loginSaved(ctx context.Context, root config.Paths, f loginFlags) error {
	cfg, err := a.Paths.Load()
	if err != nil && !errors.Is(err, config.ErrNoConfig) {
		return err
	}
	oldIssuer := cfg.Issuer
	apiExplicit := f.apiURL != ""
	if f.bootstrap != "" {
		b, err := os.ReadFile(f.bootstrap)
		if err != nil {
			return err
		}
		var raw map[string]string
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", f.bootstrap, err)
		}
		cfg.Issuer, cfg.ClientID, cfg.ClientSecret, cfg.APIURL = raw["issuer"], raw["client_id"], raw["client_secret"], raw["api"]
		apiExplicit = apiExplicit || raw["api"] != ""
	}
	if f.fromKC != "" {
		b, err := os.ReadFile(f.fromKC)
		if err != nil {
			return err
		}
		o, err := kubeconfig.ParseOIDC(b)
		if err != nil {
			return fmt.Errorf("%s: %w", f.fromKC, err)
		}
		cfg.Issuer, cfg.ClientID, cfg.ClientSecret = o.Issuer, o.ClientID, o.ClientSecret
	}
	for _, o := range []struct {
		dst *string
		v   string
	}{
		{&cfg.Issuer, f.issuer}, {&cfg.ClientID, f.clientID}, {&cfg.ClientSecret, f.clientSecret},
		{&cfg.APIURL, f.apiURL}, {&cfg.CAFile, f.caFile},
	} {
		if o.v != "" {
			*o.dst = o.v
		}
	}
	// A different issuer is a different Palette installation. Keeping the saved API URL would
	// send the new refresh token to the old one, so derive it again.
	issuerChanged := oldIssuer != "" && cfg.Issuer != oldIssuer
	if issuerChanged {
		cfg.Org, cfg.Server = "", ""
		if !apiExplicit {
			cfg.APIURL = deriveAPIURL(cfg.Issuer)
		}
	}
	if cfg.APIURL == "" {
		cfg.APIURL = deriveAPIURL(cfg.Issuer)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	hc, err := a.httpFor(cfg)
	if err != nil {
		return err
	}

	m := a.sessionMgr()
	var sess session.Session
	if f.importDir != "" {
		sess, err = importKubeloginCache(f.importDir)
		if err != nil {
			return err
		}
		_, ep, err := oidc.Discover(ctx, cfg.Issuer, hc)
		if err != nil {
			return err
		}
		cfg.AuthURL, cfg.TokenURL = ep.AuthURL, ep.TokenURL
	} else {
		sess, cfg, err = a.browserLogin(ctx, cfg, hc, f.noBrowser)
		if err != nil {
			return err
		}
	}
	if issuerChanged {
		if err := a.wipeState(m); err != nil {
			return fmt.Errorf("issuer changed from %s to %s and the old state could not be removed, nothing was saved: %w", oldIssuer, cfg.Issuer, err)
		}
		n := a.removeEntryContexts(a.Paths.Entry())
		fmt.Fprintf(a.Stderr, "issuer changed: removed the old session, index, cached kubeconfigs and %s\n", plural(n, "context", "contexts"))
	}
	return a.finishLogin(ctx, root, cfg, sess, f.noContexts)
}

// sshHint is printed on a machine with no browser. 8000 is the first default login port.
const sshHint = "No browser here? From your own machine, forward the login port, then open the URL there: ssh -L 8000:localhost:8000 <this host> (use the port shown in the URL's redirect_uri)."

func (a *App) browserLogin(ctx context.Context, cfg config.Config, hc *http.Client, noBrowser bool) (session.Session, config.Config, error) {
	headless := noBrowser || oidc.Headless(runtime.GOOS, a.getenv)
	o := oidc.LoginOptions{Cfg: cfg, HTTP: hc, Ports: a.LoginPorts, Out: a.Stderr}
	if !headless {
		o.OpenBrowser = a.OpenBrowser
	} else {
		o.Hint = sshHint
	}
	// Paste-back works wherever there is a terminal, also next to an opened browser.
	if a.StdinIsTTY && a.Stdin != nil {
		o.In = a.Stdin
	}
	login := a.Login
	if login == nil {
		login = oidc.Login
	}
	return login(ctx, o)
}

// finishLogin saves the entry a.Paths points at, makes it active and builds its index.
func (a *App) finishLogin(ctx context.Context, root config.Paths, cfg config.Config, sess session.Session, noContexts bool) error {
	if err := a.Paths.Save(cfg); err != nil {
		return err
	}
	if err := a.sessionMgr().Save(sess); err != nil {
		return err
	}
	if err := root.SetActive(a.Paths.Entry()); err != nil {
		return err
	}
	start := a.now()
	idx, err := a.buildIndex(ctx)
	if err != nil {
		return fmt.Errorf("logged in, but building the cluster index failed (run: kubectl palette sync): %w", err)
	}
	fmt.Fprintf(a.Stderr, "logged in; indexed %d clusters in %.1fs\n", len(idx.Clusters), a.now().Sub(start).Seconds())
	fmt.Fprintf(a.Stderr, "active tenant: %s\n", a.Paths.Entry())
	a.autoContexts(ctx, idx, noContexts)
	if len(idx.Clusters) == 1 {
		c := idx.Clusters[0]
		path, fetched, err := a.ensureKubeconfig(ctx, c, false, false)
		if err != nil {
			fmt.Fprintf(a.Stderr, "one cluster found (%s) but its kubeconfig could not be prepared: %v\n", c.Ref(), err)
			return nil
		}
		if !noContexts {
			a.recordContext(c, path, fetched)
		}
		fmt.Fprintf(a.Stderr, "one cluster found and selected: %s. Run: kubectl palette use\n", c.Ref())
	}
	return nil
}

// loginServer resolves a server URL and tenant to an issuer, then logs in. It never wipes
// another entry: a name that already holds a different issuer is an error.
func (a *App) loginServer(ctx context.Context, root config.Paths, f loginFlags) error {
	hc, err := a.httpWithCA(f.caFile)
	if err != nil {
		return err
	}
	res, err := palette.Resolve(ctx, hc, f.server, f.tenant)
	if err != nil {
		return err
	}
	cfg := config.Config{
		Issuer: res.Issuer, ClientID: res.UID, ClientSecret: f.clientSecret, APIURL: res.APIURL,
		Org: res.Org, Server: res.Server, CAFile: f.caFile,
	}
	if f.apiURL != "" {
		cfg.APIURL = f.apiURL
	}
	name := f.name
	if name == "" {
		name = res.Org
	}
	if name != "" {
		if err := checkEntry(root, name, cfg.Issuer); err != nil {
			return err
		}
	}
	sess, cfg, err := a.browserLogin(ctx, cfg, hc, f.noBrowser)
	if err != nil {
		return err
	}
	if name == "" {
		// Only the UID was given. The session knows which org it belongs to.
		name = config.DefaultEntry
		c := &palette.Client{BaseURL: cfg.APIURL, HTTP: hc, Token: func() (string, error) { return sess.RefreshToken, nil }}
		if me, err := c.Me(ctx); err == nil && me.OrgName != "" {
			name, cfg.Org = me.OrgName, me.OrgName
		}
		if err := checkEntry(root, name, cfg.Issuer); err != nil {
			return err
		}
	}
	a.Paths = root.ForTenant(name)
	return a.finishLogin(ctx, root, cfg, sess, f.noContexts)
}

// checkEntry validates an entry name and refuses to reuse one that holds another issuer.
func checkEntry(root config.Paths, name, issuer string) error {
	if name != config.DefaultEntry {
		if err := config.ValidEntryName(name); err != nil {
			return fmt.Errorf("%w: pass --name <entry name>", err)
		}
	}
	if !contains(root.Entries(), name) {
		return nil
	}
	old, err := root.ForTenant(name).Load()
	if err != nil {
		return err
	}
	if old.Issuer != issuer {
		return fmt.Errorf("tenant entry %q already holds a different login (%s): pass --name <entry name> to keep both", name, old.Issuer)
	}
	return nil
}

// wipeState removes everything tied to the previous installation: session, index and every
// cached cluster file, OIDC and admin.
func (a *App) wipeState(m *session.Manager) error {
	errs := []error{m.Delete()}
	for _, p := range []string{a.Paths.IndexFile(), a.Paths.IndexLock()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	_, cerrs := a.clearClusterFiles(false)
	return errors.Join(append(errs, cerrs...)...)
}
