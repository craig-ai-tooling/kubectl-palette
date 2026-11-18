package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (h *harness) setClusters(s string) {
	h.f.mu.Lock()
	h.f.clusters = s
	h.f.mu.Unlock()
}

func kubeconfigHits(h *harness, uid string) int {
	return h.f.count("/v1/spectroclusters/" + uid + "/assets/kubeconfigclient")
}

func TestCacheHitRequiresMatchingClusterUID(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	if code, _, e := h.run("use", "alpha", "--print-env"); code != 0 {
		t.Fatal(e)
	}
	// Same project and name, different cluster.
	h.setClusters(`[{"name":"alpha","uid":"ua2","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]`)
	if code, _, e := h.run("sync"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := h.run("use", "alpha", "--print-env"); code != 0 {
		t.Fatal(e)
	}
	if kubeconfigHits(h, "ua2") != 1 {
		t.Fatalf("rebuilt cluster reused the old cached file: %v", h.f.hits)
	}
	// exec checks the same thing.
	h.setClusters(`[{"name":"alpha","uid":"ua3","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]`)
	h.run("sync")
	h.run("exec", "alpha", "--", "get", "ns")
	if kubeconfigHits(h, "ua3") != 1 {
		t.Fatalf("exec reused the old cached file: %v", h.f.hits)
	}
}

func TestLegacyCacheFileWithoutUIDIsRefetched(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	path := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("legacy-file-from-the-old-layout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := h.run("use", "alpha", "--print-env"); code != 0 {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "legacy-file") || kubeconfigHits(h, "ua") != 1 {
		t.Fatalf("a file with no UID sidecar was trusted: hits %v", h.f.hits)
	}
}

func TestRefreshFlagForcesRefetch(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("use", "alpha", "--print-env")
	h.run("use", "alpha", "--print-env")
	if kubeconfigHits(h, "ua") != 1 {
		t.Fatalf("warm use refetched: %v", h.f.hits)
	}
	if code, _, e := h.run("use", "alpha", "--refresh", "--print-env"); code != 0 || kubeconfigHits(h, "ua") != 2 {
		t.Fatalf("use --refresh: code %d %s hits %v", code, e, h.f.hits)
	}
	if code, _, e := h.run("exec", "alpha", "--refresh", "--", "get", "ns"); code != 7 || kubeconfigHits(h, "ua") != 3 {
		t.Fatalf("exec --refresh: code %d %s hits %v", code, e, h.f.hits)
	}
	if got := h.kargs[len(h.kargs)-1]; strings.Join(got, " ") != "get ns" {
		t.Fatalf("--refresh leaked into kubectl args: %v", got)
	}
}

func TestPickNumberedUsesWholeInput(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.setClusters(`[{"name":"aaa","uid":"u1","annotations":{"projectUid":"p1"}},{"name":"bbb","uid":"u2","annotations":{"projectUid":"p1"}},{"name":"2-prod","uid":"u3","annotations":{"projectUid":"p1"}}]`)
	h.a.StdinIsTTY = true
	h.a.Stdin = strings.NewReader("2-prod\n")
	code, out, e := h.run("use", "--print-env", "--no-fuzzy")
	if code != 0 || !strings.Contains(out, "2-prod.yaml") {
		t.Fatalf("2-prod chose the wrong cluster: %d %q %s", code, out, e)
	}
	h.a.Stdin = strings.NewReader("2\n")
	if code, out, _ = h.run("use", "--print-env", "--no-fuzzy"); code != 0 || strings.Contains(out, "2-prod") {
		t.Fatalf("a plain row number must still work: %d %q", code, out)
	}
}

func TestShellFor(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
		want       string
	}{
		{"unix default", "linux", nil, "/bin/sh"},
		{"unix SHELL", "linux", map[string]string{"SHELL": "/bin/zsh"}, "/bin/zsh"},
		{"windows COMSPEC", "windows", map[string]string{"COMSPEC": `C:\Windows\System32\cmd.exe`}, `C:\Windows\System32\cmd.exe`},
		{"windows nothing set", "windows", nil, "powershell.exe"},
		{"windows git bash", "windows", map[string]string{"SHELL": `C:\Git\bin\bash.exe`, "COMSPEC": "cmd.exe"}, `C:\Git\bin\bash.exe`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellFor(tc.goos, env(tc.env)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestIsTTYOnlyForRealTerminals(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no null device")
	}
	defer null.Close()
	if IsTTY(null) {
		t.Error("the null device is not a terminal")
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if IsTTY(r) || IsTTY(w) {
		t.Error("a pipe is not a terminal")
	}
	reg, _ := os.Create(filepath.Join(t.TempDir(), "f"))
	defer reg.Close()
	if IsTTY(reg) {
		t.Error("a regular file is not a terminal")
	}
}

func TestAdminFileCannotBeReturnedForDotAdminCluster(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.setClusters(`[
	 {"name":"foo","uid":"uf","annotations":{"projectUid":"p1","permissions":"cluster.adminKubeconfigDownload"}},
	 {"name":"foo.admin","uid":"ufa","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]`)
	_, adminOut, e := h.run("use", "foo", "--admin", "--print-env")
	if !strings.Contains(adminOut, ".admin") {
		t.Fatal(e)
	}
	code, out, e := h.run("use", "Default/foo.admin", "--print-env")
	if code != 0 {
		t.Fatal(e)
	}
	if out == adminOut {
		t.Fatalf("OIDC selection of foo.admin returned foo's admin file: %s", out)
	}
	if kubeconfigHits(h, "ufa") != 1 || h.f.count("/v1/spectroclusters/uf/assets/adminKubeconfig") != 1 {
		t.Fatalf("hits %v", h.f.hits)
	}
}

func TestAdminCacheHitRechecksPermission(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	code, out, e := h.run("use", "alpha", "--admin", "--print-env")
	if code != 0 {
		t.Fatal(e)
	}
	path := strings.Trim(strings.TrimPrefix(strings.TrimSpace(out), "export KUBECONFIG="), "'")
	h.setClusters(`[{"name":"alpha","uid":"ua","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]`)
	h.run("sync")
	code, out, e = h.run("use", "alpha", "--admin", "--print-env")
	if code == 0 || !strings.Contains(e, "cluster.adminKubeconfigDownload") || out != "" {
		t.Fatalf("cached admin file served after the permission was removed: %d %q %s", code, out, e)
	}
	for _, p := range []string{path, path + ".uid"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was not deleted", p)
		}
	}
	// exec goes through the same gate
	if code, _, _ := h.run("exec", "alpha", "--admin", "--", "get", "ns"); code == 0 || len(h.kargs) != 0 {
		t.Fatal("exec ran kubectl with admin credentials")
	}
}

func TestLogoutReportsWhatItCouldNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("use", "alpha", "--admin", "--print-env")
	h.run("use", "plain", "--admin", "--print-env")
	locked := filepath.Join(h.a.Paths.KubeDir, "Other")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o700)
	code, _, e := h.run("logout")
	if code == 0 || strings.Contains(e, "logged out;") || !strings.Contains(e, "Other") {
		t.Fatalf("logout hid a failure: %d %s", code, e)
	}
	// What could be removed still was.
	if _, err := os.Stat(filepath.Join(h.a.Paths.KubeDir, "Default", ".admin")); err == nil {
		t.Error("readable admin files survived")
	}
}

func TestLogoutRemovesLegacyAdminFiles(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	legacy := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.admin.yaml")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	os.WriteFile(legacy, []byte("x"), 0o600)
	if code, _, e := h.run("logout"); code != 0 {
		t.Fatal(e)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Fatal("legacy admin file survived logout")
	}
}

// newIssuerServer serves just enough discovery for login --import-kubelogin-cache.
func newIssuerServer(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/auth",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func kubeloginCache(t *testing.T) string {
	dir := t.TempDir()
	id := jwtExp(time.Now().Add(time.Hour), "new")
	os.WriteFile(filepath.Join(dir, "tok"), []byte(fmt.Sprintf(`{"id_token":%q,"refresh_token":"rt-0"}`, id)), 0o600)
	return dir
}

func TestLoginIssuerChangeDoesNotKeepOldAPIAndWipesState(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("use", "alpha", "--print-env")
	h.run("use", "alpha", "--admin", "--print-env")
	oidcFile := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")
	adminFile := filepath.Join(h.a.Paths.KubeDir, "Default", ".admin", "alpha.yaml")
	newIss := newIssuerServer(t)
	hitsBefore := h.f.total()

	// No --api-url and an issuer we cannot derive one from: it must refuse rather than send
	// the new refresh token to the old API URL, and it must change nothing.
	code, _, e := h.run("login", "--issuer", newIss.URL, "--import-kubelogin-cache", kubeloginCache(t))
	if code == 0 || !strings.Contains(e, "API URL") {
		t.Fatalf("login kept the old API URL: %d %s", code, e)
	}
	if h.f.total() != hitsBefore {
		t.Fatalf("new session reached the old installation: %v", h.f.hits)
	}
	cfg, _ := h.a.Paths.Load()
	if cfg.Issuer != "https://i" || cfg.APIURL != h.f.srv.URL {
		t.Fatalf("a refused login changed the config: %+v", cfg)
	}
	for _, p := range []string{oidcFile, adminFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("a refused login wiped %s", p)
		}
	}

	// With an explicit --api-url the switch goes through and the old state is gone.
	code, _, e = h.run("login", "--issuer", newIss.URL, "--api-url", h.f.srv.URL, "--import-kubelogin-cache", kubeloginCache(t))
	if code != 0 {
		t.Fatalf("%s", e)
	}
	cfg, _ = h.a.Paths.Load()
	if cfg.Issuer != newIss.URL {
		t.Fatalf("issuer not saved: %+v", cfg)
	}
	for _, p := range []string{oidcFile, oidcFile + ".uid", adminFile, adminFile + ".uid"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived an issuer change", p)
		}
	}
}

func TestLoginSameIssuerKeepsCachedFiles(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.run("use", "alpha", "--print-env")
	oidcFile := filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")
	// "https://i" is the saved issuer; discovery is not needed because we only check the
	// wipe decision through a bootstrap login that fails before saving.
	h.run("login", "--issuer", "https://i", "--import-kubelogin-cache", t.TempDir())
	if _, err := os.Stat(oidcFile); err != nil {
		t.Fatal("same-issuer login wiped the cache")
	}
}
