package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"payments-prod", true}, {"My Project", true}, {"a.b", true}, {"", false}, {".", false}, {"..", false},
		{"a/b", false}, {"..\\x", false}, {"a\x00b", false}, {"../etc", false},
	} {
		if err := SafeName(tc.in); (err == nil) != tc.ok {
			t.Errorf("SafeName(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestClusterFile(t *testing.T) {
	p := Paths{Home: "/h", KubeDir: "/k"}
	got, err := p.ClusterFile("proj", "c1", false)
	if err != nil || got != "/k/proj/c1.yaml" {
		t.Fatalf("got %q %v", got, err)
	}
	got, _ = p.ClusterFile("proj", "c1", true)
	if got != "/k/proj/.admin/c1.yaml" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range [][2]string{{"..", "c"}, {"p", ".."}, {"p/q", "c"}, {"p", "../../x"}, {"", "c"}} {
		if _, err := p.ClusterFile(bad[0], bad[1], false); err == nil {
			t.Errorf("ClusterFile(%q,%q) should fail", bad[0], bad[1])
		}
	}
}

// A cluster named "foo.admin" must not share a path with the admin file of "foo".
func TestAdminAndOIDCPathsNeverCollide(t *testing.T) {
	p := Paths{Home: "/h", KubeDir: "/k"}
	for _, names := range [][2]string{{"foo", "foo.admin"}, {"foo", "foo"}, {"x.admin", "x"}} {
		a, err1 := p.ClusterFile("proj", names[0], true)
		o, err2 := p.ClusterFile("proj", names[1], false)
		if err1 != nil || err2 != nil || a == o {
			t.Errorf("admin(%q)=%q oidc(%q)=%q %v %v", names[0], a, names[1], o, err1, err2)
		}
	}
	// The reserved directory name cannot be a project or a cluster.
	if _, err := p.ClusterFile(".admin", "c", false); err == nil {
		t.Error("project .admin accepted")
	}
	for _, admin := range []bool{false, true} {
		if _, err := p.ClusterFile("proj", ".admin", admin); err == nil {
			t.Errorf("cluster .admin accepted (admin=%v)", admin)
		}
	}
}

func TestCachedUIDSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p", "c.yaml")
	if got := CachedUID(path); got != "" {
		t.Fatalf("no sidecar must read empty, got %q", got)
	}
	if err := WriteFileAtomic(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCachedUID(path, "uid-1"); err != nil {
		t.Fatal(err)
	}
	if got := CachedUID(path); got != "uid-1" {
		t.Fatalf("got %q", got)
	}
	if err := RemoveCluster(path); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + UIDSuffix} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s survived RemoveCluster", f)
		}
	}
}

func TestWriteFileAtomicModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	path := filepath.Join(dir, "f")
	if err := WriteFileAtomic(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("y")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}

