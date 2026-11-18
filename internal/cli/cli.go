// Package cli implements the kubectl-palette commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubeconfig"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/palette"
	"github.com/craig-ai-tooling/kubectl-palette/internal/picker"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
	"github.com/craig-ai-tooling/kubectl-palette/internal/shell"
	"golang.org/x/term"
)

// App holds everything a command touches, so tests can swap it.
type App struct {
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	HTTP    *http.Client
	Now     func() time.Time
	Paths   config.Paths

	StdoutIsTTY bool
	StdinIsTTY  bool

	OpenBrowser func(url string) error
	// SpawnSync starts a detached background index refresh for a tenant entry. The entry is
	// always named, "default" included, so the child never falls back to the active one.
	SpawnSync func(tenant string) error
	// Login runs the browser flow; the default is oidc.Login. Tests replace it.
	Login func(ctx context.Context, o oidc.LoginOptions) (session.Session, config.Config, error)
	// RunKubectl runs kubectl with the given extra environment and returns its exit code.
	RunKubectl func(env []string, args []string) int
	// SpawnShell starts an interactive shell with KUBECONFIG set.
	SpawnShell func(kubeconfigPath string) error
	// LoginPorts overrides the loopback ports, for tests.
	LoginPorts []int
	// Pick opens the fuzzy finder. Tests replace it; the default is picker.Pick.
	Pick func(items []picker.Item) (int, error)
	// Exe is the path the plugin was started as (not symlink-resolved) and RealExe the
	// resolved binary. completion install links next to Exe, or to RealExe under --dir.
	Exe, RealExe string
	// LookPath finds an executable on PATH; the default is exec.LookPath.
	LookPath func(string) (string, error)

	envNoted bool // the $KUBECONFIG note was printed
}

const usage = `usage: kubectl palette <command>

commands:
  login        log in through the browser: login --server <url> [--tenant <org>] [--name <entry>] [--ca-file <pem>]
               [--no-browser] [--no-contexts] (again later: login [--tenant <entry>];
               older form: --from-kubeconfig <file>)
  tenants      list the logged-in tenants: tenants [--available]
  tenant       show or set the active tenant: tenant [name]
  use          target a cluster in this terminal: use [name] [--admin] [--host <cluster>] [--no-fuzzy] [--refresh]
               [--no-context] [--tenant <name>]; use --unset goes back to your own kubeconfig
  exec         run kubectl against a cluster once: exec <name> [--admin] [--refresh] [--no-context] [--tenant <name>] -- <args>
  contexts     clusters as contexts in your kubeconfig, for kubectl --context and use-context in any terminal:
               contexts [list] | add <name>... | --all [--host <cluster>] | remove <context>... | --all [--all-tenants] | prune
  current      show the cluster and mode for this terminal
  sync         rebuild the local cluster index: sync [--tenant <name>]
  logout       log out of one tenant or all: logout [--tenant <name>] [--all] [--keep-contexts]
  token        exec-credential plugin used by the kubeconfigs: token [--tenant <name>] [--issuer-id <id>]
  shell-init   print the shell hook: shell-init zsh|bash
  completion   tab completion for kubectl: completion install [--dir <dir>]
  version      print the version

A tenant is chosen by --tenant, then $KUBECTL_PALETTE_TENANT, then the active tenant
(kubectl palette tenant <name>). "token" without --tenant always means the default tenant.
Contexts go in ~/.kube/config. Turn them off with KUBECTL_PALETTE_NO_CONTEXTS=1.
`

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Run executes one command line and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return 2
	}
	var err error
	rest := args[1:]
	// A command selects a tenant entry by pointing a.Paths at it. Put the root back so
	// one App can run several commands, as the tests do.
	root := a.Paths
	defer func() { a.Paths = root }()
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(a.Stdout, a.Version)
	case "login":
		err = a.login(ctx, rest)
	case "token":
		err = a.token(ctx, rest)
	case "tenants":
		err = a.tenants(ctx, rest)
	case "tenant":
		err = a.tenantCmd(rest)
	case "sync":
		err = a.sync(ctx, rest)
	case "use":
		err = a.use(ctx, rest)
	case "exec":
		return a.execCmd(ctx, rest)
	case "contexts":
		err = a.contextsCmd(ctx, rest)
	case "current":
		err = a.current()
	case "logout":
		err = a.logout(rest)
	case "shell-init":
		err = a.shellInit(rest)
	case "completion":
		err = a.completion(rest)
	case "__complete":
		a.complete(rest)
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
	default:
		fmt.Fprintf(a.Stderr, "kubectl-palette: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			return int(ee)
		}
		fmt.Fprintf(a.Stderr, "kubectl-palette %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// parseFlags parses flags that may appear before or after positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlags(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// --- session and API wiring ---

type lazyRefresher struct {
	a *App
}

func (l lazyRefresher) Refresh(ctx context.Context, rt string) (string, string, error) {
	cfg, err := l.a.Paths.Load()
	if err != nil {
		return "", "", err
	}
	hc, err := l.a.httpFor(cfg)
	if err != nil {
		return "", "", err
	}
	return oidc.Refresher{Cfg: cfg, HTTP: hc}.Refresh(ctx, rt)
}

func (a *App) sessionMgr() *session.Manager {
	return &session.Manager{
		File: a.Paths.SessionFile(), LockPath: a.Paths.SessionLock(),
		Refresher: lazyRefresher{a}, Now: a.now,
	}
}

func (a *App) apiClient() (*palette.Client, error) {
	cfg, err := a.Paths.Load()
	if err != nil {
		return nil, err
	}
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("no API URL saved: run: %s", a.loginHint())
	}
	hc, err := a.httpFor(cfg)
	if err != nil {
		return nil, err
	}
	m := a.sessionMgr()
	if _, err := m.Load(); err != nil {
		if errors.Is(err, session.ErrNoSession) {
			return nil, fmt.Errorf("no valid session: run: %s", a.loginHint())
		}
		return nil, err
	}
	return &palette.Client{BaseURL: cfg.APIURL, HTTP: hc, Token: m.APIToken, Renew: m.RefreshAfterReject}, nil
}

