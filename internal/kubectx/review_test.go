package kubectx

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

func (e env) upE(entry Entry, source []byte) (Record, error) {
	e.t.Helper()
	return Upsert(e.kc, e.reg, entry, source, t0)
}

func TestIssuerIDGrammarAndBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		tenant string
		id     string
		ok     bool
	}{
		{"tenant then id", []string{"token", "--tenant", "eu", "--issuer-id", "abc"}, "eu", "abc", true},
		{"id then tenant", []string{"token", "--issuer-id", "abc", "--tenant", "eu"}, "eu", "abc", true},
		{"equals forms", []string{"token", "--tenant=eu", "--issuer-id=abc"}, "eu", "abc", true},
		{"id only", []string{"token", "--issuer-id", "abc"}, "default", "abc", true},
		{"plain", []string{"token"}, "default", "", true},
		{"repeat", []string{"token", "--issuer-id", "a", "--issuer-id", "b"}, "", "", false},
		{"unknown", []string{"token", "--admin"}, "", "", false},
		{"missing value", []string{"token", "--tenant"}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &api.AuthInfo{Exec: &api.ExecConfig{Command: "kubectl-palette", Args: tc.args}}
			ten, id, err := isPaletteExec(u)
			if (err == nil) != tc.ok || (tc.ok && (ten != tc.tenant || id != tc.id)) {
				t.Fatalf("got %q %q %v", ten, id, err)
			}
		})
	}
	e := newEnv(t)
	good := src("https://a.example.test", "token", "--issuer-id", "abc")
	if _, err := e.upE(Entry{Project: "p", Cluster: "a", UID: "u1", IssuerID: "zzz"}, good); err == nil {
		t.Fatal("different issuer id must be refused")
	}
	if _, err := e.upE(Entry{Project: "p", Cluster: "a", UID: "u1", IssuerID: "abc"}, plain("https://a.example.test")); err == nil {
		t.Fatal("source without an issuer id must be refused when the entry has one")
	}
	r, err := e.upE(Entry{Project: "p", Cluster: "a", UID: "u1", IssuerID: "abc"}, good)
	if err != nil || r.IssuerID != "abc" {
		t.Fatalf("%+v %v", r, err)
	}
	if !reflect.DeepEqual(e.load().AuthInfos["palette"].Exec.Args, []string{"token", "--issuer-id", "abc"}) {
		t.Fatalf("args %v", e.load().AuthInfos["palette"].Exec.Args)
	}
}

// Finding B: a tenant named "2" must not adopt the user entry "palette-2" that belongs to the
// default tenant.
func TestUserEntryNotAdoptedAcrossTenants(t *testing.T) {
	e := newEnv(t)
	e.write("apiVersion: v1\nkind: Config\nusers:\n- name: palette\n  user: {token: mine}\n")
	d := e.up("", "p", "a", "u1", plain("https://a.example.test"))
	if d.User != "palette-2" {
		t.Fatalf("default user %q", d.User)
	}
	x := e.up("2", "p", "b", "u2", src("https://b.example.test", "token", "--tenant", "2"))
	if x.User == "palette-2" {
		t.Fatal("tenant 2 adopted the default tenant's user")
	}
	got := e.load()
	if !reflect.DeepEqual(got.AuthInfos["palette-2"].Exec.Args, []string{"token"}) {
		t.Fatalf("default user rewritten: %v", got.AuthInfos["palette-2"].Exec.Args)
	}
	if got.Contexts[d.Context].AuthInfo != "palette-2" {
		t.Fatal("default context re-pointed")
	}
	// Same tenant, other issuer: also not reusable.
	y := e.up("2", "p", "c", "u3", src("https://c.example.test", "token", "--tenant", "2", "--issuer-id", "other"))
	if y.User == x.User {
		t.Fatal("different issuer shares a user entry")
	}
}

// Finding C: a context the user re-pointed is not overwritten by the next upsert.
func TestUpsertDoesNotOverwriteRepointedContext(t *testing.T) {
	for _, stale := range []bool{false, true} {
		e := newEnv(t)
		a := e.up("", "p", "web", "u1", plain("https://a.example.test"))
		cfg := e.load()
		cfg.Clusters["mine"] = &api.Cluster{Server: "https://mine.example.test"}
		cfg.AuthInfos["mine"] = &api.AuthInfo{Token: "t"}
		cfg.Contexts[a.Context] = &api.Context{Cluster: "mine", AuthInfo: "mine", Namespace: "x"}
		if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
			t.Fatal(err)
		}
		uid := "u1"
		if stale {
			uid = "u2" // same project and cluster, new UID
		}
		b := e.up("", "p", "web", uid, plain("https://b.example.test"))
		if b.Context == a.Context {
			t.Fatalf("stale=%v reused the re-pointed name", stale)
		}
		got := e.load()
		if c := got.Contexts[a.Context]; c == nil || c.Cluster != "mine" || c.AuthInfo != "mine" || c.Namespace != "x" {
			t.Fatalf("user's context changed: %+v", c)
		}
		if got.Clusters[a.Context].Server != "https://a.example.test" {
			t.Fatal("cluster entry of the old name was overwritten")
		}
		recs, _ := List(e.reg)
		if len(recs) != 1 || recs[0].Context != b.Context {
			t.Fatalf("records %+v", recs)
		}
	}
}

