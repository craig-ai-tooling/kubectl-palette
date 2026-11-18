package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
	"github.com/craig-ai-tooling/kubectl-palette/internal/oidc"
	"github.com/craig-ai-tooling/kubectl-palette/internal/picker"
	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// ctxHarness is a harness whose context file is checked to sit in the test directory.
func ctxHarness(t *testing.T) *harness {
	h := newHarness(t, time.Now().Add(time.Hour))
	ctxFileOf(t, h)
	return h
}

func (h *harness) kube(t *testing.T) *api.Config {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(ctxFileOf(t, h))
	if err != nil {
		if os.IsNotExist(err) {
			return api.NewConfig()
		}
		t.Fatal(err)
	}
	return cfg
}

func (h *harness) contextNames(t *testing.T) []string {
	var out []string
	for n := range h.kube(t).Contexts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// runExec runs an exec command. The harness's fake kubectl exits 7.
func (h *harness) runExec(t *testing.T, args ...string) {
	t.Helper()
	if code, _, e := h.run(args...); code != 7 {
		t.Fatalf("%v: exit %d: %s", args, code, e)
	}
}

func (h *harness) records(t *testing.T) []kubectx.Record {
	t.Helper()
	recs, err := kubectx.List(h.a.Paths.ContextRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func (h *harness) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, _, e := h.run(args...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, e)
	}
	return e
}

func clusterJSON(i int) string {
	return fmt.Sprintf(`{"name":"c%02d","uid":"uc%02d","annotations":{"projectUid":"p1","permissions":"cluster.get"}}`, i, i)
}

func manyClusters(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, clusterJSON(i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// loginSeam makes `login --no-browser` succeed with the harness's refresh token.
func (h *harness) loginSeam() {
	h.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
		exp := time.Now().Add(time.Hour)
		return session.Session{IDToken: jwtExp(exp, "x"), RefreshToken: "rt-0", IDExpiry: exp}, o.Cfg, nil
	}
}

func TestContextFileStaysInTestDir(t *testing.T) {
	h := ctxHarness(t)
	if !strings.HasPrefix(h.a.Paths.ContextRegistry(), h.a.Paths.Home) {
		t.Fatalf("registry %q", h.a.Paths.ContextRegistry())
	}
	// An App with a hand-built Paths has no context file, so it cannot reach ~/.kube/config.
	a := &App{Paths: config.Paths{Home: t.TempDir(), KubeDir: t.TempDir()}}
	if a.contextsEnabled() {
		t.Fatal("contexts must be off without a context file")
	}
}

func TestUseRecordsContextOnce(t *testing.T) {
	h := ctxHarness(t)
	code, _, e := h.run("use", "alpha", "--print-env")
	if code != 0 || strings.Count(e, "added context alpha to ") != 1 {
		t.Fatalf("%d %s", code, e)
	}
	cfg := h.kube(t)
	c := cfg.Contexts["alpha"]
	if c == nil || cfg.Clusters["alpha"] == nil || cfg.Clusters["alpha"].Server != "https://10.0.0.1:6443" {
		t.Fatalf("context not written: %+v", cfg)
	}
	u := cfg.AuthInfos[c.AuthInfo]
	if u == nil || u.Exec == nil || u.Exec.Command != "kubectl-palette" || !reflect.DeepEqual(u.Exec.Args, []string{"token", "--issuer-id", config.IssuerID("https://i")}) {
		t.Fatalf("user: %+v", u)
	}
	if u.Token != "" || len(u.ClientCertificateData) > 0 || cfg.CurrentContext != "" {
		t.Fatalf("user holds a credential or current-context changed: %+v %q", u, cfg.CurrentContext)
	}
	if len(h.records(t)) != 1 {
		t.Fatalf("records: %+v", h.records(t))
	}

	before, _ := os.Stat(ctxFileOf(t, h))
	regBefore, _ := os.Stat(h.a.Paths.ContextRegistry())
	body, _ := os.ReadFile(ctxFileOf(t, h))
	time.Sleep(20 * time.Millisecond)
	code, _, e = h.run("use", "alpha", "--print-env")
	if code != 0 || strings.Contains(e, "added context") {
		t.Fatalf("second use: %d %s", code, e)
	}
	after, _ := os.Stat(ctxFileOf(t, h))
	regAfter, _ := os.Stat(h.a.Paths.ContextRegistry())
	body2, _ := os.ReadFile(ctxFileOf(t, h))
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || string(body) != string(body2) {
		t.Fatal("a warm use rewrote the context file")
	}
	if !os.SameFile(regBefore, regAfter) || !regBefore.ModTime().Equal(regAfter.ModTime()) {
		t.Fatal("a warm use rewrote the registry")
	}
}

func TestUseNoContextFlagAndEnvSwitch(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--no-context", "--print-env")
	if _, err := os.Stat(ctxFileOf(t, h)); err == nil {
		t.Fatal("--no-context wrote the context file")
	}
	h.env[config.EnvNoContexts] = "1"
	h.mustRun(t, "use", "beta", "--print-env")
	h.runExec(t, "exec", "beta", "--", "get", "ns")
	if _, err := os.Stat(ctxFileOf(t, h)); err == nil {
		t.Fatal("the env switch did not turn contexts off")
	}
	if code, _, e := h.run("contexts", "add", "alpha"); code == 0 || !strings.Contains(e, "turned off") {
		t.Fatalf("contexts add while off: %d %s", code, e)
	}
	delete(h.env, config.EnvNoContexts)
	// A cluster fetched while contexts were off is recorded by the next use: it has no record.
	h.mustRun(t, "use", "beta", "--print-env")
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("contexts %v", got)
	}
}

func TestAdminNeverRecordsAndNeverLeaksACertificate(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--admin", "--print-env")
	if _, err := os.Stat(ctxFileOf(t, h)); err == nil {
		t.Fatal("use --admin created the context file")
	}
	h.mustRun(t, "use", "alpha", "--print-env")
	h.mustRun(t, "use", "alpha", "--admin", "--print-env")
	h.runExec(t, "exec", "alpha", "--admin", "--", "get", "ns")
	b, _ := os.ReadFile(ctxFileOf(t, h))
	for _, bad := range []string{"client-certificate", "client-key", "Q0VSVA", "TOPSECRET", "oidc-login"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("context file contains %q", bad)
		}
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("contexts %v", got)
	}
}