// --- token ---

type execCredential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		Token               string `json:"token"`
		ExpirationTimestamp string `json:"expirationTimestamp"`
	} `json:"status"`
}

// token prints the exec credential. It never reads the active entry or the environment:
// plain "token" is the default entry and "token --tenant x" is exactly x. A cluster file
// spells out which entry it belongs to, so changing the active entry can never send one
// tenant's token to a cluster in another tenant.
func (a *App) token(ctx context.Context, args []string) error {
	fs := newFlags("token", a.Stderr)
	tenant := fs.String("tenant", "", "tenant entry whose token to print (default: the default entry)")
	issuerID := fs.String("issuer-id", "", "id of the Palette login the kubeconfig was made for")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	}
	named := *tenant != "" && *tenant != config.DefaultEntry
	if named {
		if err := config.ValidEntryName(*tenant); err != nil {
			return err
		}
		// No released version wrote a named-entry file without an issuer id.
		if *issuerID == "" {
			return issuerMismatchErr(*tenant)
		}
	}
	a.Paths = a.Paths.ForTenant(*tenant)
	if *issuerID != "" {
		// A missing config falls through to the login hint below.
		if cfg, err := a.Paths.Load(); err == nil && config.IssuerID(cfg.Issuer) != *issuerID {
			return issuerMismatchErr(a.Paths.Entry())
		}
	}
	tok, exp, err := a.sessionMgr().IDToken(ctx)
	if err != nil {
		if errors.Is(err, config.ErrNoConfig) || errors.Is(err, session.ErrNoSession) || errors.Is(err, session.ErrSessionGone) {
			return errors.New("run: " + a.loginHint())
		}
		return err
	}
	var c execCredential
	c.APIVersion, c.Kind = "client.authentication.k8s.io/v1beta1", "ExecCredential"
	c.Status.Token = tok
	c.Status.ExpirationTimestamp = exp.UTC().Format(time.RFC3339)
	return json.NewEncoder(a.Stdout).Encode(c)
}

