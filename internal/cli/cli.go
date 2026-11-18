// Package cli implements the kubectl-palette commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubeconfig"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/palette"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
	"github.com/craig-ai-tooling/kubectl-palette/internal/shell"
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
	// SpawnSync starts a detached background index refresh.
	SpawnSync func() error
	// RunKubectl runs kubectl with the given extra environment and returns its exit code.
	RunKubectl func(env []string, args []string) int
	// SpawnShell starts an interactive shell with KUBECONFIG set.
	SpawnShell func(kubeconfigPath string) error
	// LoginPorts overrides the loopback ports, for tests.
	LoginPorts []int
}

const usage = `usage: kubectl palette <command>

commands:
  login        log in through the browser (--from-kubeconfig <file> the first time)
  use          target a cluster in this terminal: use [name] [--admin]
  exec         run kubectl against a cluster once: exec <name> [--admin] -- <args>
  current      show the cluster and mode for this terminal
  sync         rebuild the local cluster index
  logout       delete the session, the index and admin kubeconfigs
  token        exec-credential plugin used by the kubeconfigs
  shell-init   print the shell hook: shell-init zsh|bash
  version      print the version
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
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(a.Stdout, a.Version)
	case "login":
		err = a.login(ctx, rest)
	case "token":
		err = a.token(ctx)
	case "sync":
		err = a.sync(ctx, rest)
	case "use":
		err = a.use(ctx, rest)
	case "exec":
		return a.execCmd(ctx, rest)
	case "current":
		err = a.current()
	case "logout":
		err = a.logout()
	case "shell-init":
		err = a.shellInit(rest)
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
	return oidc.Refresher{Cfg: cfg, HTTP: l.a.httpClient()}.Refresh(ctx, rt)
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
		return nil, errors.New("no API URL saved: run: kubectl palette login")
	}
	m := a.sessionMgr()
	if _, err := m.Load(); err != nil {
		return nil, err
	}
	return &palette.Client{BaseURL: cfg.APIURL, HTTP: a.httpClient(), Token: m.APIToken, Renew: m.RefreshAfterReject}, nil
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

func (a *App) token(ctx context.Context) error {
	tok, exp, err := a.sessionMgr().IDToken(ctx)
	if err != nil {
		if errors.Is(err, config.ErrNoConfig) {
			return errors.New("run: kubectl palette login")
		}
		if errors.Is(err, session.ErrNoSession) || errors.Is(err, session.ErrSessionGone) {
			return errors.New("run: kubectl palette login")
		}
		return err
	}
	var c execCredential
	c.APIVersion, c.Kind = "client.authentication.k8s.io/v1beta1", "ExecCredential"
	c.Status.Token = tok
	c.Status.ExpirationTimestamp = exp.UTC().Format(time.RFC3339)
	return json.NewEncoder(a.Stdout).Encode(c)
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
	if err := idx.Save(a.Paths.IndexFile()); err != nil {
		return nil, fmt.Errorf("save index: %w", err)
	}
	return idx, nil
}

func (a *App) sync(ctx context.Context, args []string) error {
	fs := newFlags("sync", a.Stderr)
	bg := fs.Bool("background", false, "internal: quiet refresh started by another command")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
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
		_ = a.SpawnSync()
	}
}

// resolve finds a cluster, doing one synchronous refresh and one retry on a miss.
func (a *App) resolve(ctx context.Context, idx *index.Index, fresh bool, name string) (index.Cluster, *index.Index, bool, error) {
	cl, err := idx.Resolve(name)
	if errors.Is(err, index.ErrNotFound) && !fresh {
		nidx, berr := a.buildIndex(ctx)
		if berr != nil {
			return cl, idx, false, fmt.Errorf("%w (refreshing the index failed: %v)", err, berr)
		}
		cl, err = nidx.Resolve(name)
		return cl, nidx, true, err
	}
	return cl, idx, fresh, err
}

// --- login ---

func (a *App) login(ctx context.Context, args []string) error {
	fs := newFlags("login", a.Stderr)
	fromKC := fs.String("from-kubeconfig", "", "read issuer, client id and secret from a Palette OIDC kubeconfig")
	issuer := fs.String("issuer", "", "OIDC issuer URL")
	clientID := fs.String("client-id", "", "OIDC client id")
	clientSecret := fs.String("client-secret", "", "OIDC client secret")
	apiURL := fs.String("api-url", "", "Palette API URL (default derived from the issuer host)")
	noBrowser := fs.Bool("no-browser", false, "print the login URL without opening a browser")
	importDir := fs.String("import-kubelogin-cache", "", "")
	bootstrap := fs.String("bootstrap-json", "", "")
	if _, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	}

	cfg, err := a.Paths.Load()
	if err != nil && !errors.Is(err, config.ErrNoConfig) {
		return err
	}
	if *bootstrap != "" {
		b, err := os.ReadFile(*bootstrap)
		if err != nil {
			return err
		}
		var j struct{ Issuer, ClientID, ClientSecret, API string }
		var raw map[string]string
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", *bootstrap, err)
		}
		j.Issuer, j.ClientID, j.ClientSecret, j.API = raw["issuer"], raw["client_id"], raw["client_secret"], raw["api"]
		cfg.Issuer, cfg.ClientID, cfg.ClientSecret, cfg.APIURL = j.Issuer, j.ClientID, j.ClientSecret, j.API
	}
	if *fromKC != "" {
		b, err := os.ReadFile(*fromKC)
		if err != nil {
			return err
		}
		o, err := kubeconfig.ParseOIDC(b)
		if err != nil {
			return fmt.Errorf("%s: %w", *fromKC, err)
		}
		cfg.Issuer, cfg.ClientID, cfg.ClientSecret = o.Issuer, o.ClientID, o.ClientSecret
	}
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&cfg.Issuer, *issuer}, {&cfg.ClientID, *clientID}, {&cfg.ClientSecret, *clientSecret}, {&cfg.APIURL, *apiURL},
	} {
		if f.v != "" {
			*f.dst = f.v
		}
	}
	if cfg.APIURL == "" {
		cfg.APIURL = deriveAPIURL(cfg.Issuer)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	m := a.sessionMgr()
	var sess session.Session
	if *importDir != "" {
		sess, err = importKubeloginCache(*importDir)
		if err != nil {
			return err
		}
		_, ep, err := oidc.Discover(ctx, cfg.Issuer, a.HTTP)
		if err != nil {
			return err
		}
		cfg.AuthURL, cfg.TokenURL = ep.AuthURL, ep.TokenURL
	} else {
		var open func(string) error
		if !*noBrowser {
			open = a.OpenBrowser
		}
		sess, cfg, err = oidc.Login(ctx, oidc.LoginOptions{Cfg: cfg, HTTP: a.HTTP, Ports: a.LoginPorts, Out: a.Stderr, OpenBrowser: open})
		if err != nil {
			return err
		}
	}
	if err := a.Paths.Save(cfg); err != nil {
		return err
	}
	if err := m.Save(sess); err != nil {
		return err
	}
	start := a.now()
	idx, err := a.buildIndex(ctx)
	if err != nil {
		return fmt.Errorf("logged in, but building the cluster index failed (run: kubectl palette sync): %w", err)
	}
	fmt.Fprintf(a.Stderr, "logged in; indexed %d clusters in %.1fs\n", len(idx.Clusters), a.now().Sub(start).Seconds())
	if len(idx.Clusters) == 1 {
		c := idx.Clusters[0]
		if _, _, err := a.ensureKubeconfig(ctx, c, false); err != nil {
			fmt.Fprintf(a.Stderr, "one cluster found (%s) but its kubeconfig could not be prepared: %v\n", c.Ref(), err)
			return nil
		}
		fmt.Fprintf(a.Stderr, "one cluster found and selected: %s. Run: kubectl palette use\n", c.Ref())
	}
	return nil
}

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

// ensureKubeconfig returns the path of the cluster's kubeconfig, fetching it once if absent.
func (a *App) ensureKubeconfig(ctx context.Context, c index.Cluster, admin bool) (path string, fetched bool, err error) {
	path, err = a.Paths.ClusterFile(c.Project, c.Name, admin)
	if err != nil {
		return "", false, err
	}
	if _, serr := os.Stat(path); serr == nil {
		return path, false, nil
	}
	if admin && !c.CanAdmin() {
		return "", false, fmt.Errorf("you do not hold %s on %s", index.AdminPermission, c.Ref())
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
		raw, err = cl.KubeconfigClient(ctx, c.UID, c.ProjectUID)
		if errors.Is(err, palette.ErrNotFound) {
			msg := fmt.Sprintf("%s has no OIDC configured, so there is no OIDC kubeconfig for it.", c.Ref())
			if c.CanAdmin() {
				msg += fmt.Sprintf(" You can use the admin kubeconfig: kubectl palette use %s --admin", c.Ref())
			}
			return "", false, errors.New(msg)
		}
		if err != nil {
			return "", false, fmt.Errorf("fetch kubeconfig for %s: %w", c.Ref(), err)
		}
		cfg, cerr := a.Paths.Load()
		if cerr != nil {
			return "", false, cerr
		}
		o, _ := kubeconfig.ParseOIDC(raw)
		raw, err = kubeconfig.Rewrite(raw, cfg.ClientSecret, o.ClientSecret)
		if err != nil {
			return "", false, fmt.Errorf("rewrite kubeconfig for %s: %w", c.Ref(), err)
		}
	}
	if err := config.WriteFileAtomic(path, raw); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func (a *App) use(ctx context.Context, args []string) error {
	fs := newFlags("use", a.Stderr)
	admin := fs.Bool("admin", false, "use the admin kubeconfig (needs the download permission)")
	printEnv := fs.Bool("print-env", false, "print an export line for the shell hook")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitError(2)
	}
	if len(pos) > 1 {
		return errors.New("usage: kubectl palette use [name] [--admin]")
	}
	idx, fresh, err := a.loadOrBuildIndex(ctx)
	if err != nil {
		return err
	}
	var c index.Cluster
	if len(pos) == 1 {
		c, idx, fresh, err = a.resolve(ctx, idx, fresh, pos[0])
		if err != nil {
			return err
		}
	} else {
		c, err = a.pick(idx)
		if err != nil {
			return err
		}
	}
	path, fetched, err := a.ensureKubeconfig(ctx, c, *admin)
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
	fmt.Fprintf(a.Stderr, "using %s (%s)\n", c.Ref(), mode)
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

// pick chooses a cluster without a name: the only cluster, or a numbered list on a TTY.
func (a *App) pick(idx *index.Index) (index.Cluster, error) {
	switch len(idx.Clusters) {
	case 0:
		return index.Cluster{}, errors.New("the index has no clusters: run: kubectl palette sync")
	case 1:
		return idx.Clusters[0], nil
	}
	if !a.StdinIsTTY {
		return index.Cluster{}, errors.New("name a cluster: kubectl palette use <name>")
	}
	for i, c := range idx.Clusters {
		fmt.Fprintf(a.Stderr, "%4d  %s\n", i+1, c.Ref())
	}
	fmt.Fprint(a.Stderr, "number or name: ")
	var in string
	if _, err := fmt.Fscanln(a.Stdin, &in); err != nil {
		return index.Cluster{}, errors.New("no cluster chosen")
	}
	var n int
	if _, err := fmt.Sscanf(in, "%d", &n); err == nil && n >= 1 && n <= len(idx.Clusters) {
		return idx.Clusters[n-1], nil
	}
	return idx.Resolve(in)
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
	pos, err := parseFlags(fs, before)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || !split || len(kargs) == 0 {
		fmt.Fprintln(a.Stderr, "usage: kubectl palette exec <name> [--admin] -- <kubectl args>")
		return 2
	}
	run := func() (int, error) {
		idx, fresh, err := a.loadOrBuildIndex(ctx)
		if err != nil {
			return 0, err
		}
		c, idx, fresh, err := a.resolve(ctx, idx, fresh, pos[0])
		if err != nil {
			return 0, err
		}
		path, _, err := a.ensureKubeconfig(ctx, c, *admin)
		if err != nil {
			return 0, err
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

func (a *App) current() error {
	kc := a.Getenv("KUBECONFIG")
	rel, err := filepath.Rel(a.Paths.KubeDir, kc)
	if kc == "" || err != nil || strings.Contains(kc, string(os.PathListSeparator)) || strings.HasPrefix(rel, "..") {
		fmt.Fprintln(a.Stdout, "no Palette cluster selected in this terminal")
		return exitError(1)
	}
	dir, file := filepath.Split(rel)
	mode := "oidc"
	name := strings.TrimSuffix(file, ".yaml")
	if strings.HasSuffix(name, ".admin") {
		mode, name = "admin", strings.TrimSuffix(name, ".admin")
	}
	fmt.Fprintf(a.Stdout, "%s%s (%s)\n", dir, name, mode)
	return nil
}

func (a *App) logout() error {
	var errs []error
	if err := a.sessionMgr().Delete(); err != nil {
		errs = append(errs, err)
	}
	for _, p := range []string{a.Paths.IndexFile(), a.Paths.IndexLock()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	removed := 0
	_ = filepath.WalkDir(a.Paths.KubeDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".admin.yaml") {
			if rerr := os.Remove(p); rerr != nil {
				errs = append(errs, rerr)
			} else {
				removed++
			}
		}
		return nil
	})
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	fmt.Fprintf(a.Stderr, "logged out; removed the session, the index and %d admin kubeconfig(s)\n", removed)
	return nil
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

// SpawnShellDefault starts $SHELL with KUBECONFIG set.
func SpawnShellDefault(kubeconfigPath string) error {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	cmd := exec.Command(sh)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// IsTTY reports whether f is a terminal.
func IsTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