func TestConfigRoundTripAndEnv(t *testing.T) {
	h := t.TempDir()
	p, err := DefaultPaths(func(k string) string {
		return map[string]string{EnvHome: h, EnvKubeDir: h + "/k"}[k]
	})
	if err != nil || p.Home != h {
		t.Fatal(p, err)
	}
	if _, err := p.Load(); err != ErrNoConfig {
		t.Fatalf("err = %v", err)
	}
	c := Config{Issuer: "i", ClientID: "c", ClientSecret: "s", APIURL: "a"}
	if err := p.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := p.Load()
	if err != nil || got != c {
		t.Fatalf("%+v %v", got, err)
	}
	if err := (Config{}).Validate(); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidEntryName(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"acme", true}, {"acme-2.prod_x", true}, {"Acme", false}, {"ACME", false}, {"acme.", false}, {"acme..", false}, {"a", true}, {strings.Repeat("a", 63), true},
		{"default", false}, {"", false}, {".hidden", false}, {"-x", false}, {"a/b", false}, {"..", false},
		{"a b", false}, {strings.Repeat("a", 64), false}, {".tenants", false}, {"a\x00b", false},
	} {
		if err := ValidEntryName(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidEntryName(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestForTenantLayout(t *testing.T) {
	root := Paths{Home: "/h", KubeDir: "/k"}
	def := root.ForTenant("default")
	if def.Home != "/h" || def.KubeDir != "/k" || def.Entry() != "default" || def.SessionFile() != "/h/session.json" {
		t.Fatalf("%+v", def)
	}
	if got := root.ForTenant("").ConfigFile(); got != "/h/config.json" {
		t.Fatal(got)
	}
	acme := root.ForTenant("acme")
	if acme.Home != "/h/tenants/acme" || acme.KubeDir != "/k/.tenants/acme" || acme.Entry() != "acme" {
		t.Fatalf("%+v", acme)
	}
	if acme.IndexFile() != "/h/tenants/acme/index.json" || acme.SessionLock() != "/h/tenants/acme/session.lock" {
		t.Fatal("entry files are not under the entry directory")
	}
	// Selecting from an already-selected Paths starts from the root again.
	if got := acme.ForTenant("globex"); got.Home != "/h/tenants/globex" || got.Root().Home != "/h" {
		t.Fatalf("%+v", got)
	}
	if acme.ActiveFile() != "/h/active" {
		t.Fatal(acme.ActiveFile())
	}
	got, err := acme.ClusterFile("proj", "c1", true)
	if err != nil || got != "/k/.tenants/acme/proj/.admin/c1.yaml" {
		t.Fatalf("%q %v", got, err)
	}
	// A project cannot be named like the reserved directory, so the layouts never collide.
	if _, err := root.ClusterFile(".tenants", "c", false); err == nil {
		t.Error("project .tenants accepted")
	}
}

func TestEntriesAndActive(t *testing.T) {
	root := Paths{Home: t.TempDir(), KubeDir: t.TempDir()}
	if got := root.Entries(); len(got) != 0 {
		t.Fatal(got)
	}
	c := Config{Issuer: "i", ClientID: "c", APIURL: "a"}
	for _, n := range []string{"globex", "default", "acme"} {
		if err := root.ForTenant(n).Save(c); err != nil {
			t.Fatal(err)
		}
	}
	// A directory with no config is not an entry.
	os.MkdirAll(filepath.Join(root.Home, TenantsDir, "empty"), 0o700)
	if got := strings.Join(root.Entries(), ","); got != "default,acme,globex" {
		t.Fatal(got)
	}
	if root.Active() != "" {
		t.Fatal("active before set")
	}
	if err := root.SetActive("acme"); err != nil || root.Active() != "acme" {
		t.Fatal(err)
	}
	if err := root.ForTenant("globex").ClearActive(); err != nil || root.Active() != "" {
		t.Fatal("ClearActive")
	}
	if err := root.ClearActive(); err != nil {
		t.Fatal("clearing twice must not fail:", err)
	}
	if err := root.RemoveEntry("acme"); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveEntry("default"); err == nil {
		t.Fatal("RemoveEntry must refuse the default entry")
	}
	if got := strings.Join(root.Entries(), ","); got != "default,globex" {
		t.Fatal(got)
	}
}

func TestConfigNewFieldsRoundTripAndOmitEmpty(t *testing.T) {
	p := Paths{Home: t.TempDir()}
	c := Config{Issuer: "i", ClientID: "c", APIURL: "a", Org: "acme", Server: "https://s", CAFile: "/ca.pem"}
	if err := p.Save(c); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Load(); err != nil || got != c {
		t.Fatalf("%+v %v", got, err)
	}
	if err := p.Save(Config{Issuer: "i", ClientID: "c", APIURL: "a"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.ConfigFile())
	for _, k := range []string{"org", "server", "ca_file"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s must be omitted when empty:\n%s", k, b)
		}
	}
}

func TestContextFileResolution(t *testing.T) {
	h := t.TempDir()
	get := func(env map[string]string) func(string) string {
		return func(k string) string { return env[k] }
	}
	// A redirected kube dir keeps the context file inside it.
	p, err := DefaultPaths(get(map[string]string{EnvHome: h, EnvKubeDir: h + "/k"}))
	if err != nil || p.ContextFile() != h+"/k/contexts.yaml" {
		t.Fatalf("redirected: %q %v", p.ContextFile(), err)
	}
	// An explicit file wins.
	p, err = DefaultPaths(get(map[string]string{EnvHome: h, EnvKubeDir: h + "/k", EnvContextFile: h + "/x.yaml"}))
	if err != nil || p.ContextFile() != h+"/x.yaml" {
		t.Fatalf("explicit: %q %v", p.ContextFile(), err)
	}
	// Nothing set: the user's kubeconfig.
	p, err = DefaultPaths(get(nil))
	if err != nil || !strings.HasSuffix(filepath.ToSlash(p.ContextFile()), "/.kube/config") {
		t.Fatalf("default: %q %v", p.ContextFile(), err)
	}
	// The file survives Root and ForTenant, and the registry is shared by every entry.
	q := p.ForTenant("acme")
	if q.ContextFile() != p.ContextFile() || q.Root().ContextFile() != p.ContextFile() {
		t.Fatalf("context file lost: %q", q.ContextFile())
	}
	if q.ContextRegistry() != p.ContextRegistry() || filepath.Base(p.ContextRegistry()) != "contexts.json" {
		t.Fatalf("registry %q vs %q", q.ContextRegistry(), p.ContextRegistry())
	}
	// A hand-built Paths has no context file.
	if (Paths{Home: h, KubeDir: h}).ContextFile() != "" {
		t.Fatal("hand-built Paths must not name a context file")
	}
}

func TestIssuerID(t *testing.T) {
	a := IssuerID("https://p.example.test/v1/oidc/tenant/abc")
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("id %q", a)
	}
	if a != IssuerID("https://p.example.test/v1/oidc/tenant/abc/") || a != IssuerID("https://p.example.test/v1/oidc/tenant/abc//") {
		t.Fatal("trailing slashes must not change the id")
	}
	if a == IssuerID("https://p.example.test/v1/oidc/tenant/abd") {
		t.Fatal("different issuers share an id")
	}
}

func TestMatchEntry(t *testing.T) {
	entries := []string{"default", "acme"}
	for in, want := range map[string]string{"acme": "acme", "ACME": "acme", "Default": "default"} {
		if got, ok := MatchEntry(entries, in); !ok || got != want {
			t.Errorf("%q -> %q %v", in, got, ok)
		}
	}
	if _, ok := MatchEntry(entries, "other"); ok {
		t.Error("matched a missing entry")
	}
}