// issuerMismatchErr is the refusal when a kubeconfig entry was made for another login.
func issuerMismatchErr(entry string) error {
	return fmt.Errorf("this kubeconfig entry was made for a different Palette login than tenant entry %q now holds: run: kubectl palette contexts prune (and kubectl palette use <cluster> --refresh in a terminal pinned with use)", entry)
}

// entryIssuerID is the issuer id of the selected entry, "" when it has no config.
func (a *App) entryIssuerID() string {
	cfg, err := a.Paths.Load()
	if err != nil || cfg.Issuer == "" {
		return ""
	}
	return config.IssuerID(cfg.Issuer)
}

// --- sync and index ---

func (a *App) buildIndex(ctx context.Context) (*index.Index, error) {
	c, err := a.apiClient()
	if err != nil {
		return nil, err
	}
	idx, err := index.Build(ctx, c, a.now())
	if err != nil {
		return nil, err
	}
	if old, lerr := index.Load(a.Paths.IndexFile()); lerr == nil {
		idx.CarryOver(old)
	}
	if err := idx.Save(a.Paths.IndexFile()); err != nil {
		return nil, fmt.Errorf("save index: %w", err)
	}
	return idx, nil
}

func (a *App) sync(ctx context.Context, args []string) error {
	fs := newFlags("sync", a.Stderr)
	bg := fs.Bool("background", false, "internal: quiet refresh started by another command")
	tenant := fs.String("tenant", "", "tenant entry to sync")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	}
	if err := a.selectTenant(*tenant); err != nil {
		return err
	}
	if *bg {
		// One background refresh at a time; a busy lock means one is already running.
		lk, err := session.TryLockFile(a.Paths.IndexLock())
		if err != nil || lk == nil {
			return nil
		}
		defer lk.Unlock()
	}
	start := a.now()
	idx, err := a.buildIndex(ctx)
	if err != nil {
		return err
	}
	if !*bg {
		fmt.Fprintf(a.Stdout, "indexed %d clusters in %.1fs\n", len(idx.Clusters), a.now().Sub(start).Seconds())
	}
	return nil
}

func (a *App) loadOrBuildIndex(ctx context.Context) (idx *index.Index, built bool, err error) {
	idx, err = index.Load(a.Paths.IndexFile())
	if errors.Is(err, index.ErrNoIndex) {
		idx, err = a.buildIndex(ctx)
		return idx, true, err
	}
	return idx, false, err
}

func (a *App) maybeBackgroundSync(idx *index.Index) {
	if a.SpawnSync != nil && idx.Stale(a.now()) {
		_ = a.SpawnSync(a.Paths.Entry())
	}
}

// noVirtualError means --host named a cluster that hosts no virtual cluster.
type noVirtualError struct{ host string }

func (e noVirtualError) Error() string {
	return fmt.Sprintf("no virtual clusters are hosted by %s", e.host)
}

// hostedIndex limits idx to the virtual clusters hosted by host.
func hostedIndex(idx *index.Index, host string) (*index.Index, error) {
	sub, err := idx.HostedBy(host)
	if err != nil {
		return nil, fmt.Errorf("--host %s: %w", host, err)
	}
	if len(sub.Clusters) == 0 {
		return nil, noVirtualError{host}
	}
	return sub, nil
}

// resolve finds a cluster, doing one synchronous refresh and one retry on a miss. A host
// limits the search to the virtual clusters that cluster hosts. The index it returns is
// always the full one.
func (a *App) resolve(ctx context.Context, idx *index.Index, fresh bool, name, host string) (index.Cluster, *index.Index, bool, error) {
	find := func(i *index.Index) (index.Cluster, error) {
		if host != "" {
			sub, err := hostedIndex(i, host)
			if err != nil {
				return index.Cluster{}, err
			}
			i = sub
		}
		return i.Resolve(name)
	}
	cl, err := find(idx)
	var nv noVirtualError
	if (errors.Is(err, index.ErrNotFound) || errors.As(err, &nv)) && !fresh {
		nidx, berr := a.buildIndex(ctx)
		if berr != nil {
			return cl, idx, false, fmt.Errorf("%w (refreshing the index failed: %v)", err, berr)
		}
		cl, err = find(nidx)
		return cl, nidx, true, err
	}
	return cl, idx, fresh, err
}

