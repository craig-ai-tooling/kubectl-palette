package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

const oidcKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://10.0.0.1:6443"}
contexts:
- name: x
  context: {cluster: c, user: u}
current-context: x
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: kubectl
      args: [oidc-login, get-token, --oidc-issuer-url=https://i, --oidc-client-id=T, --oidc-client-secret=TOPSECRET]
`

// oidcKubeconfigFor is oidcKubeconfig as a cluster that trusts the given issuer would serve it.
func oidcKubeconfigFor(issuer string) string {
	return strings.Replace(oidcKubeconfig, "--oidc-issuer-url=https://i,", "--oidc-issuer-url="+issuer+",", 1)
}

func jwtExp(exp time.Time, tag string) string {
	e := base64.RawURLEncoding
	return e.EncodeToString([]byte(`{"alg":"none"}`)) + "." + e.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"t":%q}`, exp.Unix(), tag))) + ".s"
}

// fakePalette serves the Palette API and a token endpoint and counts requests per path.
type fakePalette struct {
	srv       *httptest.Server
	mu        sync.Mutex
	hits      map[string]int
	clusters  string // JSON items for metadata
	noOIDC    map[string]bool
	fail      map[string]bool // cluster uid -> kubeconfig download answers 500
	delay     time.Duration   // held by every kubeconfig download, to overlap them
	cur, max  int             // kubeconfig downloads in flight now and at most
	tokenBody string
	issuers   map[string]string // API token -> issuer its clusters trust; default https://i
}

