package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

func (a *App) getenv(k string) string {
	if a.Getenv == nil {
		return ""
	}
	return a.Getenv(k)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// loginHint is the command that fixes a missing or expired session for the selected entry.
func (a *App) loginHint() string {
	e := a.Paths.Entry()
	if e != config.DefaultEntry {
		return "kubectl palette login --tenant " + e
	}
	// A plain login goes to the active entry, which may be another one.
	if len(a.Paths.Root().Entries()) > 1 {
		return "kubectl palette login --tenant default"
	}
	return "kubectl palette login"
}

// entryPrefix labels output with the entry name for a named entry only, so the default
// entry reads exactly as it did before tenants existed.
func (a *App) entryPrefix() string {
	if e := a.Paths.Entry(); e != config.DefaultEntry {
		return e + ": "
	}
	return ""
}

func unknownEntryErr(name string, entries []string) error {
	if len(entries) == 0 {
		return fmt.Errorf("no tenant entry named %q and none are saved: run: kubectl palette login --server <url> [--tenant <org>]", name)
	}
	return fmt.Errorf("no tenant entry named %q (saved: %s)", name, strings.Join(entries, ", "))
}

// pickEntry applies the selection order: the flag, KUBECTL_PALETTE_TENANT, the active entry,
// the default entry when it exists, then the only entry there is. It reads state and
// changes nothing. A flag or env value that names no entry is an error. An active marker
// that names a removed entry is ignored.
func (a *App) pickEntry(flagVal string) (string, error) {
	root := a.Paths.Root()
	entries := root.Entries()
	if flagVal != "" {
		m, ok := config.MatchEntry(entries, flagVal)
		if !ok {
			return "", unknownEntryErr(flagVal, entries)
		}
		return m, nil
	}
	if env := a.getenv(config.EnvTenant); env != "" {
		m, ok := config.MatchEntry(entries, env)
		if !ok {
			return "", fmt.Errorf("%s: %w", config.EnvTenant, unknownEntryErr(env, entries))
		}
		return m, nil
	}
	if act := root.Active(); act != "" && contains(entries, act) {
		return act, nil
	}
	if contains(entries, config.DefaultEntry) {
		return config.DefaultEntry, nil
	}
	switch len(entries) {
	case 0:
		return "", config.ErrNoConfig
	case 1:
		return entries[0], nil
	}
	return "", fmt.Errorf("no tenant selected (saved: %s): run: kubectl palette tenant <name>, or pass --tenant <name>", strings.Join(entries, ", "))
}

// selectTenant points a.Paths at the chosen entry.
func (a *App) selectTenant(flagVal string) error {
	name, err := a.pickEntry(flagVal)
	if err != nil {
		return err
	}
	a.Paths = a.Paths.Root().ForTenant(name)
	return nil
}

// httpFor returns the HTTP client for an entry. With a saved CA file it trusts that CA on
// top of the system roots. TLS verification stays on.
func (a *App) httpFor(cfg config.Config) (*http.Client, error) {
	return a.httpWithCA(cfg.CAFile)
}

func (a *App) httpWithCA(caFile string) (*http.Client, error) {
	base := a.httpClient()
	if caFile == "" {
		return base, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA file %s holds no PEM certificate", caFile)
	}
	var tr *http.Transport
	switch t := base.Transport.(type) {
	case nil:
		tr = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		tr = t.Clone()
	default:
		// A test transport that is not an *http.Transport has no TLS config to extend.
		return base, nil
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = pool
	c := *base
	c.Transport = tr
	return &c, nil
}

// --- tenants and tenant ---

type entryRow struct {
	name   string
	active bool
	cfg    config.Config
	status string
}

func serverHost(c config.Config) string {
	for _, raw := range []string{c.Server, c.Issuer} {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return "-"
}

// sessionStatus reads the saved refresh token's exp claim without a network call. A token
// that is not a JWT, or has no exp, counts as logged in.
func (a *App) sessionStatus(p config.Paths) string {
	m := &session.Manager{File: p.SessionFile(), LockPath: p.SessionLock()}
	s, err := m.Load()
	if err != nil {
		return "logged out"
	}
	if s.RefreshToken == "" {
		return "logged out"
	}
	if exp, err := session.Expiry(s.RefreshToken); err == nil && !exp.After(a.now()) {
		return "expired"
	}
	return "logged in"
}

func (a *App) tenants(ctx context.Context, args []string) error {
	fs := newFlags("tenants", a.Stderr)
	avail := fs.Bool("available", false, "also list organizations this account belongs to that have no saved entry")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	}
	root := a.Paths.Root()
	entries := root.Entries()
	active, _ := a.pickEntry("")
	if len(entries) == 0 {
		fmt.Fprintln(a.Stdout, "no tenants saved: run: kubectl palette login --server <url> [--tenant <org>]")
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	var rows []entryRow
	for _, n := range entries {
		p := root.ForTenant(n)
		cfg, _ := p.Load()
		rows = append(rows, entryRow{name: n, active: n == active, cfg: cfg, status: a.sessionStatus(p)})
	}
	for _, r := range rows {
		mark := " "
		if r.active {
			mark = "*"
		}
		org := r.cfg.Org
		if org == "" {
			org = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", mark, r.name, org, serverHost(r.cfg), r.status)
	}
	tw.Flush()
	if !*avail {
		return nil
	}
	if active == "" {
		return errors.New("--available needs a logged-in tenant: run: kubectl palette login --server <url> [--tenant <org>]")
	}
	a.Paths = root.ForTenant(active)
	cfg, err := a.Paths.Load()
	if err != nil {
		return err
	}
	c, err := a.apiClient()
	if err != nil {
		return fmt.Errorf("list organizations with tenant %s: %w", active, err)
	}
	orgs, err := c.Orgs(ctx)
	if err != nil {
		return fmt.Errorf("list organizations with tenant %s: %w", active, err)
	}
	// An entry hides an org only when it is on the same server: the same org name can exist
	// on another Palette installation.
	have := map[string]bool{}
	for _, r := range rows {
		// Compare the issuer host: entries logged in through different org hosts of one
		// installation share it, while --server keeps the host that was typed.
		if issuerHost(r.cfg.Issuer) == issuerHost(cfg.Issuer) {
			have[r.name], have[r.cfg.Org] = true, true
		}
	}
	server := cfg.Server
	if server == "" {
		if u, err := url.Parse(cfg.Issuer); err == nil && u.Host != "" {
			server = u.Scheme + "://" + u.Host
		}
	}
	var missing []string
	for _, o := range orgs {
		if o.Name != "" && !have[o.Name] {
			missing = append(missing, o.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		fmt.Fprintln(a.Stdout, "\nno other organizations on this account")
		return nil
	}
	fmt.Fprintf(a.Stdout, "\navailable, not logged in (via %s):\n", active)
	for _, n := range missing {
		fmt.Fprintf(a.Stdout, "  %s\tkubectl palette login --server %s --tenant %s\n", n, server, n)
	}
	return nil
}

func (a *App) tenantCmd(args []string) error {
	fs := newFlags("tenant", a.Stderr)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitError(2)
	}
	if len(pos) > 1 {
		return errors.New("usage: kubectl palette tenant [name]")
	}
	root := a.Paths.Root()
	if len(pos) == 0 {
		name, err := a.pickEntry("")
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, name)
		return nil
	}
	entries := root.Entries()
	name, ok := config.MatchEntry(entries, pos[0])
	if !ok {
		return unknownEntryErr(pos[0], entries)
	}
	if err := root.SetActive(name); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "active tenant: %s\n", name)
	return nil
}

// --- logout ---

func (a *App) logout(args []string) error {
	fs := newFlags("logout", a.Stderr)
	tenant := fs.String("tenant", "", "tenant entry to log out of (default: the selected one)")
	all := fs.Bool("all", false, "log out of every tenant")
	keep := fs.Bool("keep-contexts", false, "leave the tenant's contexts in your kubeconfig")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	}
	root := a.Paths.Root()
	var names []string
	if *all {
		if *tenant != "" {
			return errors.New("use either --tenant or --all")
		}
		names = root.Entries()
	} else {
		n, err := a.pickEntry(*tenant)
		if err != nil {
			return err
		}
		names = []string{n}
	}
	failed := false
	removed := 0
	for _, n := range names {
		c, err := a.logoutEntry(root, n, *keep)
		removed += c
		if err != nil {
			failed = true
		}
	}
	if *all && !*keep {
		// Records of an entry that no longer exists go too, also when no entry is saved.
		a.Paths = root
		rest, err := a.removeContexts(func(kubectx.Record) bool { return true })
		if err != nil {
			a.warnf("could not remove contexts: %v", err)
		}
		removed += deletedCount(rest)
		for _, r := range rest {
			if !r.EntriesDeleted {
				fmt.Fprintf(a.Stderr, "left %s in place (you changed it), no longer tracked\n", r.Record.Context)
			}
		}
		if len(names) == 0 && len(rest) == 0 {
			fmt.Fprintln(a.Stderr, "nothing to log out of")
			return nil
		}
	}
	a.Paths = root
	if removed > 0 {
		fmt.Fprintf(a.Stderr, "removed %s from %s\n", plural(removed, "context", "contexts"), shortPath(root.ContextFile()))
	}
	if failed {
		return exitError(1)
	}
	return nil
}

// logoutEntry logs out of one entry. The default entry keeps its config, so the next login
// needs no flags, and loses its session, index and admin files. A named entry is deleted
// whole, and the active marker goes if it pointed there.
func (a *App) logoutEntry(root config.Paths, name string, keepContexts bool) (contexts int, err error) {
	a.Paths = root.ForTenant(name)
	defer func() { a.Paths = root }()
	// A session that merely expires leaves contexts in place. A logout removes them, unless
	// asked not to. A failure here is a warning: it must not block the logout.
	if !keepContexts {
		contexts = a.removeEntryContexts(name)
	}
	var errs []error
	if err := a.sessionMgr().Delete(); err != nil {
		errs = append(errs, err)
	}
	for _, p := range []string{a.Paths.IndexFile(), a.Paths.IndexLock()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	removed, cerrs := a.clearClusterFiles(true)
	errs = append(errs, cerrs...)
	if name != config.DefaultEntry && len(errs) == 0 {
		if err := root.RemoveEntry(name); err != nil {
			errs = append(errs, err)
		} else if root.Active() == name {
			if err := root.ClearActive(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		fmt.Fprintf(a.Stderr, "logout of %s incomplete, these could not be removed:\n", name)
		for _, e := range errs {
			fmt.Fprintf(a.Stderr, "  %v\n", e)
		}
		return contexts, errors.Join(errs...)
	}
	if name == config.DefaultEntry {
		fmt.Fprintf(a.Stderr, "logged out; removed the session, the index and %d admin kubeconfig(s)\n", removed)
	} else {
		fmt.Fprintf(a.Stderr, "logged out of %s; removed its session, index and cluster kubeconfigs\n", name)
	}
	return contexts, nil
}

// absPath makes a user-supplied path absolute for storing in a config file.
func absPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Abs(p)
}
