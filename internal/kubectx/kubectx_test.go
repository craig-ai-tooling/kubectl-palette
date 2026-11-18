package kubectx

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

type env struct {
	t       *testing.T
	kc, reg string
}

func newEnv(t *testing.T) env {
	d := t.TempDir()
	return env{t: t, kc: filepath.Join(d, "kube", "config"), reg: filepath.Join(d, "cfg", "contexts.json")}
}

func src(server string, args ...string) []byte {
	a := ""
	for _, x := range args {
		a += fmt.Sprintf("\n      - %s", x)
	}
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: c
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: Zm9v
    tls-server-name: api.example.test
contexts:
- name: c
  context: {cluster: c, user: u}
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: kubectl-palette
      args:%s
`, server, a))
}

func plain(server string) []byte { return src(server, "token") }

func (e env) up(tenant, project, cluster, uid string, source []byte) Record {
	e.t.Helper()
	r, err := Upsert(e.kc, e.reg, Entry{Tenant: tenant, Project: project, Cluster: cluster, UID: uid}, source, t0)
	if err != nil {
		e.t.Fatalf("Upsert: %v", err)
	}
	return r
}

func (e env) load() *api.Config {
	e.t.Helper()
	c, err := clientcmd.LoadFromFile(e.kc)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e env) write(s string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.kc), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.kc, []byte(s), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

const foreign = `apiVersion: v1
kind: Config
current-context: other
clusters:
- name: other
  cluster: {server: "https://other.example.test"}
users:
- name: other-user
  user: {token: abc}
contexts:
- name: other
  context: {cluster: other, user: other-user, namespace: kube-system}
`

func TestUpsertStartingStates(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		create        bool
	}{
		{"missing", "", false},
		{"empty", "", true},
		{"unrelated", foreign, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			var before *api.Config
			if tc.create {
				e.write(tc.content)
				before = e.load()
			}
			r := e.up("", "proj", "web", "uid-1", plain("https://web.example.test"))
			if r.Context != "web" || r.User != "palette" || r.Tenant != "default" {
				t.Fatalf("record %+v", r)
			}
			got := e.load()
			cl := got.Clusters["web"]
			if cl == nil || cl.Server != "https://web.example.test" || cl.TLSServerName != "api.example.test" || string(cl.CertificateAuthorityData) != "foo" {
				t.Fatalf("cluster %+v", cl)
			}
			u := got.AuthInfos["palette"]
			if u == nil || u.Exec == nil || u.Exec.Command != "kubectl-palette" || !reflect.DeepEqual(u.Exec.Args, []string{"token"}) {
				t.Fatalf("user %+v", u)
			}
			if c := got.Contexts["web"]; c == nil || c.Cluster != "web" || c.AuthInfo != "palette" {
				t.Fatalf("context %+v", c)
			}
			if before != nil {
				if got.CurrentContext != before.CurrentContext {
					t.Fatalf("current-context changed to %q", got.CurrentContext)
				}
				for n, c := range before.Clusters {
					if !reflect.DeepEqual(c, got.Clusters[n]) {
						t.Fatalf("cluster %s changed", n)
					}
				}
				for n, c := range before.AuthInfos {
					if !reflect.DeepEqual(c, got.AuthInfos[n]) {
						t.Fatalf("user %s changed", n)
					}
				}
				for n, c := range before.Contexts {
					if !reflect.DeepEqual(c, got.Contexts[n]) {
						t.Fatalf("context %s changed", n)
					}
				}
			} else if got.CurrentContext != "" {
				t.Fatalf("current-context set to %q", got.CurrentContext)
			}
		})
	}
}

func TestIdempotent(t *testing.T) {
	e := newEnv(t)
	a := e.up("", "p", "web", "u1", plain("https://a.example.test"))
	b := e.up("", "p", "web", "u1", plain("https://b.example.test"))
	if a.Context != b.Context || a.User != b.User {
		t.Fatalf("names changed: %+v %+v", a, b)
	}
	recs, _ := List(e.reg)
	if len(recs) != 1 {
		t.Fatalf("records %d", len(recs))
	}
	if e.load().Clusters["web"].Server != "https://b.example.test" {
		t.Fatal("cluster not refreshed")
	}
}

func TestNameFallbackTenant(t *testing.T) {
	cases := []struct{ name, kube, want string }{
		{"plain", "", "web"},
		{"context taken", "apiVersion: v1\nkind: Config\ncontexts:\n- name: web\n  context: {cluster: x, user: x}\n", "p/web"},
		{"cluster entry taken", "apiVersion: v1\nkind: Config\nclusters:\n- name: web\n  cluster: {server: \"https://x.example.test\"}\n", "p/web"},
		{"two taken", "apiVersion: v1\nkind: Config\nclusters:\n- name: web\n  cluster: {server: \"https://x.example.test\"}\ncontexts:\n- name: p/web\n  context: {cluster: x, user: x}\n", "t/p/web"},
		{"three taken", "apiVersion: v1\nkind: Config\ncontexts:\n- name: web\n  context: {cluster: x, user: x}\n- name: p/web\n  context: {cluster: x, user: x}\n- name: t/p/web\n  context: {cluster: x, user: x}\n", "t/p/web-2"},
		{"four taken", "apiVersion: v1\nkind: Config\ncontexts:\n- name: web\n  context: {cluster: x, user: x}\n- name: p/web\n  context: {cluster: x, user: x}\n- name: t/p/web\n  context: {cluster: x, user: x}\n- name: t/p/web-2\n  context: {cluster: x, user: x}\n", "t/p/web-3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.kube != "" {
				e.write(tc.kube)
			}
			r := e.up("t", "p", "web", "u1", src("https://a.example.test", "token", "--tenant", "t"))
			if r.Context != tc.want {
				t.Fatalf("got %q want %q", r.Context, tc.want)
			}
			if again := e.up("t", "p", "web", "u1", src("https://a.example.test", "token", "--tenant", "t")); again.Context != tc.want {
				t.Fatalf("second upsert gave %q", again.Context)
			}
		})
	}
}

func TestSecondClusterSameNameGetsNextForm(t *testing.T) {
	e := newEnv(t)
	a := e.up("", "p1", "web", "u1", plain("https://a.example.test"))
	b := e.up("", "p2", "web", "u2", plain("https://b.example.test"))
	if a.Context != "web" || b.Context != "p2/web" {
		t.Fatalf("%q %q", a.Context, b.Context)
	}
}

func TestSameNameNewUIDReplaces(t *testing.T) {
	e := newEnv(t)
	a := e.up("", "p", "web", "u1", plain("https://old.example.test"))
	b := e.up("", "p", "web", "u2", plain("https://new.example.test"))
	if a.Context != b.Context {
		t.Fatalf("names differ %q %q", a.Context, b.Context)
	}
	recs, _ := List(e.reg)
	if len(recs) != 1 || recs[0].UID != "u2" {
		t.Fatalf("records %+v", recs)
	}
	if e.load().Clusters["web"].Server != "https://new.example.test" {
		t.Fatal("server not updated")
	}
}

func TestRenamedClusterKeepsContext(t *testing.T) {
	e := newEnv(t)
	a := e.up("", "p", "web", "u1", plain("https://a.example.test"))
	b := e.up("", "p", "web-renamed", "u1", plain("https://a.example.test"))
	if a.Context != b.Context || b.Cluster != "web-renamed" {
		t.Fatalf("%+v %+v", a, b)
	}
	recs, _ := List(e.reg)
	if len(recs) != 1 || recs[0].Cluster != "web-renamed" {
		t.Fatalf("records %+v", recs)
	}
}

func TestNamespaceSurvives(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "web", "u1", plain("https://a.example.test"))
	cfg := e.load()
	cfg.Contexts["web"].Namespace = "payments"
	if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
		t.Fatal(err)
	}
	e.up("", "p", "web", "u1", plain("https://a.example.test"))
	if ns := e.load().Contexts["web"].Namespace; ns != "payments" {
		t.Fatalf("namespace %q", ns)
	}
}

func TestTwoTenants(t *testing.T) {
	e := newEnv(t)
	a := e.up("", "p", "a", "u1", plain("https://a.example.test"))
	b := e.up("eu", "p", "b", "u2", src("https://b.example.test", "token", "--tenant", "eu"))
	c := e.up("eu", "p", "c", "u3", src("https://c.example.test", "token", "--tenant", "eu"))
	if a.User != "palette" || b.User != "palette-eu" || c.User != "palette-eu" {
		t.Fatalf("users %q %q %q", a.User, b.User, c.User)
	}
	got := e.load()
	if len(got.AuthInfos) != 2 {
		t.Fatalf("users %d", len(got.AuthInfos))
	}
	if !reflect.DeepEqual(got.AuthInfos["palette-eu"].Exec.Args, []string{"token", "--tenant", "eu"}) {
		t.Fatalf("args %v", got.AuthInfos["palette-eu"].Exec.Args)
	}
}

func TestRefusals(t *testing.T) {
	mk := func(userYAML string) []byte {
		return []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster: {server: \"https://s.example.test\"}\ncontexts:\n- name: c\n  context: {cluster: c, user: u}\nusers:\n- name: u\n  user:\n" + userYAML)
	}
	cases := map[string][]byte{
		"client cert":                mk("    client-certificate-data: Zm9v\n    client-key-data: YmFy\n"),
		"static token":               mk("    token: abc\n"),
		"password":                   mk("    username: a\n    password: b\n"),
		"cert plus exec":             mk("    client-certificate-data: Zm9v\n    exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: kubectl-palette, args: [token]}\n"),
		"other command":              mk("    exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: aws, args: [token]}\n"),
		"other first arg":            mk("    exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: kubectl-palette, args: [admin]}\n"),
		"exec env":                   mk("    exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: kubectl-palette, args: [token], env: [{name: A, value: b}]}\n"),
		"extra arg":                  mk("    exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: kubectl-palette, args: [token, --admin]}\n"),
		"tenant mismatch":            src("https://s.example.test", "token", "--tenant", "other"),
		"no flag, entry not default": plain("https://s.example.test"),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.write(foreign)
			e.up("", "p", "keep", "u0", plain("https://keep.example.test"))
			kb, _ := os.ReadFile(e.kc)
			rb, _ := os.ReadFile(e.reg)
			tenant := ""
			if name == "no flag, entry not default" {
				tenant = "eu"
			}
			if _, err := Upsert(e.kc, e.reg, Entry{Tenant: tenant, Project: "p", Cluster: "x", UID: "u9"}, source, t0); err == nil {
				t.Fatal("expected refusal")
			}
			ka, _ := os.ReadFile(e.kc)
			ra, _ := os.ReadFile(e.reg)
			if string(kb) != string(ka) || string(rb) != string(ra) {
				t.Fatal("files changed after refusal")
			}
		})
	}
	t.Run("matching tenant flag is accepted", func(t *testing.T) {
		e := newEnv(t)
		e.up("eu", "p", "x", "u1", src("https://s.example.test", "token", "--tenant=eu"))
	})
}

func TestForeignUserNotOverwritten(t *testing.T) {
	e := newEnv(t)
	e.write("apiVersion: v1\nkind: Config\nusers:\n- name: palette\n  user: {token: mine}\n")
	r := e.up("", "p", "web", "u1", plain("https://a.example.test"))
	if r.User != "palette-2" {
		t.Fatalf("user %q", r.User)
	}
	got := e.load()
	if got.AuthInfos["palette"].Token != "mine" {
		t.Fatal("foreign user overwritten")
	}
	if r2 := e.up("", "p", "db", "u2", plain("https://b.example.test")); r2.User != "palette-2" {
		t.Fatalf("second cluster user %q", r2.User)
	}
}

func TestRemove(t *testing.T) {
	e := newEnv(t)
	e.write(foreign)
	a := e.up("", "p", "a", "u1", plain("https://a.example.test"))
	b := e.up("", "p", "b", "u2", plain("https://b.example.test"))
	cfg := e.load()
	cfg.CurrentContext = a.Context
	if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove(e.kc, e.reg, func(r Record) bool { return r.UID == "u1" })
	if err != nil || len(removed) != 1 {
		t.Fatalf("%v %v", removed, err)
	}
	got := e.load()
	if got.Contexts["a"] != nil || got.Clusters["a"] != nil {
		t.Fatal("a still present")
	}
	if got.Contexts["b"] == nil || got.AuthInfos["palette"] == nil {
		t.Fatal("b or shared user removed")
	}
	if got.CurrentContext != "" {
		t.Fatalf("current-context %q", got.CurrentContext)
	}
	if got.Contexts["other"] == nil || got.AuthInfos["other-user"] == nil {
		t.Fatal("foreign entries removed")
	}
	if _, err := Remove(e.kc, e.reg, func(r Record) bool { return r.UID == b.UID }); err != nil {
		t.Fatal(err)
	}
	got = e.load()
	if got.AuthInfos["palette"] != nil || len(got.Contexts) != 1 {
		t.Fatalf("user or contexts left: %v %v", got.AuthInfos, got.Contexts)
	}
	if recs, _ := List(e.reg); len(recs) != 0 {
		t.Fatalf("records %v", recs)
	}
}

func TestRemoveLeavesHandEditedAlone(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "a", "u1", plain("https://a.example.test"))
	cfg := e.load()
	cfg.Contexts["a"].AuthInfo = "mine"
	cfg.AuthInfos["mine"] = &api.AuthInfo{Token: "t"}
	cfg.CurrentContext = "a"
	if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove(e.kc, e.reg, func(Record) bool { return true })
	if err != nil || len(removed) != 1 {
		t.Fatalf("%v %v", removed, err)
	}
	got := e.load()
	if got.Contexts["a"] == nil || got.Clusters["a"] == nil || got.AuthInfos["mine"] == nil || got.CurrentContext != "a" {
		t.Fatal("hand-edited context was touched")
	}
	if got.AuthInfos["palette"] != nil {
		t.Fatal("orphan palette user left")
	}
	if recs, _ := List(e.reg); len(recs) != 0 {
		t.Fatal("record not dropped")
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "a", "u1", plain("https://a.example.test"))
	e.up("", "p", "b", "u2", plain("https://b.example.test"))
	e.up("", "p", "c", "u3", plain("https://c.example.test"))
	cfg := e.load()
	delete(cfg.Contexts, "c")
	if err := clientcmd.WriteToFile(*cfg, e.kc); err != nil {
		t.Fatal(err)
	}
	removed, err := Prune(e.kc, e.reg, func(r Record) bool { return r.UID != "u1" })
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	recs, _ := List(e.reg)
	if len(recs) != 1 || recs[0].UID != "u2" {
		t.Fatalf("records %v", recs)
	}
	got := e.load()
	if got.Contexts["a"] != nil || got.Contexts["b"] == nil {
		t.Fatal("wrong contexts left")
	}
}

func TestUnparseableKubeconfig(t *testing.T) {
	e := newEnv(t)
	e.write("clusters: [this is: not valid yaml\n  - {")
	before, _ := os.ReadFile(e.kc)
	if _, err := Upsert(e.kc, e.reg, Entry{Project: "p", Cluster: "a", UID: "u"}, plain("https://a.example.test"), t0); err == nil {
		t.Fatal("Upsert should fail")
	}
	if _, err := Remove(e.kc, e.reg, func(Record) bool { return true }); err != nil {
		// registry is empty so nothing to do; either outcome must leave the file alone
		_ = err
	}
	if _, err := Prune(e.kc, e.reg, func(Record) bool { return false }); err != nil {
		_ = err
	}
	after, _ := os.ReadFile(e.kc)
	if string(before) != string(after) {
		t.Fatal("kubeconfig was modified")
	}
	if _, err := os.Stat(e.reg); err == nil {
		t.Fatal("registry written despite failure")
	}
}

func TestUnparseableKubeconfigWithRecords(t *testing.T) {
	e := newEnv(t)
	e.up("", "p", "a", "u1", plain("https://a.example.test"))
	e.write("{ not yaml")
	before, _ := os.ReadFile(e.kc)
	rb, _ := os.ReadFile(e.reg)
	if _, err := Remove(e.kc, e.reg, func(Record) bool { return true }); err == nil {
		t.Fatal("Remove should fail")
	}
	if _, err := Prune(e.kc, e.reg, func(Record) bool { return false }); err == nil {
		t.Fatal("Prune should fail")
	}
	after, _ := os.ReadFile(e.kc)
	ra, _ := os.ReadFile(e.reg)
	if string(before) != string(after) || string(rb) != string(ra) {
		t.Fatal("files modified")
	}
}

func TestListAndLookup(t *testing.T) {
	e := newEnv(t)
	if recs, err := List(e.reg); err != nil || len(recs) != 0 {
		t.Fatalf("%v %v", recs, err)
	}
	e.up("", "p", "zeta", "u1", plain("https://a.example.test"))
	e.up("", "p", "alpha", "u2", plain("https://b.example.test"))
	recs, _ := List(e.reg)
	if len(recs) != 2 || recs[0].Context != "alpha" {
		t.Fatalf("%v", recs)
	}
	r, ok, err := Lookup(e.reg, "default", "u1")
	if err != nil || !ok || r.Cluster != "zeta" {
		t.Fatalf("%v %v %v", r, ok, err)
	}
	if _, ok, _ := Lookup(e.reg, "default", "nope"); ok {
		t.Fatal("found missing uid")
	}
	if _, ok, _ := Lookup(e.reg, "eu", "u1"); ok {
		t.Fatal("tenant ignored")
	}
}

func TestFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	e := newEnv(t)
	e.up("", "p", "a", "u1", plain("https://a.example.test"))
	for _, p := range []string{e.kc, e.reg} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", p, st.Mode().Perm())
		}
	}
}

func TestConcurrentUpserts(t *testing.T) {
	e := newEnv(t)
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Upsert(e.kc, e.reg, Entry{Project: "p", Cluster: fmt.Sprintf("c%d", i), UID: fmt.Sprintf("u%d", i)}, plain("https://x.example.test"), t0)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := e.load()
	recs, _ := List(e.reg)
	if len(recs) != n || len(got.Contexts) != n || len(got.Clusters) != n {
		t.Fatalf("records %d contexts %d clusters %d", len(recs), len(got.Contexts), len(got.Clusters))
	}
}
