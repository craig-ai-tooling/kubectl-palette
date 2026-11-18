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