// Finding F: a failed kubeconfig write restores the registry.
func TestKubeconfigWriteFailureRestoresRegistry(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "keep", "u0", plain("https://k.example.test"))
	before, _ := os.ReadFile(e.reg)
	old := saveKubeconfigFn
	saveKubeconfigFn = func(string, *api.Config) error { return errors.New("disk full") }
	defer func() { saveKubeconfigFn = old }()
	if _, err := e.upE(Entry{Project: "p", Cluster: "new", UID: "u9"}, plain("https://n.example.test")); err == nil {
		t.Fatal("expected error")
	}
	after, _ := os.ReadFile(e.reg)
	if string(before) != string(after) {
		t.Fatal("registry not restored")
	}
	// Registry did not exist before: it must not exist after.
	e2 := newEnv(t)
	if _, err := e2.upE(Entry{Project: "p", Cluster: "new", UID: "u9"}, plain("https://n.example.test")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(e2.reg); err == nil {
		t.Fatal("registry left behind")
	}
}

// Finding H: removal says whether entries were deleted or only the record dropped.
func TestRemoveDetailed(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "a", "u1", plain("https://a.example.test"))
	e.up("", "p", "b", "u2", plain("https://b.example.test"))
	cfg := e.load()
	cfg.Contexts["b"].AuthInfo = "mine"
	cfg.AuthInfos["mine"] = &api.AuthInfo{Token: "t"}
	if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
		t.Fatal(err)
	}
	res, err := RemoveDetailed(e.kc, e.reg, func(Record) bool { return true })
	if err != nil || len(res) != 2 {
		t.Fatalf("%v %v", res, err)
	}
	for _, r := range res {
		if want := r.Record.Context == "a"; r.EntriesDeleted != want {
			t.Fatalf("%s deleted=%v", r.Record.Context, r.EntriesDeleted)
		}
	}
}

// Finding J: a record applies only to the kubeconfig it was written to.
func TestRecordsAreBoundToTheirFile(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(filepath.Dir(e.kc), "other-config")
	a, err := Upsert(e.kc, e.reg, Entry{Project: "p", Cluster: "web", UID: "u1"}, plain("https://a.example.test"), t0)
	if err != nil {
		t.Fatal(err)
	}
	// A different file with a hand-made "web" entry: the record for e.kc must not claim it.
	if err := os.WriteFile(other, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: web\n  cluster: {server: \"https://mine.example.test\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Upsert(other, e.reg, Entry{Project: "p", Cluster: "web", UID: "u1"}, plain("https://b.example.test"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Context == "web" || a.Context != "web" {
		t.Fatalf("a=%q b=%q", a.Context, b.Context)
	}
	oc, _ := clientcmd.LoadFromFile(other)
	if oc.Clusters["web"].Server != "https://mine.example.test" {
		t.Fatal("foreign entry in the other file was overwritten")
	}
	all, _ := List(e.reg)
	mine, _ := ListFor(e.kc, e.reg)
	if len(all) != 2 || len(mine) != 1 || mine[0].Context != "web" {
		t.Fatalf("all=%d mine=%v", len(all), mine)
	}
	if _, ok, _ := Lookup(other, e.reg, "default", "u1"); !ok {
		t.Fatal("lookup in other file")
	}
	// Removing in e.kc leaves the other file's record and file alone.
	if _, err := Remove(e.kc, e.reg, func(Record) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if rest, _ := List(e.reg); len(rest) != 1 || rest[0].File == "" {
		t.Fatalf("rest %+v", rest)
	}
	oc2, _ := clientcmd.LoadFromFile(other)
	if oc2.Contexts[b.Context] == nil {
		t.Fatal("other file's context removed")
	}
}

// Finding K: a cluster entry another context still uses stays.
func TestRemoveKeepsClusterUsedByAnotherContext(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "web", "u1", plain("https://a.example.test"))
	cfg := e.load()
	cfg.Contexts["web-ns"] = &api.Context{Cluster: "web", AuthInfo: "palette", Namespace: "other"}
	if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(e.kc, e.reg, func(Record) bool { return true }); err != nil {
		t.Fatal(err)
	}
	got := e.load()
	if got.Clusters["web"] == nil || got.AuthInfos["palette"] == nil || got.Contexts["web-ns"] == nil {
		t.Fatalf("shared entries removed: %v %v", got.Clusters, got.AuthInfos)
	}
	if got.Contexts["web"] != nil {
		t.Fatal("our context still there")
	}
}