// --- login: see login.go ---

func deriveAPIURL(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	if strings.HasPrefix(host, "console.") {
		return "https://api." + strings.TrimPrefix(host, "console.")
	}
	return ""
}

// importKubeloginCache reads the one token JSON file kubelogin leaves in its cache dir.
func importKubeloginCache(dir string) (session.Session, error) {
	var s session.Session
	ents, err := os.ReadDir(dir)
	if err != nil {
		return s, err
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return s, err
		}
		var t struct {
			IDToken      string `json:"id_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(b, &t); err != nil || t.IDToken == "" {
			continue
		}
		exp, err := session.Expiry(t.IDToken)
		if err != nil {
			return s, err
		}
		return session.Session{IDToken: t.IDToken, RefreshToken: t.RefreshToken, IDExpiry: exp}, nil
	}
	return s, fmt.Errorf("no kubelogin token file found in %s", dir)
}

// --- use ---

func (a *App) shouldPrintEnv(flagPrint bool) bool { return flagPrint || !a.StdoutIsTTY }

// ensureKubeconfig returns the path of the cluster's kubeconfig, fetching it when absent.
// A cached file is trusted only if its UID sidecar matches the index entry, so a cluster
// rebuilt under the same name, or a file from an older layout, is fetched again. refresh
// forces a fetch. An admin file is never returned once the permission is gone.
func (a *App) ensureKubeconfig(ctx context.Context, c index.Cluster, admin, refresh bool) (path string, fetched bool, err error) {
	path, err = a.Paths.ClusterFile(c.Project, c.Name, admin)
	if err != nil {
		return "", false, err
	}
	if admin && !c.CanAdmin() {
		if rerr := config.RemoveCluster(path); rerr != nil {
			return "", false, fmt.Errorf("you no longer hold %s on %s and the cached admin kubeconfig could not be removed: %w", index.AdminPermission, c.Ref(), rerr)
		}
		return "", false, fmt.Errorf("you do not hold %s on %s", index.AdminPermission, c.Ref())
	}
	if _, serr := os.Stat(path); serr == nil {
		if !refresh && c.UID != "" && config.CachedUID(path) == c.UID {
			return path, false, nil
		}
		// Stale, foreign or legacy: drop it before fetching so a failed fetch cannot
		// leave the wrong cluster's file behind.
		if rerr := config.RemoveCluster(path); rerr != nil {
			return "", false, fmt.Errorf("remove stale kubeconfig for %s: %w", c.Ref(), rerr)
		}
	}
	cl, err := a.apiClient()
	if err != nil {
		return "", false, err
	}
	var raw []byte
	if admin {
		raw, err = cl.AdminKubeconfig(ctx, c.UID, c.ProjectUID)
		if err != nil {
			return "", false, fmt.Errorf("fetch admin kubeconfig for %s: %w", c.Ref(), err)
		}
	} else {
		raw, err = a.fetchOIDC(ctx, cl, c)
		if errors.Is(err, errNoOIDC) {
			a.recordOIDC(c.UID, true)
			msg := fmt.Sprintf("%s has no OIDC configured, so there is no OIDC kubeconfig for it.", c.Ref())
			if c.CanAdmin() {
				msg += fmt.Sprintf(" You can use the admin kubeconfig: kubectl palette use %s --admin", c.Ref())
			}
			return "", false, errors.New(msg)
		}
		if err != nil {
			return "", false, err
		}
		if c.NoOIDC {
			a.recordOIDC(c.UID, false)
		}
	}
	if err := config.WriteFileAtomic(path, raw); err != nil {
		return "", false, err
	}
	if err := config.WriteCachedUID(path, c.UID); err != nil {
		_ = config.RemoveCluster(path)
		return "", false, err
	}
	return path, true, nil
}

// errNoOIDC means Palette has no OIDC kubeconfig for the cluster.
var errNoOIDC = errors.New("no OIDC configured")

// errOtherIssuer means a cluster's OIDC setup names an identity provider other than the
// Palette tenant this entry is logged in to.
var errOtherIssuer = errors.New("this cluster uses a different identity provider than your Palette login, which this version does not support")

// sameIssuer compares two issuer URLs, ignoring a trailing slash.
func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// issuerHost is the host of an issuer URL, for messages. The path can hold a tenant UID.
func issuerHost(issuer string) string {
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		return u.Host
	}
	return "unknown issuer"
}

// fetchOIDC downloads a cluster's OIDC kubeconfig and rewrites it so it holds no secret.
// It writes nothing and is safe to call from several goroutines.
func (a *App) fetchOIDC(ctx context.Context, cl *palette.Client, c index.Cluster) ([]byte, error) {
	raw, err := cl.KubeconfigClient(ctx, c.UID, c.ProjectUID)
	if errors.Is(err, palette.ErrNotFound) {
		return nil, errNoOIDC
	}
	if err != nil {
		return nil, fmt.Errorf("fetch kubeconfig for %s: %w", c.Ref(), err)
	}
	cfg, err := a.Paths.Load()
	if err != nil {
		return nil, err
	}
	o, perr := kubeconfig.ParseOIDC(raw)
	// The token this plugin hands out comes from the Palette login. A cluster that trusts
	// another identity provider would reject it, so never point such a cluster at it.
	if perr == nil && !sameIssuer(o.Issuer, cfg.Issuer) {
		return nil, fmt.Errorf("%s: %w (%s). Use the kubeconfig from Palette for it", c.Ref(), errOtherIssuer, issuerHost(o.Issuer))
	}
	raw, err = kubeconfig.Rewrite(raw, kubeconfig.RewriteOptions{Tenant: a.Paths.Entry(), IssuerID: config.IssuerID(cfg.Issuer), Secrets: []string{cfg.ClientSecret, o.ClientSecret}})
	if err != nil {
		return nil, fmt.Errorf("rewrite kubeconfig for %s: %w", c.Ref(), err)
	}
	return raw, nil
}

// recordOIDC remembers in the index whether a cluster has OIDC. A failed write is not
// worth failing the command for: the flag is only a hint for the picker.
func (a *App) recordOIDC(uid string, none bool) {
	idx, err := index.Load(a.Paths.IndexFile())
	if err != nil || !idx.SetNoOIDC(uid, none) {
		return
	}
	_ = idx.Save(a.Paths.IndexFile())
}

func (a *App) use(ctx context.Context, args []string) error {
	fs := newFlags("use", a.Stderr)
	admin := fs.Bool("admin", false, "use the admin kubeconfig (needs the download permission)")
	printEnv := fs.Bool("print-env", false, "print an export line for the shell hook")
	noFuzzy := fs.Bool("no-fuzzy", false, "with no name, use a numbered list instead of the fuzzy finder")
	refresh := fs.Bool("refresh", false, "fetch the kubeconfig again instead of using the cached file")
	tenant := fs.String("tenant", "", "tenant entry to use")
	host := fs.String("host", "", "only the virtual clusters hosted by this cluster")
	noContext := fs.Bool("no-context", false, "do not add this cluster as a context in your kubeconfig")
	unset := fs.Bool("unset", false, "stop using a Palette cluster in this terminal (unset KUBECONFIG)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitError(2)
	}
	if *unset {
		if len(pos) > 0 || *admin || *host != "" {
			return errors.New("usage: kubectl palette use --unset")
		}
		return a.unsetKubeconfig(*printEnv)
	}
	if len(pos) > 1 {
		return errors.New("usage: kubectl palette use [name] [--admin] [--host <cluster>] [--refresh] [--tenant <name>]")
	}
	if err := a.selectTenant(*tenant); err != nil {
		return err
	}
	idx, fresh, err := a.loadOrBuildIndex(ctx)
	if err != nil {
		return err
	}
	var c index.Cluster
	if len(pos) == 1 {
		c, idx, fresh, err = a.resolve(ctx, idx, fresh, pos[0], *host)
		if err != nil {
			return err
		}
	} else {
		scope := idx
		if *host != "" {
			if scope, err = hostedIndex(idx, *host); err != nil {
				return err
			}
		}
		c, err = a.pick(scope, *noFuzzy)
		if err != nil {
			return err
		}
	}
	path, fetched, err := a.ensureKubeconfig(ctx, c, *admin, *refresh)
	if err != nil {
		return err
	}
	mode := "oidc"
	if *admin {
		mode = "admin"
	}
	if fetched {
		fmt.Fprintf(a.Stderr, "fetched kubeconfig for %s\n", c.Ref())
	}
	fmt.Fprintf(a.Stderr, "using %s%s (%s)\n", a.entryPrefix(), c.Ref(), mode)
	if !*admin && !*noContext {
		a.recordContext(c, path, fetched)
	}
	if !fresh {
		a.maybeBackgroundSync(idx)
	}
	if a.shouldPrintEnv(*printEnv) {
		fmt.Fprintln(a.Stdout, shell.ExportKubeconfig(path))
		return nil
	}
	if a.SpawnShell == nil {
		return errors.New("no shell to start; use the shell hook: eval \"$(kubectl-palette shell-init bash)\"")
	}
	fmt.Fprintln(a.Stderr, "no shell hook installed, starting a subshell (exit to leave). See: kubectl palette shell-init")
	return a.SpawnShell(path)
}

// unsetKubeconfig is `use --unset`. The shell hook evals what it prints, so this terminal
// goes back to the user's own kubeconfig. Without the hook there is nothing to eval.
func (a *App) unsetKubeconfig(flagPrint bool) error {
	if a.shouldPrintEnv(flagPrint) {
		fmt.Fprintln(a.Stdout, shell.UnsetKubeconfig)
		fmt.Fprintln(a.Stderr, "this terminal uses your own kubeconfig again")
		return nil
	}
	fmt.Fprintf(a.Stderr, "no shell hook installed, so this cannot change your shell. Run: %s\n(or install the hook: kubectl palette shell-init)\n", shell.UnsetKubeconfig)
	return nil
}

// pick chooses a cluster without a name: the only cluster, the fuzzy finder on a TTY, or
// a numbered list when the finder is off or cannot start.
func (a *App) pick(idx *index.Index, noFuzzy bool) (index.Cluster, error) {
	switch len(idx.Clusters) {
	case 0:
		return index.Cluster{}, errors.New("the index has no clusters: run: kubectl palette sync")
	case 1:
		return idx.Clusters[0], nil
	}
	if !a.StdinIsTTY {
		return index.Cluster{}, errors.New("name a cluster: kubectl palette use <name>")
	}
	if !noFuzzy {
		pk := a.Pick
		if pk == nil {
			pk = picker.Pick
		}
		n, err := pk(pickerItems(idx))
		switch {
		case err == nil:
			return idx.Clusters[n], nil
		case errors.Is(err, picker.ErrAborted):
			return index.Cluster{}, err
		}
		fmt.Fprintf(a.Stderr, "fuzzy finder unavailable (%v), using a numbered list\n", err)
	}
	return a.pickNumbered(idx)
}

// pickerItems labels each cluster project/cluster, then its status words.
func pickerItems(idx *index.Index) []picker.Item {
	items := make([]picker.Item, len(idx.Clusters))
	for i, c := range idx.Clusters {
		label := c.Ref()
		oidc, admin := "oidc ok or not yet known", "no"
		if c.NoOIDC {
			label += "  no-oidc"
			oidc = "none (use --admin)"
		}
		if c.CanAdmin() {
			label += "  admin"
			admin = "yes"
		}
		detail := fmt.Sprintf("project: %s\ncluster: %s\noidc: %s\nadmin kubeconfig: %s", c.Project, c.Name, oidc, admin)
		if c.Virtual() {
			label += "  " + virtualWords(c)
			detail += "\n" + virtualWords(c)
		}
		items[i] = picker.Item{Label: label, Detail: detail}
	}
	return items
}

func (a *App) pickNumbered(idx *index.Index) (index.Cluster, error) {
	for i, c := range idx.Clusters {
		fmt.Fprintf(a.Stderr, "%4d  %s%s\n", i+1, c.Ref(), statusWords(c))
	}
	fmt.Fprint(a.Stderr, "number or name: ")
	var in string
	if _, err := fmt.Fscanln(a.Stdin, &in); err != nil {
		return index.Cluster{}, errors.New("no cluster chosen")
	}
	// The whole input must be a number: "2-prod" is a name, not row 2.
	if n, err := strconv.Atoi(in); err == nil && n >= 1 && n <= len(idx.Clusters) {
		return idx.Clusters[n-1], nil
	}
	return idx.Resolve(in)
}

func statusWords(c index.Cluster) string {
	var s string
	if c.NoOIDC {
		s += "  no-oidc"
	}
	if c.CanAdmin() {
		s += "  admin"
	}
	if c.Virtual() {
		s += "  " + virtualWords(c)
	}
	return s
}

// virtualWords marks a virtual cluster and names its host when the index knows it.
func virtualWords(c index.Cluster) string {
	if c.Host == "" {
		return "virtual"
	}
	return "virtual on " + c.Host
}

// --- exec ---

func (a *App) execCmd(ctx context.Context, args []string) int {
	var before, kargs []string
	split := false
	for i, s := range args {
		if s == "--" {
			before, kargs, split = args[:i], args[i+1:], true
			break
		}
	}
	if !split {
		before = args
	}
	fs := newFlags("exec", a.Stderr)
	admin := fs.Bool("admin", false, "use the admin kubeconfig")
	refresh := fs.Bool("refresh", false, "fetch the kubeconfig again instead of using the cached file")
	tenant := fs.String("tenant", "", "tenant entry to use")
	noContext := fs.Bool("no-context", false, "do not add this cluster as a context in your kubeconfig")
	pos, err := parseFlags(fs, before)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || !split || len(kargs) == 0 {
		fmt.Fprintln(a.Stderr, "usage: kubectl palette exec <name> [--admin] [--refresh] [--no-context] [--tenant <name>] -- <kubectl args>")
		return 2
	}
	run := func() (int, error) {
		if err := a.selectTenant(*tenant); err != nil {
			return 0, err
		}
		idx, fresh, err := a.loadOrBuildIndex(ctx)
		if err != nil {
			return 0, err
		}
		c, idx, fresh, err := a.resolve(ctx, idx, fresh, pos[0], "")
		if err != nil {
			return 0, err
		}
		path, fetched, err := a.ensureKubeconfig(ctx, c, *admin, *refresh)
		if err != nil {
			return 0, err
		}
		if !*admin && !*noContext {
			a.recordContext(c, path, fetched)
		}
		if !fresh {
			a.maybeBackgroundSync(idx)
		}
		return a.RunKubectl([]string{"KUBECONFIG=" + path}, kargs), nil
	}
	code, err := run()
	if err != nil {
		fmt.Fprintf(a.Stderr, "kubectl-palette exec: %v\n", err)
		return 1
	}
	return code
}

// --- current, logout, shell-init ---

// current reads the layout from KUBECONFIG alone, so it needs no saved entry: a flat file
// is <project>/<cluster>.yaml and a named entry's is .tenants/<entry>/<project>/....
func (a *App) current() error {
	kc := a.Getenv("KUBECONFIG")
	kube := a.Paths.Root().KubeDir
	rel, err := filepath.Rel(kube, kc)
	if kc == "" || err != nil || strings.Contains(kc, string(os.PathListSeparator)) || strings.HasPrefix(rel, "..") {
		fmt.Fprintln(a.Stdout, "no Palette cluster selected in this terminal")
		return exitError(1)
	}
	prefix := ""
	if parts := strings.Split(rel, string(filepath.Separator)); parts[0] == config.TenantKubeDir {
		if len(parts) < 4 {
			fmt.Fprintln(a.Stdout, "no Palette cluster selected in this terminal")
			return exitError(1)
		}
		prefix = parts[1] + ": "
		rel = filepath.Join(parts[2:]...)
	}
	dir, file := filepath.Split(rel)
	mode := "oidc"
	name := strings.TrimSuffix(file, ".yaml")
	if filepath.Base(filepath.Clean(dir)) == config.AdminDir {
		mode = "admin"
		dir = filepath.Dir(filepath.Clean(dir)) + string(filepath.Separator)
	}
	fmt.Fprintf(a.Stdout, "%s%s%s (%s)\n", prefix, dir, name, mode)
	return nil
}

// clearClusterFiles removes cached cluster files under the kube directory and reports every
// path it could not read or remove. Admin files always go, including the flat
// <cluster>.admin.yaml files an older layout left in the project directory. With adminOnly
// false the OIDC files and their UID sidecars go too. It never stops at the first error:
// credentials left behind must be named.
func (a *App) clearClusterFiles(adminOnly bool) (admin int, errs []error) {
	projects, err := os.ReadDir(a.Paths.KubeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []error{err}
	}
	for _, pd := range projects {
		if !pd.IsDir() || pd.Name() == config.TenantKubeDir {
			continue // named entries own that directory and are removed by their own logout
		}
		dir := filepath.Join(a.Paths.KubeDir, pd.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			switch {
			case e.IsDir() && e.Name() == config.AdminDir:
				files, rerr := os.ReadDir(p)
				if rerr != nil {
					errs = append(errs, rerr)
					continue
				}
				for _, f := range files {
					if strings.HasSuffix(f.Name(), ".yaml") {
						admin++
					}
				}
				if rerr := os.RemoveAll(p); rerr != nil {
					errs = append(errs, rerr)
				}
			case e.IsDir():
			case strings.HasSuffix(e.Name(), ".admin.yaml") || strings.HasSuffix(e.Name(), ".admin.yaml"+config.UIDSuffix):
				if rerr := os.Remove(p); rerr != nil {
					errs = append(errs, rerr)
				} else if strings.HasSuffix(e.Name(), ".yaml") {
					admin++
				}
			case !adminOnly && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yaml"+config.UIDSuffix)):
				if rerr := os.Remove(p); rerr != nil {
					errs = append(errs, rerr)
				}
			}
		}
	}
	return admin, errs
}

func (a *App) shellInit(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kubectl palette shell-init zsh|bash")
	}
	s, err := shell.Init(args[0])
	if err != nil {
		return err
	}
	fmt.Fprint(a.Stdout, s)
	if !a.completerInstalled() {
		// stderr, because stdout is eval'd by the shell.
		fmt.Fprintln(a.Stderr, "kubectl-palette: tab completion is off. Run: kubectl palette completion install")
	}
	return nil
}

// --- real-world wiring ---

// OpenBrowserDefault opens a URL with the platform opener.
func OpenBrowserDefault(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// RunKubectlDefault runs the real kubectl with stdio attached.
func RunKubectlDefault(env []string, args []string) int {
	bin := os.Getenv("KUBECTL")
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "kubectl-palette exec: run %s: %v\n", bin, err)
		return 127
	}
	return 0
}

// SpawnShellDefault starts the user's shell with KUBECONFIG set.
func SpawnShellDefault(kubeconfigPath string) error {
	cmd := exec.Command(shellFor(runtime.GOOS, os.Getenv))
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// shellFor picks the interactive shell. $SHELL wins everywhere (Git Bash sets it on
// Windows). Without it Windows has no /bin/sh, so fall back to COMSPEC, then PowerShell.
func shellFor(goos string, getenv func(string) string) string {
	if sh := getenv("SHELL"); sh != "" {
		return sh
	}
	if goos == "windows" {
		if cs := getenv("COMSPEC"); cs != "" {
			return cs
		}
		return "powershell.exe"
	}
	return "/bin/sh"
}

// IsTTY reports whether f is a terminal. A character-device check is not enough: it also
// accepts /dev/null.
func IsTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}
