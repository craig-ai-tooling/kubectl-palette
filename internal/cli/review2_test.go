package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
)

// Finding A: token refuses a kubeconfig made for another Palette login and prints nothing.
func TestTokenRefusesWrongIssuerID(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-acme", "acme")
	good := config.IssuerID("https://i/acme")
	for name, args := range map[string][]string{
		"wrong id":                 {"--tenant", "acme", "--issuer-id", "0000000000000000"},
		"named entry without id":   {"--tenant", "acme"},
		"default with wrong id":    {"--issuer-id", "0000000000000000"},
		"default tenant, wrong id": {"--tenant", "default", "--issuer-id", "0000000000000000"},
	} {
		code, out, e := h.run(append([]string{"token"}, args...)...)
		if code == 0 || out != "" || !strings.Contains(e, "different Palette login") || !strings.Contains(e, "contexts prune") {
			t.Errorf("%s: code %d out %q err %q", name, code, out, e)
		}
	}
	if code, out, e := h.run("token", "--tenant", "acme", "--issuer-id", good); code != 0 || out == "" {
		t.Fatalf("good id refused: %d %s", code, e)
	}
	// Files from v0.1.0 have plain "token" and stay valid for the default entry.
	if code, out, e := h.run("token"); code != 0 || out == "" {
		t.Fatalf("plain token refused: %d %s", code, e)
	}
}

// Findings A and D: contexts kept after logout cannot be used by a later login that reuses
// the entry name, and a new entry cleans up what an earlier one left.
func TestKeptContextsCannotServeANewLogin(t *testing.T) {
	h := ctxHarness(t)
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "use", "beta", "--tenant", "acme", "--print-env")
	rec := h.records(t)[0]
	cfg := h.kube(t)
	args := cfg.AuthInfos[rec.User].Exec.Args
	if len(args) != 5 || args[3] != "--issuer-id" {
		t.Fatalf("args %v", args)
	}
	h.mustRun(t, "logout", "--tenant", "acme", "--keep-contexts")
	if len(h.records(t)) != 1 {
		t.Fatal("record should stay")
	}
	// The tenant name now belongs to another Palette.
	srv := tenantServer(t, "acme", uidAcme, "acme-web")
	h.a.Login = fakeLogin(map[string]string{uidAcme: "acme"})
	e := h.mustRun(t, "login", "--server", srv.URL, "--no-browser")
	if !strings.Contains(e, "left from an earlier tenant entry named acme") {
		t.Fatalf("no cleanup line: %s", e)
	}
	for _, r := range h.records(t) {
		if r.Context == rec.Context && r.IssuerID == rec.IssuerID {
			t.Fatalf("old record survived: %+v", r)
		}
	}
	// The old context's pin no longer matches the entry.
	if code, out, e := h.run(append([]string{"token"}, args...)...); code == 0 || out != "" {
		t.Fatalf("old pin was served: %d %q %s", code, out, e)
	}
}

// Finding D: cleanup works for a tenant name with records but no saved entry.
func TestRemoveAndPruneForTenantWithoutEntry(t *testing.T) {
	for _, via := range []string{"remove", "prune", "prune-tenant"} {
		h := ctxHarness(t)
		h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
		h.mustRun(t, "use", "alpha", "--print-env")
		h.mustRun(t, "use", "beta", "--tenant", "acme", "--print-env")
		h.mustRun(t, "logout", "--tenant", "acme", "--keep-contexts")
		if len(h.records(t)) != 2 {
			t.Fatal("setup")
		}
		switch via {
		case "remove":
			h.mustRun(t, "contexts", "remove", "--all", "--tenant", "acme")
		case "prune":
			h.mustRun(t, "contexts", "prune")
		case "prune-tenant":
			h.mustRun(t, "contexts", "prune", "--tenant", "acme")
		}
		recs := h.records(t)
		if len(recs) != 1 || recs[0].Tenant != "default" {
			t.Fatalf("%s: records %+v", via, recs)
		}
		if names := h.contextNames(t); len(names) != 1 || names[0] != "alpha" {
			t.Fatalf("%s: contexts %v", via, names)
		}
	}
}

