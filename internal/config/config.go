// Package config holds the saved bootstrap settings, file locations and safe file helpers.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables that move the state directories, for tests and live checks.
const (
	EnvHome    = "KUBECTL_PALETTE_HOME"
	EnvKubeDir = "KUBECTL_PALETTE_KUBEDIR"
)

// Paths locates every file the plugin owns.
type Paths struct {
	Home    string // config, session and index
	KubeDir string // per-cluster kubeconfigs
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
}

// ErrNoConfig means login has never been configured.
var ErrNoConfig = errors.New("not configured: run: kubectl palette login --from-kubeconfig <file>")

// Validate reports which required field is missing.
func (c Config) Validate() error {
	for _, f := range []struct{ n, v string }{
		{"issuer", c.Issuer}, {"client id", c.ClientID}, {"client secret", c.ClientSecret}, {"API URL", c.APIURL},
	} {
		if f.v == "" {
			return fmt.Errorf("missing %s: pass --from-kubeconfig or the matching flag to kubectl palette login", f.n)
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

// SafeName rejects names that could escape a directory. API names are untrusted.
func SafeName(name string) error {
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

// ClusterFile is the path of a cluster's kubeconfig. Admin files get their own suffix.
func (p Paths) ClusterFile(project, cluster string, admin bool) (string, error) {
	suffix := ".yaml"
	if admin {
		suffix = ".admin.yaml"
	}
	if err := SafeName(cluster); err != nil {
		return "", err
	}
	return SafeJoin(p.KubeDir, project, cluster+suffix)
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
