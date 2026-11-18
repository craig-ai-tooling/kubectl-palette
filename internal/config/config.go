// Package config holds the saved bootstrap settings, file locations and safe file helpers.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Environment variables that move the state directories, for tests and live checks.
const (
	EnvHome    = "KUBECTL_PALETTE_HOME"
	EnvKubeDir = "KUBECTL_PALETTE_KUBEDIR"
)

// Environment variables that pick a tenant entry and a default Palette server.
const (
	EnvTenant = "KUBECTL_PALETTE_TENANT"
	EnvServer = "KUBECTL_PALETTE_SERVER"
)

// DefaultEntry is the tenant entry that keeps the original flat layout: files directly in
// Home and cluster files directly in KubeDir. It needs no migration.
const DefaultEntry = "default"

// TenantsDir is the subdirectory of Home that holds named entries.
const TenantsDir = "tenants"

// TenantKubeDir is the subdirectory of KubeDir that holds the cluster files of named
// entries. It cannot be a project name, so the two layouts never share a path.
const TenantKubeDir = ".tenants"

// Paths locates every file the plugin owns. Home and KubeDir point at the selected
// tenant entry once ForTenant has been applied. Without it they are the flat default layout.
type Paths struct {
	Home    string // config, session and index of the entry
	KubeDir string // per-cluster kubeconfigs of the entry

	entry    string // "" before a selection, which behaves as DefaultEntry
	rootHome string // Home of the whole plugin, "" when this Paths is already the root
	rootKube string
}

var entryNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidEntryName checks a name for a named tenant entry. It keeps the name safe to use as a
// directory name. DefaultEntry is reserved for the flat layout.
func ValidEntryName(name string) error {
	if name == DefaultEntry {
		return fmt.Errorf("%q is reserved for the default entry", name)
	}
	if !entryNameRE.MatchString(name) {
		return fmt.Errorf("invalid tenant entry name %q: use letters, digits, '.', '_' or '-', starting with a letter or digit, at most 63 characters", name)
	}
	return nil
}

// Entry is the selected entry name, DefaultEntry when none was applied.
func (p Paths) Entry() string {
	if p.entry == "" {
		return DefaultEntry
	}
	return p.entry
}

// Root returns the Paths of the whole plugin, whatever entry p points at.
func (p Paths) Root() Paths {
	if p.rootHome == "" {
		return Paths{Home: p.Home, KubeDir: p.KubeDir}
	}
	return Paths{Home: p.rootHome, KubeDir: p.rootKube}
}

// ForTenant returns the paths of one entry. DefaultEntry (or "") is the flat layout. It
// does not check the name or that the entry exists.
func (p Paths) ForTenant(name string) Paths {
	r := p.Root()
	out := Paths{Home: r.Home, KubeDir: r.KubeDir, entry: DefaultEntry, rootHome: r.Home, rootKube: r.KubeDir}
	if name != "" && name != DefaultEntry {
		out.entry = name
		out.Home = filepath.Join(r.Home, TenantsDir, name)
		out.KubeDir = filepath.Join(r.KubeDir, TenantKubeDir, name)
	}
	return out
}

// ActiveFile holds the name of the active entry. It lives in the root Home.
func (p Paths) ActiveFile() string { return filepath.Join(p.Root().Home, "active") }