// Finding D/A: a record whose entry now holds another login is stale.
func TestPruneDropsRecordsOfAnotherLogin(t *testing.T) {
	h := ctxHarness(t)
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "use", "beta", "--tenant", "acme", "--print-env")
	cfg := h.namedCfg("acme")
	cfg.Issuer = "https://other-palette/acme"
	h.addEntry(t, "acme", cfg, "rt-0", "acme")
	h.mustRun(t, "contexts", "prune", "--tenant", "acme")
	if len(h.records(t)) != 0 || len(h.contextNames(t)) != 0 {
		t.Fatalf("%+v %v", h.records(t), h.contextNames(t))
	}
}

// Finding E: entry names are lowercase.
func TestEntryNamesAreLowercase(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	srv := tenantServer(t, "Acme", uidAcme, "acme-web")
	h.a.Login = fakeLogin(map[string]string{uidAcme: "Acme"})
	if code, _, e := h.run("login", "--server", srv.URL, "--no-browser", "--name", "ACME"); code == 0 || !strings.Contains(e, "lowercase") {
		t.Fatalf("uppercase --name accepted: %d %s", code, e)
	}
	if code, _, e := h.run("login", "--server", srv.URL, "--no-browser"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if got := strings.Join(h.a.Paths.Root().Entries(), ","); got != "default,acme" {
		t.Fatalf("entries %q", got)
	}
	if code, out, e := h.run("tenant", "ACME"); code != 0 || !strings.Contains(e, "active tenant: acme") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if code, _, e := h.run("sync", "--tenant", "ACME"); code != 0 {
		t.Fatalf("sync: %d %s", code, e)
	}
	if code, _, e := h.run("token", "--tenant", "ACME", "--issuer-id", "x"); code == 0 || !strings.Contains(e, "invalid tenant entry name") {
		t.Fatalf("token: %d %s", code, e)
	}
}

// Finding H: a re-pointed context is reported as left in place.
func TestContextsRemoveReportsLeftInPlace(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--print-env")
	h.mustRun(t, "use", "beta", "--print-env")
	cfg := h.kube(t)
	cfg.Contexts["alpha"].AuthInfo = "mine"
	writeKube(t, h, cfg)
	e := h.mustRun(t, "contexts", "remove", "alpha", "beta")
	if !strings.Contains(e, "left alpha in place (you changed it), no longer tracked") || !strings.Contains(e, "removed 1 context from") {
		t.Fatalf("%s", e)
	}
	e = h.mustRun(t, "contexts", "remove", "--all")
	if strings.Contains(e, "removed 1 context") {
		t.Fatal(e)
	}
}

// Finding I: one note when $KUBECONFIG hides the context file.
func TestNoteWhenKubeconfigEnvHidesContextFile(t *testing.T) {
	const note = "$KUBECONFIG is set"
	h := ctxHarness(t)
	h.env["KUBECONFIG"] = filepath.Join(t.TempDir(), "elsewhere")
	if e := h.mustRun(t, "contexts", "add", "alpha"); !strings.Contains(e, note) {
		t.Fatalf("no note: %s", e)
	}
	if e := h.mustRun(t, "contexts", "add", "beta"); strings.Contains(e, note) {
		t.Fatalf("note printed twice: %s", e)
	}
	for name, env := range map[string]func(h *harness) string{
		"lists the file":  func(h *harness) string { return "/nowhere" + string(os.PathListSeparator) + ctxFileOf(t, h) },
		"pinned terminal": func(h *harness) string { return filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml") },
		"unset":           func(*harness) string { return "" },
	} {
		h2 := ctxHarness(t)
		h2.env["KUBECONFIG"] = env(h2)
		if e := h2.mustRun(t, "use", "alpha", "--print-env"); strings.Contains(e, note) {
			t.Errorf("%s: unexpected note: %s", name, e)
		}
	}
}

// Finding J: the list shows the file only for records of another file.
func TestContextsListShowsOtherFile(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--print-env")
	_, out, _ := h.run("contexts")
	if strings.Contains(out, "kubeconfig") {
		t.Fatalf("file shown for the current one: %q", out)
	}
	other := filepath.Join(filepath.Dir(ctxFileOf(t, h)), "other-config")
	h.a.Paths = h.a.Paths.WithContextFile(other)
	_, out, _ = h.run("contexts")
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "kubeconfig") {
		t.Fatalf("file not shown: %q", out)
	}
	// The record does not apply to the other file: remove leaves it alone.
	if code, _, _ := h.run("contexts", "remove", "alpha"); code == 0 {
		t.Fatal("removed a record of another file")
	}
	if len(h.records(t)) != 1 {
		t.Fatal("record lost")
	}
}

// Finding M: logout --all clears kept contexts even when no entry is saved.
func TestLogoutAllWithNoEntriesRemovesContexts(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--print-env")
	h.mustRun(t, "logout", "--keep-contexts")
	if err := os.Remove(h.a.Paths.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if len(h.a.Paths.Root().Entries()) != 0 {
		t.Fatal("setup")
	}
	e := h.mustRun(t, "logout", "--all")
	if !strings.Contains(e, "removed 1 context") || len(h.records(t)) != 0 || len(h.contextNames(t)) != 0 {
		t.Fatalf("%s %v %v", e, h.records(t), h.contextNames(t))
	}
}

// Finding N: the hint for the default entry names it when another entry exists, and
// login --tenant default selects it.
func TestLoginDefaultWhileAnotherIsActive(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.a.Paths.SetActive("acme")
	h.loginSeam()
	if code, _, e := h.run("login", "--tenant", "default", "--no-browser"); code != 0 || !strings.Contains(e, "active tenant: default") {
		t.Fatalf("%d %s", code, e)
	}
}

// Finding O: an entry with the same org name on another server does not hide the org.
func TestAvailableComparesServerAndOrg(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	srv := tenantServer(t, "acme", uidAcme, "acme-web")
	cfg := h.namedCfg("acme")
	cfg.APIURL, cfg.Server = srv.URL, srv.URL
	cfg.Issuer = srv.URL + "/v1/oidc/tenant/" + uidAcme
	h.addEntry(t, "acme", cfg, "rt-acme", "acme")
	h.a.Paths.SetActive("acme")
	// globex exists, but on another server (another issuer host).
	h.addEntry(t, "globex", h.namedCfg("globex"), "rt-0", "globex")
	_, out, _ := h.run("tenants", "--available")
	if !strings.Contains(out, "--tenant globex") {
		t.Fatalf("globex on this server is hidden:\n%s", out)
	}
	// An entry on the same server still hides it.
	// It was logged in through its own org host, so only the issuer host matches.
	g := h.namedCfg("globex")
	g.Server = "https://globex.palette.example.com"
	g.Issuer = srv.URL + "/v1/oidc/tenant/" + uidGlobex
	h.addEntry(t, "globex", g, "rt-0", "globex")
	_, out, _ = h.run("tenants", "--available")
	if strings.Contains(out, "--tenant globex") {
		t.Fatalf("globex on the same server is shown:\n%s", out)
	}
}

// Finding P: issuers that differ only by a trailing slash are the same login.
func TestTrailingSlashIssuerIsTheSameLogin(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	if err := checkEntry(h.a.Paths.Root(), "acme", "https://i/acme/"); err != nil {
		t.Fatalf("checkEntry: %v", err)
	}
	if err := checkEntry(h.a.Paths.Root(), "acme", "https://i/other"); err == nil {
		t.Fatal("different issuer accepted")
	}
	// Legacy login with the same issuer plus a slash must not wipe the entry's files.
	h.mustRun(t, "use", "alpha", "--print-env")
	h.loginSeam()
	e := h.mustRun(t, "login", "--issuer", "https://i/", "--client-id", "T", "--no-browser")
	if strings.Contains(e, "issuer changed") {
		t.Fatalf("wiped: %s", e)
	}
}

func writeKube(t *testing.T, h *harness, cfg *api.Config) {
	t.Helper()
	if err := clientcmd.WriteToFile(*cfg, ctxFileOf(t, h)); err != nil {
		t.Fatal(err)
	}
}

// With an issuer pin, token prints nothing unless the entry's config can be read.
func TestTokenWithPinFailsClosedOnUnreadableConfig(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	cfg, err := h.a.Paths.Load()
	if err != nil {
		t.Fatal(err)
	}
	id := config.IssuerID(cfg.Issuer)
	if code, out, _ := h.run("token", "--issuer-id", id); code != 0 || !strings.Contains(out, "ExecCredential") {
		t.Fatalf("valid pin refused: %d %s", code, out)
	}
	if err := os.WriteFile(h.a.Paths.ConfigFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := h.run("token", "--issuer-id", id)
	if code == 0 || strings.Contains(out, "ExecCredential") {
		t.Fatalf("token printed with an unreadable config: %d %s", code, out)
	}
}