func newFakePalette(t *testing.T) *fakePalette {
	f := &fakePalette{hits: map[string]int{}, noOIDC: map[string]bool{}, fail: map[string]bool{}, issuers: map[string]string{}}
	f.clusters = `[
	 {"name":"alpha","uid":"ua","annotations":{"projectUid":"p1","permissions":"cluster.get,cluster.adminKubeconfigDownload"}},
	 {"name":"beta","uid":"ub","annotations":{"projectUid":"p1","permissions":"cluster.get"}},
	 {"name":"plain","uid":"un","annotations":{"projectUid":"p2","permissions":"cluster.adminKubeconfigDownload"}}]`
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		clusters := f.clusters
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			if f.tokenBody != "" {
				fmt.Fprint(w, f.tokenBody)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer", "refresh_token": "rt-new",
				"id_token": jwtExp(time.Now().Add(time.Hour), "refreshed")})
			return
		case !strings.HasPrefix(r.Header.Get("Authorization"), "rt-0") && r.Header.Get("Authorization") != "rt-new":
			w.WriteHeader(401)
		case r.URL.Path == "/v1/dashboard/spectroclusters/metadata":
			fmt.Fprintf(w, `{"items":%s}`, clusters)
		case r.URL.Path == "/v1/dashboard/projects":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"Default","uid":"p1"}},{"metadata":{"name":"Other","uid":"p2"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/assets/kubeconfigclient"):
			if f.noOIDC[strings.Split(r.URL.Path, "/")[3]] {
				http.NotFound(w, r)
				return
			}
			f.mu.Lock()
			f.cur++
			f.max = max(f.max, f.cur)
			f.mu.Unlock()
			defer func() { f.mu.Lock(); f.cur--; f.mu.Unlock() }()
			time.Sleep(f.delay)
			if f.fail[strings.Split(r.URL.Path, "/")[3]] {
				w.WriteHeader(500)
				return
			}
			if r.Header.Get("ProjectUid") == "" {
				w.WriteHeader(400)
				return
			}
			f.mu.Lock()
			iss := f.issuers[r.Header.Get("Authorization")]
			f.mu.Unlock()
			if iss == "" {
				iss = "https://i"
			}
			fmt.Fprint(w, oidcKubeconfigFor(iss))
		case strings.HasSuffix(r.URL.Path, "/assets/adminKubeconfig"):
			fmt.Fprint(w, "apiVersion: v1\nusers:\n- name: a\n  user: {client-certificate-data: Q0VSVA==}\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePalette) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakePalette) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, c := range f.hits {
		if p != "/token" {
			n += c
		}
	}
	return n
}

type harness struct {
	a      *App
	f      *fakePalette
	out    *bytes.Buffer
	errb   *bytes.Buffer
	env    map[string]string
	spawns int
	kargs  [][]string
	kenv   [][]string
}

func newHarness(t *testing.T, idExp time.Time) *harness {
	f := newFakePalette(t)
	dir := t.TempDir()
	h := &harness{f: f, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, env: map[string]string{}}
	h.a = &App{
		Version: "test", Stdout: h.out, Stderr: h.errb, Stdin: strings.NewReader(""),
		Getenv:    func(k string) string { return h.env[k] },
		Paths:     testPaths(dir),
		SpawnSync: func(string) error { h.spawns++; return nil },
		RunKubectl: func(env, args []string) int {
			h.kenv, h.kargs = append(h.kenv, env), append(h.kargs, args)
			return 7
		},
	}
	cfg := config.Config{Issuer: "https://i", ClientID: "T", ClientSecret: "TOPSECRET", APIURL: f.srv.URL, TokenURL: f.srv.URL + "/token"}
	if err := h.a.Paths.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := h.a.sessionMgr().Save(session.Session{IDToken: jwtExp(idExp, "orig"), RefreshToken: "rt-0", IDExpiry: idExp}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) run(args ...string) (int, string, string) {
	h.out.Reset()
	h.errb.Reset()
	code := h.a.Run(context.Background(), args)
	return code, h.out.String(), h.errb.String()
}

func TestUseColdThenWarm(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	code, out, errs := h.run("use", "alpha", "--print-env")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	path := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")
	if strings.TrimSpace(out) != "export KUBECONFIG='"+path+"'" {
		t.Fatalf("stdout = %q", out)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig %v %v", fi, err)
	}
	b, _ := os.ReadFile(path)
	for _, bad := range []string{"TOPSECRET", "oidc-login"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("kubeconfig contains %q", bad)
		}
	}
	if !strings.Contains(string(b), "kubectl-palette") {
		t.Errorf("exec command not rewritten:\n%s", b)
	}
	// cold: one index build (2 calls) plus one kubeconfig fetch
	if h.f.count("/v1/spectroclusters/ua/assets/kubeconfigclient") != 1 {
		t.Fatalf("hits %v", h.f.hits)
	}
	before := h.f.total()
	code, out, _ = h.run("use", "alpha", "--print-env")
	if code != 0 || h.f.total() != before {
		t.Fatalf("warm use made API calls: %v", h.f.hits)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("warm stdout = %q", out)
	}
}

func TestUseMissThenRefresh(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	if code, _, e := h.run("sync"); code != 0 {
		t.Fatal(e)
	}
	h.f.mu.Lock()
	h.f.clusters = strings.Replace(h.f.clusters, "]", `,{"name":"gamma","uid":"ug","annotations":{"projectUid":"p1"}}]`, 1)
	h.f.mu.Unlock()
	metaBefore := h.f.count("/v1/dashboard/spectroclusters/metadata")
	if code, _, e := h.run("use", "gamma", "--print-env"); code != 0 {
		t.Fatalf("%s", e)
	}
	if h.f.count("/v1/dashboard/spectroclusters/metadata")-metaBefore != 1 {
		t.Fatal("expected exactly one refresh on a miss")
	}
	// a name that is still missing refreshes once and then fails
	metaBefore = h.f.count("/v1/dashboard/spectroclusters/metadata")
	code, _, e := h.run("use", "nosuch", "--print-env")
	if code == 0 || !strings.Contains(e, "no such cluster") || h.f.count("/v1/dashboard/spectroclusters/metadata")-metaBefore != 1 {
		t.Fatalf("code %d %s", code, e)
	}
}

func TestUseAmbiguousDoesNotRefresh(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.f.mu.Lock()
	h.f.clusters = `[{"name":"dup","uid":"u1","annotations":{"projectUid":"p1"}},{"name":"dup","uid":"u2","annotations":{"projectUid":"p2"}}]`
	h.f.mu.Unlock()
	h.run("sync")
	n := h.f.total()
	code, _, e := h.run("use", "dup", "--print-env")
	if code == 0 || !strings.Contains(e, "Default/dup") || !strings.Contains(e, "Other/dup") || h.f.total() != n {
		t.Fatalf("code %d %s hits %v", code, e, h.f.hits)
	}
	if code, _, e = h.run("use", "Other/dup", "--print-env"); code != 0 {
		t.Fatal(e)
	}
}

func TestUseNoOIDC(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.f.noOIDC["un"] = true
	code, _, e := h.run("use", "plain", "--print-env")
	if code == 0 || !strings.Contains(e, "no OIDC") || !strings.Contains(e, "--admin") {
		t.Fatalf("code %d %s", code, e)
	}
	h.f.noOIDC["ub"] = true
	_, _, e = h.run("use", "beta", "--print-env")
	if !strings.Contains(e, "no OIDC") || strings.Contains(e, "--admin") {
		t.Fatalf("no admin pointer expected without the permission: %s", e)
	}
}

func TestAdminGate(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("sync")
	n := h.f.total()
	code, _, e := h.run("use", "beta", "--admin", "--print-env")
	if code == 0 || !strings.Contains(e, "cluster.adminKubeconfigDownload") || h.f.total() != n {
		t.Fatalf("code %d %s hits %v", code, e, h.f.hits)
	}
	code, out, e := h.run("use", "alpha", "--admin", "--print-env")
	if code != 0 {
		t.Fatal(e)
	}
	path := filepath.Join(h.a.Paths.KubeDir, "Default", ".admin", "alpha.yaml")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 || !strings.Contains(out, path) {
		t.Fatalf("%v %v %q", fi, err, out)
	}
	if h.f.count("/v1/spectroclusters/ua/assets/adminKubeconfig") != 1 {
		t.Fatal("admin kubeconfig not fetched once")
	}
	// current reports admin mode
	h.env["KUBECONFIG"] = path
	if _, out, _ := h.run("current"); strings.TrimSpace(out) != "Default/alpha (admin)" {
		t.Fatalf("current = %q", out)
	}
	// logout removes it, the session and the index
	if code, _, e := h.run("logout"); code != 0 {
		t.Fatal(e)
	}
	for _, p := range []string{path, h.a.Paths.SessionFile(), h.a.Paths.IndexFile()} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived logout", p)
		}
	}
	// the OIDC kubeconfig holds no secret and is left alone
	if code, _, _ := h.run("token"); code == 0 {
		t.Fatal("token must fail after logout")
	}
}