// Active reads the active entry name, "" when none is set.
func (p Paths) Active() string {
	b, err := os.ReadFile(p.ActiveFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetActive records the active entry.
func (p Paths) SetActive(name string) error {
	return WriteFileAtomic(p.ActiveFile(), []byte(name+"\n"))
}

// ClearActive removes the active marker.
func (p Paths) ClearActive() error {
	if err := os.Remove(p.ActiveFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Entries lists the saved entries: DefaultEntry first when its config exists, then the
// named ones in order. An entry exists when its config file does.
func (p Paths) Entries() []string {
	r := p.Root()
	var out []string
	if _, err := os.Stat(r.ForTenant(DefaultEntry).ConfigFile()); err == nil {
		out = append(out, DefaultEntry)
	}
	ents, _ := os.ReadDir(filepath.Join(r.Home, TenantsDir))
	var named []string
	for _, e := range ents {
		if !e.IsDir() || ValidEntryName(e.Name()) != nil {
			continue
		}
		if _, err := os.Stat(r.ForTenant(e.Name()).ConfigFile()); err == nil {
			named = append(named, e.Name())
		}
	}
	sort.Strings(named)
	return append(out, named...)
}

// DefaultPaths resolves the locations from the environment and the user's home.
func DefaultPaths(getenv func(string) string) (Paths, error) {
	home, kube := getenv(EnvHome), getenv(EnvKubeDir)
	if home == "" || kube == "" {
		uh, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("find home directory: %w", err)
		}
		if home == "" {
			home = filepath.Join(uh, ".config", "kubectl-palette")
		}
		if kube == "" {
			kube = filepath.Join(uh, ".kube", "palette")
		}
	}
	return Paths{Home: home, KubeDir: kube}, nil
}

// File paths under Home.
func (p Paths) ConfigFile() string  { return filepath.Join(p.Home, "config.json") }
func (p Paths) SessionFile() string { return filepath.Join(p.Home, "session.json") }
func (p Paths) SessionLock() string { return filepath.Join(p.Home, "session.lock") }
func (p Paths) IndexFile() string   { return filepath.Join(p.Home, "index.json") }
func (p Paths) IndexLock() string   { return filepath.Join(p.Home, "index.lock") }

// Config is the bootstrap data saved at login so later logins need no flags.
type Config struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	APIURL       string `json:"api_url"`
	AuthURL      string `json:"auth_url,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
	// Org and Server are recorded by a server login, for display and for logging in again.
	Org    string `json:"org,omitempty"`
	Server string `json:"server,omitempty"`
	// CAFile is an absolute path to a PEM file trusted in addition to the system roots for
	// every request of this entry.
	CAFile string `json:"ca_file,omitempty"`
}

// ErrNoConfig means login has never been configured.
var ErrNoConfig = errors.New("not configured: run: kubectl palette login --server <url> [--tenant <org>]")

// Validate reports which required field is missing. The client secret is optional: a
// Palette tenant client works without one.
func (c Config) Validate() error {
	for _, f := range []struct{ n, v string }{
		{"issuer", c.Issuer}, {"client id", c.ClientID}, {"API URL", c.APIURL},
	} {
		if f.v == "" {
			return fmt.Errorf("missing %s: pass --server and --tenant, --from-kubeconfig, or the matching flag to kubectl palette login", f.n)
		}
	}
	return nil
}

// Load reads the config file.
func (p Paths) Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(p.ConfigFile())
	if errors.Is(err, fs.ErrNotExist) {
		return c, ErrNoConfig
	}
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config %s: %w", p.ConfigFile(), err)
	}
	return c, nil
}

// Save writes the config file atomically with mode 600.
func (p Paths) Save(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p.ConfigFile(), append(b, '\n'))
}

// AdminDir is the per-project subdirectory that holds admin kubeconfigs. It cannot be a
// project or cluster name, so admin and OIDC files can never share a path.
const AdminDir = ".admin"

// UIDSuffix names the sidecar that records which cluster UID a cached file belongs to.
const UIDSuffix = ".uid"

// SafeName rejects names that could escape a directory. API names are untrusted.
func SafeName(name string) error {
	if name == AdminDir {
		return fmt.Errorf("unsafe name %q: reserved for admin kubeconfigs", name)
	}
	if name == TenantKubeDir {
		return fmt.Errorf("unsafe name %q: reserved for tenant entries", name)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("unsafe name %q: refusing to build a path", name)
	}
	return nil
}

// SafeJoin joins validated name elements under base and confirms the result stays inside it.
func SafeJoin(base string, names ...string) (string, error) {
	for _, n := range names {
		if err := SafeName(n); err != nil {
			return "", err
		}
	}
	p := filepath.Join(append([]string{base}, names...)...)
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes %q", p, base)
	}
	return p, nil
}

// ClusterFile is the path of a cluster's kubeconfig. Admin files live in their own
// subdirectory of the project, so no cluster name can make the two kinds collide.
func (p Paths) ClusterFile(project, cluster string, admin bool) (string, error) {
	if err := SafeName(cluster); err != nil {
		return "", err
	}
	if admin {
		dir, err := SafeJoin(p.KubeDir, project)
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, AdminDir, cluster+".yaml"), nil
	}
	return SafeJoin(p.KubeDir, project, cluster+".yaml")
}

// CachedUID reads the cluster UID recorded next to a cached kubeconfig. It never opens the
// kubeconfig itself. It returns "" when there is no sidecar, which callers treat as untrusted.
func CachedUID(path string) string {
	b, err := os.ReadFile(path + UIDSuffix)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteCachedUID records the cluster UID for a cached kubeconfig.
func WriteCachedUID(path, uid string) error {
	return WriteFileAtomic(path+UIDSuffix, []byte(uid+"\n"))
}

// RemoveCluster deletes a cached kubeconfig and its UID sidecar.
func RemoveCluster(path string) error {
	var errs []error
	for _, p := range []string{path + UIDSuffix, path} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WriteFileAtomic writes data to a mode 600 temp file in the same directory, fsyncs it and
// renames it over path, creating the parent directory with mode 700. A reader never sees a
// partial file and the file is never created with a wider mode.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// RemoveEntry deletes everything a named entry owns: its state directory and its cluster
// files. The default entry is flat and is removed file by file by the caller.
func (p Paths) RemoveEntry(name string) error {
	if err := ValidEntryName(name); err != nil {
		return err
	}
	e := p.ForTenant(name)
	return errors.Join(os.RemoveAll(e.Home), os.RemoveAll(e.KubeDir))
}
