// Package kubectx records every Palette cluster a user has used as a context in the
// user's normal kubeconfig, so plain kubectl can switch between them.
//
// The plugin writes one small kubeconfig per cluster. Those files hold one cluster, one
// context and one user whose only credential is the exec plugin `kubectl-palette token`.
// This package copies that cluster and user into the shared kubeconfig and tracks what it
// created in a registry file. It never touches an entry the registry does not own, never
// changes current-context (except to clear it when it removes the context it names), and
// refuses any source whose user carries a credential other than the exec plugin.
//
// Every mutating function takes a lock on <registryPath>.lock for the whole
// read-modify-write. A kubeconfig that fails to parse is never overwritten.
package kubectx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/craig-ai-tooling/kubectl-palette/internal/session"
)

// DefaultTenant is the tenant name used for the legacy single-tenant layout.
const DefaultTenant = "default"

// ExecCommand is the only credential command a user entry may run.
const ExecCommand = "kubectl-palette"

// Entry identifies one Palette cluster.
type Entry struct {
	Tenant  string `json:"tenant"`  // plugin tenant entry name, "default" for the legacy layout
	Project string `json:"project"` // project name
	Cluster string `json:"cluster"` // cluster name
	UID     string `json:"uid"`     // Palette cluster UID
}

// Record is an Entry plus the names this package created for it.
type Record struct {
	Entry
	Context string    `json:"context"` // context name, also the cluster entry name
	User    string    `json:"user"`    // user entry name
	AddedAt time.Time `json:"added_at"`
}

type registryFile struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Test switches. skipSync turns off fsync so tests run fast; writes counts file writes.
var (
	skipSync bool
	writes   atomic.Int64
)