func TestStaleIndexSpawnsBackgroundRefresh(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("sync")
	h.run("use", "alpha", "--print-env")
	if h.spawns != 0 {
		t.Fatal("fresh index must not spawn")
	}
	h.a.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	n := h.f.total()
	h.run("use", "alpha", "--print-env")
	if h.spawns != 1 || h.f.total() != n {
		t.Fatalf("spawns %d, API calls %d -> %d", h.spawns, n, h.f.total())
	}
}

func TestSyncBackgroundSkipsWhenLocked(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	lk, err := session.LockFile(h.a.Paths.IndexLock())
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Unlock()
	if code, _, _ := h.run("sync", "--background"); code != 0 || h.f.total() != 0 {
		t.Fatalf("background sync ran while locked: %v", h.f.hits)
	}
}

func TestTokenShapeAndZeroNetwork(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	h := newHarness(t, exp)
	code, out, e := h.run("token")
	if code != 0 {
		t.Fatal(e)
	}
	var c struct {
		APIVersion, Kind string
		Status           struct{ Token, ExpirationTimestamp string }
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.APIVersion != "client.authentication.k8s.io/v1beta1" || c.Kind != "ExecCredential" ||
		c.Status.ExpirationTimestamp != exp.UTC().Format(time.RFC3339) || !strings.Contains(c.Status.Token, ".") {
		t.Fatalf("%+v", c)
	}
	if len(h.f.hits) != 0 {
		t.Fatalf("warm token made network calls: %v", h.f.hits)
	}
}

func TestTokenRefreshesNearExpiry(t *testing.T) {
	h := newHarness(t, time.Now().Add(30*time.Second))
	code, out, e := h.run("token")
	if code != 0 || !strings.Contains(out, `"status"`) {
		t.Fatalf("%d %s %s", code, out, e)
	}
	s, _ := h.a.sessionMgr().Load()
	if s.RefreshToken != "rt-new" || s.IDExpiry.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("rotation not persisted: %+v", s)
	}
}