func TestExecRecordsContext(t *testing.T) {
	h := ctxHarness(t)
	code, _, e := h.run("exec", "beta", "--", "get", "ns")
	if code != 7 || !strings.Contains(e, "added context beta to ") {
		t.Fatalf("%d %s", code, e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("contexts %v", got)
	}
	h.runExec(t, "exec", "plain", "--no-context", "--", "get", "ns")
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("--no-context recorded: %v", got)
	}
}

func TestRecordFailureIsOnlyAWarning(t *testing.T) {
	h := ctxHarness(t)
	if err := os.WriteFile(ctxFileOf(t, h), []byte("{{ not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, e := h.run("use", "alpha", "--print-env")
	if code != 0 || !strings.Contains(out, "alpha.yaml") || !strings.Contains(e, "warning: could not add a context") {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if b, _ := os.ReadFile(ctxFileOf(t, h)); string(b) != "{{ not yaml" {
		t.Fatal("an unparseable kubeconfig was overwritten")
	}
}

func TestLoginAutoAddsContexts(t *testing.T) {
	h := ctxHarness(t)
	h.loginSeam()
	e := h.mustRun(t, "login", "--no-browser")
	if !strings.Contains(e, "added 3 contexts to ") {
		t.Fatalf("summary: %s", e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"alpha", "beta", "plain"}) {
		t.Fatalf("contexts %v", got)
	}
	for _, uid := range []string{"ua", "ub", "un"} {
		if kubeconfigHits(h, uid) != 1 {
			t.Errorf("%s fetched %d times", uid, kubeconfigHits(h, uid))
		}
	}
	// Logging in again adds nothing and fetches nothing: the files are cached.
	e = h.mustRun(t, "login", "--no-browser")
	if !strings.Contains(e, "no new contexts") || !strings.Contains(e, "3 already present") || kubeconfigHits(h, "ua") != 1 {
		t.Fatalf("second login: %s", e)
	}
}

func TestLoginContextLimit(t *testing.T) {
	h := ctxHarness(t)
	h.loginSeam()
	h.setClusters(manyClusters(maxAutoContexts + 1))
	e := h.mustRun(t, "login", "--no-browser")
	want := fmt.Sprintf("%d clusters: contexts are added as you use them, or run: kubectl palette contexts add --all", maxAutoContexts+1)
	if !strings.Contains(e, want) {
		t.Fatalf("no hint: %s", e)
	}
	if _, err := os.Stat(ctxFileOf(t, h)); err == nil {
		t.Fatal("login above the limit wrote contexts")
	}
	if h.f.count("/v1/spectroclusters/uc00/assets/kubeconfigclient") != 0 {
		t.Fatal("login above the limit fetched kubeconfigs")
	}
	// Exactly at the limit everything is added.
	h.setClusters(manyClusters(maxAutoContexts))
	e = h.mustRun(t, "login", "--no-browser")
	if !strings.Contains(e, fmt.Sprintf("added %d contexts", maxAutoContexts)) || len(h.contextNames(t)) != maxAutoContexts {
		t.Fatalf("at the limit: %s", e)
	}
}

func TestLoginNoContextsFlag(t *testing.T) {
	h := ctxHarness(t)
	h.loginSeam()
	e := h.mustRun(t, "login", "--no-browser", "--no-contexts")
	if strings.Contains(e, "contexts") {
		t.Fatalf("login --no-contexts mentioned contexts: %s", e)
	}
	if _, err := os.Stat(ctxFileOf(t, h)); err == nil {
		t.Fatal("--no-contexts wrote the context file")
	}
	// The one-cluster auto-select honors it too.
	h.setClusters(clusterJSON(0)[:0] + "[" + clusterJSON(0) + "]")
	h.mustRun(t, "login", "--no-browser", "--no-contexts")
	if _, err := os.Stat(ctxFileOf(t, h)); err == nil {
		t.Fatal("one-cluster login with --no-contexts wrote the context file")
	}
}

func TestLoginOneClusterAddsItsContext(t *testing.T) {
	h := ctxHarness(t)
	h.loginSeam()
	h.setClusters("[" + clusterJSON(0) + "]")
	e := h.mustRun(t, "login", "--no-browser")
	if !strings.Contains(e, "added 1 context to ") || strings.Contains(e, "added context") || !strings.Contains(e, "one cluster found and selected") {
		t.Fatalf("%s", e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"c00"}) {
		t.Fatalf("contexts %v", got)
	}
}

func TestLoginCountsNoOIDCAndSurvivesFetchFailure(t *testing.T) {
	h := ctxHarness(t)
	h.loginSeam()
	h.f.noOIDC["ub"] = true
	h.f.fail["un"] = true
	code, _, e := h.run("login", "--no-browser")
	if code != 0 {
		t.Fatalf("a failed fetch failed the login: %d %s", code, e)
	}
	if !strings.Contains(e, "added 1 context to ") || !strings.Contains(e, "1 cluster has no OIDC") || !strings.Contains(e, "1 failed") || !strings.Contains(e, "Other/plain") {
		t.Fatalf("summary: %s", e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("contexts %v", got)
	}
	// The no-OIDC finding reaches the index, so the picker can mark it.
	h.a.StdinIsTTY = true
	var items []picker.Item
	h.a.Pick = func(in []picker.Item) (int, error) { items = in; return 0, nil }
	h.run("use", "--print-env")
	if len(items) != 3 || !strings.Contains(items[1].Label, "no-oidc") {
		t.Fatalf("labels: %+v", items)
	}
}

func TestContextsListAddRemovePrune(t *testing.T) {
	h := ctxHarness(t)
	e := h.mustRun(t, "contexts", "list")
	if !strings.Contains(e, "no contexts yet") {
		t.Fatalf("%s", e)
	}
	h.mustRun(t, "use", "alpha", "--print-env")
	_, out, _ := h.run("contexts")
	if f := strings.Fields(out); len(f) != 3 || f[0] != "alpha" || f[1] != "default" || f[2] != "Default/alpha" {
		t.Fatalf("list: %q", out)
	}

	e = h.mustRun(t, "contexts", "add", "beta", "Other/plain", "alpha")
	if !strings.Contains(e, "added 2 contexts") || !strings.Contains(e, "1 already present") {
		t.Fatalf("add: %s", e)
	}
	if code, _, e := h.run("contexts", "add"); code == 0 {
		t.Fatalf("add with no name: %s", e)
	}
	if code, _, e := h.run("contexts", "add", "nope"); code == 0 || !strings.Contains(e, "no such cluster") {
		t.Fatalf("add unknown: %d %s", code, e)
	}
	e = h.mustRun(t, "contexts", "add", "--all")
	if !strings.Contains(e, "no new contexts") || !strings.Contains(e, "3 already present") {
		t.Fatalf("add --all: %s", e)
	}

	// remove by name, then an unknown name is an error that still removes the rest
	code, _, e := h.run("contexts", "remove", "beta", "ghost")
	if code == 0 || !strings.Contains(e, "removed 1 context") || !strings.Contains(e, "ghost") {
		t.Fatalf("remove: %d %s", code, e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"alpha", "plain"}) {
		t.Fatalf("contexts %v", got)
	}
	if h.kube(t).Clusters["beta"] != nil {
		t.Fatal("cluster entry left behind")
	}

	// prune drops what Palette no longer lists
	h.setClusters(`[{"name":"alpha","uid":"ua","annotations":{"projectUid":"p1"}}]`)
	e = h.mustRun(t, "contexts", "prune")
	if !strings.Contains(e, "removed 1 context") || !strings.Contains(e, "plain") {
		t.Fatalf("prune: %s", e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("contexts %v", got)
	}

	h.mustRun(t, "contexts", "remove", "--all")
	if got := h.contextNames(t); len(got) != 0 || len(h.records(t)) != 0 {
		t.Fatalf("remove --all left %v", got)
	}
	if code, _, _ := h.run("contexts", "remove", "--all-tenants"); code == 0 {
		t.Fatal("--all-tenants alone must be refused")
	}
	if code, _, _ := h.run("contexts", "bogus"); code == 0 {
		t.Fatal("unknown subcommand must fail")
	}
}

func TestContextsAcrossTenants(t *testing.T) {
	h := ctxHarness(t)
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "use", "alpha", "--tenant", "default", "--print-env")
	h.mustRun(t, "use", "alpha", "--tenant", "acme", "--print-env")

	// The same cluster name in two tenants gets two names, each with its own --tenant pin.
	names := h.contextNames(t)
	if len(names) != 2 {
		t.Fatalf("contexts %v", names)
	}
	cfg := h.kube(t)
	pins := map[string][]string{}
	for _, r := range h.records(t) {
		u := cfg.AuthInfos[cfg.Contexts[r.Context].AuthInfo]
		pins[r.Tenant] = u.Exec.Args
		if cfg.Contexts[r.Context].AuthInfo == "" {
			t.Fatalf("no user for %s", r.Context)
		}
	}
	if !reflect.DeepEqual(pins["default"], []string{"token", "--issuer-id", config.IssuerID("https://i")}) || !reflect.DeepEqual(pins["acme"], []string{"token", "--tenant", "acme", "--issuer-id", config.IssuerID("https://i/acme")}) {
		t.Fatalf("exec args: %v", pins)
	}
	if h.records(t)[0].Context == h.records(t)[1].Context || h.records(t)[0].User == h.records(t)[1].User {
		t.Fatalf("names collide: %+v", h.records(t))
	}

	// list shows both; remove --all stays inside the selected tenant
	_, out, _ := h.run("contexts", "list")
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "acme") || !strings.Contains(out, "default") {
		t.Fatalf("list: %q", out)
	}
	h.mustRun(t, "contexts", "remove", "--all", "--tenant", "acme")
	recs := h.records(t)
	if len(recs) != 1 || recs[0].Tenant != "default" {
		t.Fatalf("remove --all crossed tenants: %+v", recs)
	}
	h.mustRun(t, "use", "alpha", "--tenant", "acme", "--print-env")
	h.mustRun(t, "contexts", "remove", "--all", "--all-tenants")
	if len(h.records(t)) != 0 || len(h.contextNames(t)) != 0 {
		t.Fatalf("--all-tenants left %v", h.contextNames(t))
	}
}

func TestContextsPruneLeavesOtherTenants(t *testing.T) {
	h := ctxHarness(t)
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "contexts", "add", "--all", "--tenant", "acme")
	h.mustRun(t, "contexts", "add", "--all", "--tenant", "default")
	if len(h.records(t)) != 6 {
		t.Fatalf("records: %d", len(h.records(t)))
	}
	h.setClusters(`[{"name":"alpha","uid":"ua","annotations":{"projectUid":"p1"}}]`)
	h.mustRun(t, "contexts", "prune", "--tenant", "default")
	byTenant := map[string]int{}
	for _, r := range h.records(t) {
		byTenant[r.Tenant]++
	}
	if byTenant["default"] != 1 || byTenant["acme"] != 3 {
		t.Fatalf("prune crossed tenants: %v", byTenant)
	}
}

func TestLogoutRemovesOnlyThatEntrysContexts(t *testing.T) {
	h := ctxHarness(t)
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "use", "alpha", "--print-env")
	h.mustRun(t, "use", "beta", "--tenant", "acme", "--print-env")
	e := h.mustRun(t, "logout", "--tenant", "acme")
	if !strings.Contains(e, "removed 1 context from ") {
		t.Fatalf("%s", e)
	}
	recs := h.records(t)
	if len(recs) != 1 || recs[0].Tenant != "default" || !reflect.DeepEqual(h.contextNames(t), []string{"alpha"}) {
		t.Fatalf("records %+v contexts %v", recs, h.contextNames(t))
	}
	// an expired session on its own leaves contexts alone
	h.addEntry(t, "globex", h.namedCfg("globex"), "rt-0", "globex")
	h.mustRun(t, "use", "plain", "--tenant", "globex", "--print-env")
	h.a.Now = func() time.Time { return time.Now().Add(1000 * time.Hour) }
	h.run("tenants")
	h.a.Now = nil
	if len(h.records(t)) != 2 {
		t.Fatal("an expired session removed contexts")
	}
}

func TestLogoutKeepContextsAndAll(t *testing.T) {
	h := ctxHarness(t)
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "use", "alpha", "--print-env")
	h.mustRun(t, "use", "beta", "--tenant", "acme", "--print-env")
	e := h.mustRun(t, "logout", "--tenant", "acme", "--keep-contexts")
	if strings.Contains(e, "removed 1 context") || len(h.records(t)) != 2 {
		t.Fatalf("--keep-contexts: %s %+v", e, h.records(t))
	}
	e = h.mustRun(t, "logout", "--all")
	if !strings.Contains(e, "removed 2 contexts from ") || len(h.records(t)) != 0 || len(h.contextNames(t)) != 0 {
		t.Fatalf("logout --all: %s %+v %v", e, h.records(t), h.contextNames(t))
	}
}