// writeAtomic writes data to path through a mode-600 temp file in the same directory and a
// rename. It is config.WriteFileAtomic with a switch for tests.
func writeAtomic(path string, data []byte) error {
	writes.Add(1)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	if !skipSync {
		if err := f.Sync(); err != nil {
			return fail(fmt.Errorf("sync %s: %w", path, err))
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// process-wide guard: flock does not serialize on Windows in this repo.
var procMu sync.Mutex

func normTenant(t string) string {
	if t == "" {
		return DefaultTenant
	}
	return t
}

func lockRegistry(registryPath string) (func(), error) {
	procMu.Lock()
	l, err := session.LockFile(registryPath + ".lock")
	if err != nil {
		procMu.Unlock()
		return nil, fmt.Errorf("lock registry: %w", err)
	}
	return func() { l.Unlock(); procMu.Unlock() }, nil
}

func readRegistry(path string) ([]Record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var rf registryFile
	if err := json.Unmarshal(b, &rf); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", path, err)
	}
	return rf.Records, nil
}

func writeRegistry(path string, recs []Record) error {
	sort.Slice(recs, func(i, j int) bool { return recs[i].Context < recs[j].Context })
	if recs == nil {
		recs = []Record{}
	}
	b, err := json.MarshalIndent(registryFile{Version: 1, Records: recs}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// loadKubeconfig returns an empty config when the file is missing or empty and an error
// when it does not parse.
func loadKubeconfig(path string) (*api.Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return api.NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return api.NewConfig(), nil
	}
	cfg, err := clientcmd.Load(b)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

func saveKubeconfig(path string, cfg *api.Config) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("encode kubeconfig: %w", err)
	}
	return writeAtomic(path, b)
}

// isPaletteExec reports whether u is only an exec call to kubectl-palette token, and
// returns the tenant its args name.
func isPaletteExec(u *api.AuthInfo) (tenant string, err error) {
	if u == nil {
		return "", errors.New("user entry is missing")
	}
	switch {
	case u.ClientCertificate != "" || len(u.ClientCertificateData) > 0:
		return "", errors.New("user has a client certificate")
	case u.ClientKey != "" || len(u.ClientKeyData) > 0:
		return "", errors.New("user has a client key")
	case u.Token != "" || u.TokenFile != "":
		return "", errors.New("user has a token")
	case u.Username != "" || u.Password != "":
		return "", errors.New("user has a username or password")
	case u.AuthProvider != nil:
		return "", errors.New("user has an auth provider")
	case u.Impersonate != "" || len(u.ImpersonateGroups) > 0 || len(u.ImpersonateUserExtra) > 0 || u.ImpersonateUID != "":
		return "", errors.New("user impersonates another identity")
	case u.Exec == nil:
		return "", errors.New("user has no exec credential")
	}
	ex := u.Exec
	if filepath.Base(ex.Command) != ExecCommand || ex.Command != ExecCommand {
		return "", fmt.Errorf("exec command is %q, want %q", ex.Command, ExecCommand)
	}
	if len(ex.Env) > 0 {
		return "", errors.New("exec block sets environment variables")
	}
	if len(ex.Args) == 0 || ex.Args[0] != "token" {
		return "", errors.New("exec args do not start with \"token\"")
	}
	tenant = DefaultTenant
	rest := ex.Args[1:]
	for i := 0; i < len(rest); i++ {
		switch {
		case rest[i] == "--tenant" && i+1 < len(rest):
			tenant = rest[i+1]
			i++
		case strings.HasPrefix(rest[i], "--tenant="):
			tenant = strings.TrimPrefix(rest[i], "--tenant=")
		default:
			return "", fmt.Errorf("unexpected exec arg %q", rest[i])
		}
	}
	return tenant, nil
}

// parseSource picks the cluster, user and namespace the source's current context (or only
// context) points at.
func parseSource(source []byte) (*api.Cluster, *api.AuthInfo, string, error) {
	cfg, err := clientcmd.Load(source)
	if err != nil {
		return nil, nil, "", fmt.Errorf("parse source kubeconfig: %w", err)
	}
	name := cfg.CurrentContext
	if name == "" {
		if len(cfg.Contexts) != 1 {
			return nil, nil, "", errors.New("source kubeconfig has no current-context and not exactly one context")
		}
		for n := range cfg.Contexts {
			name = n
		}
	}
	c := cfg.Contexts[name]
	if c == nil {
		return nil, nil, "", fmt.Errorf("source context %q not found", name)
	}
	cl, u := cfg.Clusters[c.Cluster], cfg.AuthInfos[c.AuthInfo]
	if cl == nil {
		return nil, nil, "", fmt.Errorf("source cluster %q not found", c.Cluster)
	}
	if u == nil {
		return nil, nil, "", fmt.Errorf("source user %q not found", c.AuthInfo)
	}
	return cl, u, c.Namespace, nil
}

func findRecord(recs []Record, tenant, uid string) int {
	for i, r := range recs {
		if r.Tenant == tenant && r.UID == uid {
			return i
		}
	}
	return -1
}

func candidates(e Entry) []string {
	var out []string
	add := func(parts ...string) {
		var keep []string
		for _, p := range parts {
			if p != "" {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			return
		}
		n := strings.Join(keep, "/")
		for _, o := range out {
			if o == n {
				return
			}
		}
		out = append(out, n)
	}
	add(e.Cluster)
	add(e.Project, e.Cluster)
	add(e.Tenant, e.Project, e.Cluster)
	return out
}

func pickContext(cfg *api.Config, recs []Record, e Entry) string {
	taken := func(n string) bool {
		if cfg.Contexts[n] != nil || cfg.Clusters[n] != nil {
			return true
		}
		for _, r := range recs {
			if r.Context == n {
				return true
			}
		}
		return false
	}
	cands := candidates(e)
	for _, n := range cands {
		if !taken(n) {
			return n
		}
	}
	base := cands[len(cands)-1]
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", base, i); !taken(n) {
			return n
		}
	}
}

func baseUser(tenant string) string {
	if tenant == DefaultTenant {
		return "palette"
	}
	return "palette-" + tenant
}

// pickUser returns the user entry name for a tenant: the name already used by one of the
// tenant's records, else the first of base, base-2, ... that is absent or already one of
// ours.
func pickUser(cfg *api.Config, recs []Record, tenant string) string {
	for _, r := range recs {
		if r.Tenant == tenant && r.User != "" {
			if u := cfg.AuthInfos[r.User]; u == nil {
				return r.User
			} else if _, err := isPaletteExec(u); err == nil {
				return r.User
			}
		}
	}
	base := baseUser(tenant)
	for i := 1; ; i++ {
		n := base
		if i > 1 {
			n = fmt.Sprintf("%s-%d", base, i)
		}
		u := cfg.AuthInfos[n]
		if u == nil {
			return n
		}
		if _, err := isPaletteExec(u); err == nil {
			return n
		}
	}
}

// Item is one cluster for UpsertAll: its identity and the bytes of the plugin's
// per-cluster kubeconfig.
type Item struct {
	Entry  Entry
	Source []byte
}

// Result is the outcome of one Item. Err is set when the item was refused or skipped.
type Result struct {
	Record Record
	Err    error
}

// upsertOne applies one item to cfg and recs in memory. It changes neither when it returns
// an error. It returns the new record list.
func upsertOne(cfg *api.Config, recs []Record, e Entry, source []byte, now time.Time) (Record, []Record, error) {
	e.Tenant = normTenant(e.Tenant)
	if e.UID == "" || e.Cluster == "" {
		return Record{}, recs, errors.New("entry needs a cluster name and UID")
	}
	srcCluster, srcUser, srcNS, err := parseSource(source)
	if err != nil {
		return Record{}, recs, err
	}
	tenant, err := isPaletteExec(srcUser)
	if err != nil {
		return Record{}, recs, fmt.Errorf("refusing to copy cluster %q into the kubeconfig: %w", e.Cluster, err)
	}
	if tenant != e.Tenant {
		return Record{}, recs, fmt.Errorf("refusing to copy cluster %q: exec args name tenant %q, entry is tenant %q", e.Cluster, tenant, e.Tenant)
	}

	recs = append([]Record(nil), recs...)
	var rec Record
	if i := findRecord(recs, e.Tenant, e.UID); i >= 0 {
		rec = recs[i]
		rec.Entry = e
		recs = append(recs[:i], recs[i+1:]...)
	} else {
		stale := -1
		for i, r := range recs {
			if r.Tenant == e.Tenant && r.Project == e.Project && r.Cluster == e.Cluster {
				stale = i
				break
			}
		}
		if stale >= 0 {
			rec = recs[stale]
			rec.Entry = e
			rec.AddedAt = now.UTC()
			recs = append(recs[:stale], recs[stale+1:]...)
		} else {
			rec = Record{Entry: e, AddedAt: now.UTC()}
			rec.Context = pickContext(cfg, recs, e)
		}
	}
	rec.User = pickUser(cfg, append(append([]Record{}, recs...), rec), e.Tenant)

	cl := srcCluster.DeepCopy()
	cl.LocationOfOrigin = ""
	cfg.Clusters[rec.Context] = cl

	ex := srcUser.Exec
	au := &api.AuthInfo{Exec: &api.ExecConfig{
		APIVersion:      ex.APIVersion,
		Command:         ex.Command,
		Args:            append([]string(nil), ex.Args...),
		InteractiveMode: ex.InteractiveMode,
		InstallHint:     ex.InstallHint,
	}}
	if au.Exec.APIVersion == "" {
		au.Exec.APIVersion = "client.authentication.k8s.io/v1beta1"
	}
	if au.Exec.InteractiveMode == "" {
		au.Exec.InteractiveMode = api.IfAvailableExecInteractiveMode
	}
	cfg.AuthInfos[rec.User] = au

	ns := srcNS
	if old := cfg.Contexts[rec.Context]; old != nil && old.Cluster == rec.Context {
		ns = old.Namespace
	}
	cfg.Contexts[rec.Context] = &api.Context{Cluster: rec.Context, AuthInfo: rec.User, Namespace: ns}
	return rec, append(recs, rec), nil
}

// UpsertAll applies every item under one lock with one kubeconfig load, one registry load
// and one write of each file at the end. It returns one Result per item, in order. A refused
// or unparseable item sets its Result's Err and is skipped; the rest still apply, and names
// chosen for earlier items are taken for later ones. The error is for whole-operation
// failures (lock, unparseable kubeconfig or registry, write failure); then nothing is
// written. When no item succeeds, nothing is written. The rules for each item are those of
// Upsert.
func UpsertAll(kubeconfigPath, registryPath string, items []Item, now time.Time) ([]Result, error) {
	results := make([]Result, len(items))
	if len(items) == 0 {
		return results, nil
	}
	unlock, err := lockRegistry(registryPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	recs, err := readRegistry(registryPath)
	if err != nil {
		return nil, err
	}
	cfg, err := loadKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	ok := 0
	for i, it := range items {
		rec, next, err := upsertOne(cfg, recs, it.Entry, it.Source, now)
		if err != nil {
			results[i].Err = err
			continue
		}
		recs = next
		results[i].Record = rec
		ok++
	}
	if ok == 0 {
		return results, nil
	}
	if err := writeRegistry(registryPath, recs); err != nil {
		return nil, err
	}
	if err := saveKubeconfig(kubeconfigPath, cfg); err != nil {
		return nil, err
	}
	return results, nil
}

// Upsert records the cluster e in the registry and adds or refreshes its cluster, user and
// context entries in the kubeconfig at kubeconfigPath. source holds the bytes of the
// plugin's per-cluster kubeconfig. It returns the record, including the context name to
// pass to `kubectl --context`. It is UpsertAll with one item.
//
// It returns an error and writes nothing when the source user has any credential besides
// an exec call to `kubectl-palette token [--tenant <name>]`, or when the tenant named in
// those args differs from e.Tenant (an empty Tenant means "default"). The context name is
// chosen once per (Tenant, UID) and kept afterwards. A missing kubeconfig is created
// (mode 600). An existing context's namespace is kept. current-context is never changed.
func Upsert(kubeconfigPath, registryPath string, e Entry, source []byte, now time.Time) (Record, error) {
	res, err := UpsertAll(kubeconfigPath, registryPath, []Item{{Entry: e, Source: source}}, now)
	if err != nil {
		return Record{}, err
	}
	return res[0].Record, res[0].Err
}

// removeRecords deletes the kubeconfig entries of every record in drop (by index into recs)
// and returns the surviving records. It reports whether cfg changed.
func removeFromConfig(cfg *api.Config, drop []Record) (changed bool) {
	for _, r := range drop {
		c := cfg.Contexts[r.Context]
		if c == nil || c.Cluster != r.Context || c.AuthInfo != r.User {
			continue
		}
		delete(cfg.Contexts, r.Context)
		delete(cfg.Clusters, r.Context)
		if cfg.CurrentContext == r.Context {
			cfg.CurrentContext = ""
		}
		changed = true
	}
	for _, r := range drop {
		if r.User == "" || cfg.AuthInfos[r.User] == nil {
			continue
		}
		used := false
		for _, c := range cfg.Contexts {
			if c.AuthInfo == r.User {
				used = true
				break
			}
		}
		if used {
			continue
		}
		if _, err := isPaletteExec(cfg.AuthInfos[r.User]); err == nil {
			delete(cfg.AuthInfos, r.User)
			changed = true
		}
	}
	return changed
}

func apply(kubeconfigPath, registryPath string, pick func(Record, *api.Config) bool) ([]Record, error) {
	unlock, err := lockRegistry(registryPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	recs, err := readRegistry(registryPath)
	if err != nil {
		return nil, err
	}
	cfg, err := loadKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	var drop, keep []Record
	for _, r := range recs {
		if pick(r, cfg) {
			drop = append(drop, r)
		} else {
			keep = append(keep, r)
		}
	}
	if len(drop) == 0 {
		return nil, nil
	}
	if err := writeRegistry(registryPath, keep); err != nil {
		return nil, err
	}
	if removeFromConfig(cfg, drop) {
		if err := saveKubeconfig(kubeconfigPath, cfg); err != nil {
			return nil, err
		}
	}
	return drop, nil
}

// Remove drops every record for which match returns true. For each one it deletes the
// context and cluster entry only if the context still points at the cluster and user names
// the record holds; a context the user re-pointed or deleted is left alone and the record is
// just dropped. A user entry we created is deleted once no remaining context uses it. If
// current-context named a removed context it is set to empty. It returns the removed records.
func Remove(kubeconfigPath, registryPath string, match func(Record) bool) ([]Record, error) {
	return apply(kubeconfigPath, registryPath, func(r Record, _ *api.Config) bool { return match(r) })
}

// Prune is Remove for the records alive reports as false, plus the records whose context
// no longer exists in the kubeconfig. It returns the removed records.
func Prune(kubeconfigPath, registryPath string, alive func(Record) bool) ([]Record, error) {
	return apply(kubeconfigPath, registryPath, func(r Record, cfg *api.Config) bool {
		return cfg.Contexts[r.Context] == nil || !alive(r)
	})
}

// List returns the registry's records sorted by Context. A missing registry is an empty list.
func List(registryPath string) ([]Record, error) {
	recs, err := readRegistry(registryPath)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Context < recs[j].Context })
	return recs, nil
}

// Lookup returns the record for a tenant and cluster UID. The bool is false when there is none.
func Lookup(registryPath string, tenant, uid string) (Record, bool, error) {
	recs, err := readRegistry(registryPath)
	if err != nil {
		return Record{}, false, err
	}
	if i := findRecord(recs, normTenant(tenant), uid); i >= 0 {
		return recs[i], true, nil
	}
	return Record{}, false, nil
}