func TestToken200ErrorBody(t *testing.T) {
	h := newHarness(t, time.Now().Add(-time.Minute))
	h.f.tokenBody = `{"error":"session not found"}`
	code, out, e := h.run("token")
	if code == 0 || out != "" || !strings.Contains(e, "run: kubectl palette login") {
		t.Fatalf("code %d out %q err %q", code, out, e)
	}
	// the old pair is still on disk: a failed refresh must not wipe it
	if s, err := h.a.sessionMgr().Load(); err != nil || s.RefreshToken != "rt-0" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestTokenNoSession(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.a.sessionMgr().Delete()
	code, _, e := h.run("token")
	if code == 0 || !strings.Contains(e, "run: kubectl palette login") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestExecRunsKubectlWithKubeconfig(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	code, _, e := h.run("exec", "alpha", "--", "get", "ns", "-o", "name")
	if code != 7 {
		t.Fatalf("exit code should pass through: %d %s", code, e)
	}
	want := "KUBECONFIG=" + filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")
	if h.kenv[0][0] != want || strings.Join(h.kargs[0], " ") != "get ns -o name" {
		t.Fatalf("%v %v", h.kenv, h.kargs)
	}
	if code, _, _ := h.run("exec", "alpha"); code != 2 {
		t.Fatal("exec without -- must be a usage error")
	}
}

func TestCurrentNone(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.env["KUBECONFIG"] = "/home/u/.kube/config"
	if code, out, _ := h.run("current"); code != 1 || !strings.Contains(out, "no Palette cluster") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestUsePicker(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.a.StdinIsTTY = true
	h.a.Stdin = strings.NewReader("2\n")
	code, out, e := h.run("use", "--print-env", "--no-fuzzy")
	if code != 0 || !strings.Contains(out, "beta.yaml") || !strings.Contains(e, "1  Default/alpha") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	h.a.StdinIsTTY = false
	if code, _, _ = h.run("use", "--print-env"); code == 0 {
		t.Fatal("no name and no TTY must fail")
	}
}

func TestUseSingleClusterAutoSelects(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.f.clusters = `[{"name":"only","uid":"uo","annotations":{"projectUid":"p1"}}]`
	code, out, e := h.run("use", "--print-env")
	if code != 0 || !strings.Contains(out, "only.yaml") {
		t.Fatalf("%d %q %s", code, out, e)
	}
}

func TestUseWithoutHookSpawnsShell(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.a.StdoutIsTTY = true
	var got string
	h.a.SpawnShell = func(p string) error { got = p; return nil }
	code, out, _ := h.run("use", "alpha")
	if code != 0 || out != "" || !strings.HasSuffix(got, "alpha.yaml") {
		t.Fatalf("%d %q %q", code, out, got)
	}
}

func TestShellInitAndUsage(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	if code, out, _ := h.run("shell-init", "zsh"); code != 0 || !strings.Contains(out, "function kubectl {") {
		t.Fatal(out)
	}
	if code, _, _ := h.run("shell-init", "fish"); code == 0 {
		t.Fatal("fish unsupported")
	}
	if code, out, _ := h.run("version"); code != 0 || out != "test\n" {
		t.Fatal(out)
	}
	if code, _, _ := h.run("bogus"); code != 2 {
		t.Fatal("unknown command")
	}
	if code, _, _ := h.run(); code != 2 {
		t.Fatal("no args")
	}
}

func TestIndexNeverFetchesKubeconfigsInBulk(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("sync")
	for p := range h.f.hits {
		if strings.Contains(p, "assets") {
			t.Fatalf("sync fetched %s", p)
		}
	}
	if _, err := index.Load(h.a.Paths.IndexFile()); err != nil {
		t.Fatal(err)
	}
	// metadata and projects are the only two calls
	if h.f.total() != 2 {
		t.Fatalf("sync made %d calls: %v", h.f.total(), h.f.hits)
	}
}

func TestImportKubeloginCache(t *testing.T) {
	dir := t.TempDir()
	id := jwtExp(time.Now().Add(time.Hour), "i")
	os.WriteFile(filepath.Join(dir, "abc"), []byte(fmt.Sprintf(`{"id_token":%q,"refresh_token":"r"}`, id)), 0o600)
	os.WriteFile(filepath.Join(dir, "abc.lock"), nil, 0o600)
	s, err := importKubeloginCache(dir)
	if err != nil || s.RefreshToken != "r" || s.IDToken != id {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := importKubeloginCache(t.TempDir()); err == nil {
		t.Fatal("empty dir must fail")
	}
}

func TestDeriveAPIURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://console.spectrocloud.com/v1/oidc/tenant/x": "https://api.spectrocloud.com",
		"https://palette.corp.example/v1/oidc/tenant/x":     "",
		"::": "",
	} {
		if got := deriveAPIURL(in); got != want {
			t.Errorf("deriveAPIURL(%q) = %q, want %q", in, got, want)
		}
	}
}