func TestLogoutSurvivesABrokenKubeconfig(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--print-env")
	if err := os.WriteFile(ctxFileOf(t, h), []byte("{{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, e := h.run("logout")
	if code != 0 || !strings.Contains(e, "logged out") || !strings.Contains(e, "warning: could not remove the contexts") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestIssuerChangeWipeRemovesContexts(t *testing.T) {
	h := ctxHarness(t)
	h.mustRun(t, "use", "alpha", "--print-env")
	h.mustRun(t, "use", "beta", "--print-env")
	h.addEntry(t, "acme", h.namedCfg("acme"), "rt-0", "acme")
	h.mustRun(t, "use", "alpha", "--tenant", "acme", "--print-env")
	newIss := newIssuerServer(t)
	e := h.mustRun(t, "login", "--no-contexts", "--issuer", newIss.URL, "--api-url", h.f.srv.URL, "--import-kubelogin-cache", kubeloginCache(t))
	if !strings.Contains(e, "and 2 contexts") {
		t.Fatalf("%s", e)
	}
	recs := h.records(t)
	if len(recs) != 1 || recs[0].Tenant != "acme" {
		t.Fatalf("records after wipe: %+v", recs)
	}
}

func TestUseUnset(t *testing.T) {
	h := ctxHarness(t)
	code, out, e := h.run("use", "--unset", "--print-env")
	if code != 0 || strings.TrimSpace(out) != "unset KUBECONFIG" || h.f.total() != 0 {
		t.Fatalf("%d %q %s hits=%v", code, out, e, h.f.hits)
	}
	// Piped stdout counts as the hook too.
	if _, out, _ = h.run("use", "--unset"); strings.TrimSpace(out) != "unset KUBECONFIG" {
		t.Fatalf("%q", out)
	}
	// On a terminal without the hook there is nothing to eval: say what to run.
	h.a.StdoutIsTTY = true
	code, out, e = h.run("use", "--unset")
	if code != 0 || out != "" || !strings.Contains(e, "Run: unset KUBECONFIG") {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if code, _, _ = h.run("use", "alpha", "--unset"); code == 0 {
		t.Fatal("--unset with a name must be refused")
	}
}

func virtualClusters() string {
	return `[
	 {"name":"host","uid":"uh","annotations":{"projectUid":"p1","permissions":"cluster.get"}},
	 {"name":"vc1","uid":"uv1","annotations":{"projectUid":"p1","hostClusterUid":"uh","permissions":"cluster.get"}},
	 {"name":"vc2","uid":"uv2","annotations":{"projectUid":"p2","hostClusterUid":"uh","permissions":"cluster.get"}},
	 {"name":"orphan","uid":"uo","annotations":{"projectUid":"p1","hostClusterUid":"gone","permissions":"cluster.get"}},
	 {"name":"other","uid":"uother","annotations":{"projectUid":"p1","permissions":"cluster.get"}}]`
}

func TestVirtualClusterLabels(t *testing.T) {
	h := ctxHarness(t)
	h.setClusters(virtualClusters())
	h.mustRun(t, "sync")
	h.a.StdinIsTTY = true
	var items []picker.Item
	h.a.Pick = func(in []picker.Item) (int, error) { items = in; return 0, nil }
	h.mustRun(t, "use", "--print-env")
	labels := map[string]picker.Item{}
	for _, it := range items {
		labels[strings.Fields(it.Label)[0]] = it
	}
	if l := labels["Default/vc1"].Label; !strings.HasSuffix(l, "virtual on Default/host") {
		t.Errorf("vc1 label %q", l)
	}
	if d := labels["Default/vc1"].Detail; !strings.Contains(d, "virtual on Default/host") {
		t.Errorf("vc1 detail %q", d)
	}
	if l := labels["Default/orphan"].Label; !strings.HasSuffix(l, "virtual") {
		t.Errorf("orphan label %q", l)
	}
	if l := labels["Default/host"].Label; strings.Contains(l, "virtual") {
		t.Errorf("host label %q", l)
	}
	// the numbered list marks them too
	h.a.Stdin = strings.NewReader("1\n")
	_, _, e := h.run("use", "--print-env", "--no-fuzzy")
	if !strings.Contains(e, "Default/vc1  virtual on Default/host") || !strings.Contains(e, "Default/orphan  virtual\n") {
		t.Fatalf("numbered list:\n%s", e)
	}
}

func TestUseHost(t *testing.T) {
	h := ctxHarness(t)
	h.setClusters(virtualClusters())
	h.mustRun(t, "sync")
	code, out, e := h.run("use", "vc1", "--host", "host", "--print-env")
	if code != 0 || !strings.Contains(out, "vc1.yaml") {
		t.Fatalf("%d %q %s", code, out, e)
	}
	// a cluster the host does not host is not found, even though it exists
	if code, _, e = h.run("use", "other", "--host", "host", "--print-env"); code == 0 || !strings.Contains(e, "no such cluster") {
		t.Fatalf("%d %s", code, e)
	}
	// a host with no virtual clusters
	if code, _, e = h.run("use", "--host", "other", "--print-env"); code == 0 || !strings.Contains(e, "no virtual clusters are hosted by other") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e = h.run("use", "vc1", "--host", "nothere", "--print-env"); code == 0 || !strings.Contains(e, "--host nothere") {
		t.Fatalf("%d %s", code, e)
	}
	// the picker sees only what the host hosts
	h.a.StdinIsTTY = true
	var items []picker.Item
	h.a.Pick = func(in []picker.Item) (int, error) { items = in; return 1, nil }
	code, out, e = h.run("use", "--host", "Default/host", "--print-env")
	if code != 0 || len(items) != 2 || !strings.Contains(out, "vc2.yaml") {
		t.Fatalf("%d %q %s items=%d", code, out, e, len(items))
	}
}

func TestContextsAddAllWithHost(t *testing.T) {
	h := ctxHarness(t)
	h.setClusters(virtualClusters())
	h.mustRun(t, "sync")
	e := h.mustRun(t, "contexts", "add", "--all", "--host", "host")
	if !strings.Contains(e, "added 2 contexts") {
		t.Fatalf("%s", e)
	}
	if got := h.contextNames(t); !reflect.DeepEqual(got, []string{"vc1", "vc2"}) {
		t.Fatalf("contexts %v", got)
	}
	if code, _, e := h.run("contexts", "add", "--all", "--host", "other"); code == 0 || !strings.Contains(e, "no virtual clusters") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestContextsAddParallelismIsBounded(t *testing.T) {
	h := ctxHarness(t)
	h.setClusters(manyClusters(30))
	h.f.delay = 30 * time.Millisecond
	h.mustRun(t, "sync")
	e := h.mustRun(t, "contexts", "add", "--all")
	if !strings.Contains(e, "added 30 contexts") || len(h.contextNames(t)) != 30 {
		t.Fatalf("%s", e)
	}
	if h.f.max < 2 || h.f.max > fetchParallelism {
		t.Fatalf("downloads overlapped %d at most, want 2 to %d", h.f.max, fetchParallelism)
	}
}

func TestLoginHeadlessAndPasteBack(t *testing.T) {
	var got oidc.LoginOptions
	setup := func() *harness {
		h := ctxHarness(t)
		h.a.OpenBrowser = func(string) error { return nil }
		h.a.Login = func(_ context.Context, o oidc.LoginOptions) (session.Session, config.Config, error) {
			got = o
			exp := time.Now().Add(time.Hour)
			return session.Session{IDToken: jwtExp(exp, "x"), RefreshToken: "rt-0", IDExpiry: exp}, o.Cfg, nil
		}
		return h
	}

	// An ssh session is headless: no browser, an ssh hint, and no paste without a TTY.
	h := setup()
	h.env["SSH_CONNECTION"] = "1.2.3.4 1 5.6.7.8 22"
	h.mustRun(t, "login", "--no-contexts")
	if got.OpenBrowser != nil || !strings.Contains(got.Hint, "ssh -L 8000:localhost:8000 <this host>") || !strings.Contains(got.Hint, "redirect_uri") {
		t.Fatalf("headless options: %+v", got)
	}
	if got.In != nil {
		t.Fatal("a non-TTY stdin must not be offered for paste-back")
	}

	// A TTY gets paste-back, also when a browser is opened.
	h = setup()
	h.env["DISPLAY"] = ":0"
	h.a.StdinIsTTY = true
	h.mustRun(t, "login", "--no-contexts")
	if got.OpenBrowser == nil || got.Hint != "" || got.In != h.a.Stdin {
		t.Fatalf("desktop options: open=%v hint=%q in=%v", got.OpenBrowser != nil, got.Hint, got.In)
	}

	// --no-browser skips the browser even on a desktop.
	h = setup()
	h.env["DISPLAY"] = ":0"
	h.mustRun(t, "login", "--no-contexts", "--no-browser")
	if got.OpenBrowser != nil {
		t.Fatal("--no-browser opened a browser")
	}
}

// A cluster whose OIDC setup names another identity provider must not be pointed at the
// Palette token: no cluster file, no context, and an error that says why.
func TestClusterWithAnotherIssuerIsRefused(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	h.f.mu.Lock()
	h.f.issuers["rt-0"] = "https://login.idp.example.com/tenant-a"
	h.f.mu.Unlock()
	code, out, errs := h.run("use", "alpha", "--print-env")
	if code == 0 {
		t.Fatalf("use succeeded: %s", out)
	}
	if !strings.Contains(errs, "different identity provider") || !strings.Contains(errs, "login.idp.example.com") {
		t.Fatalf("stderr = %q", errs)
	}
	if strings.Contains(errs, "tenant-a") {
		t.Errorf("the message shows the issuer path: %q", errs)
	}
	if _, err := os.Stat(filepath.Join(h.a.Paths.KubeDir, "Default", "alpha.yaml")); err == nil {
		t.Error("a cluster file was written for a cluster with another issuer")
	}
	if n := h.contextNames(t); len(n) != 0 {
		t.Errorf("contexts recorded: %v", n)
	}
	// A trailing slash is the same issuer.
	h.f.mu.Lock()
	h.f.issuers["rt-0"] = "https://i/"
	h.f.mu.Unlock()
	if code, _, errs := h.run("use", "alpha", "--print-env"); code != 0 {
		t.Fatalf("trailing slash issuer refused: %s", errs)
	}
}

// Logging in with --server to a tenant that already has an entry reuses that entry.
func TestServerLoginReusesTheEntryOfTheSameIssuer(t *testing.T) {
	h := newHarness(t, time.Now().Add(time.Hour))
	acme := tenantServer(t, "acme", uidAcme, "alpha")
	h.a.Login = fakeLogin(map[string]string{uidAcme: "acme"})
	h.a.HTTP = acme.Client()
	// The same tenant is already saved under another name.
	h.mustRun(t, "login", "--server", acme.URL, "--name", "work", "--no-contexts")
	h.mustRun(t, "login", "--server", acme.URL, "--no-contexts")
	got := h.a.Paths.Root().Entries()
	for _, e := range got {
		if e == "acme" {
			t.Fatalf("a second entry was created for the same tenant: %v", got)
		}
	}
	if h.a.Paths.Root().Active() != "work" {
		t.Fatalf("active %q, entries %v", h.a.Paths.Root().Active(), got)
	}
}
