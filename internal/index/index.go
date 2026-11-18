// Package index is the local cache of cluster names. Completion, the picker and name
// resolution read only this file.
package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/palette"
)

// StaleAfter is the age after which a command starts a background refresh.
const StaleAfter = 10 * time.Minute

// AdminPermission is the annotation entry that allows an admin kubeconfig download.
const AdminPermission = "cluster.adminKubeconfigDownload"

// Cluster is one entry in the index.
type Cluster struct {
	Name        string   `json:"name"`
	UID         string   `json:"uid"`
	ProjectUID  string   `json:"project_uid"`
	Project     string   `json:"project"`
	Permissions []string `json:"permissions,omitempty"`
}

// CanAdmin reports whether the caller may download this cluster's admin kubeconfig.
func (c Cluster) CanAdmin() bool {
	for _, p := range c.Permissions {
		if p == AdminPermission {
			return true
		}
	}
	return false
}

// Ref is the unambiguous project/cluster form.
func (c Cluster) Ref() string { return c.Project + "/" + c.Name }

// Index is the cached listing.
type Index struct {
	UpdatedAt time.Time `json:"updated_at"`
	Clusters  []Cluster `json:"clusters"`
}

// Source is the part of the Palette client the index needs.
type Source interface {
	Metadata(ctx context.Context) ([]palette.ClusterMeta, error)
	Projects(ctx context.Context) ([]palette.Project, error)
}

// ErrNoIndex means no index file exists yet.
var ErrNoIndex = errors.New("no cluster index yet")

// Build fetches clusters and projects concurrently and assembles the index. A failed
// projects call is not fatal: clusters then show their project uid as the name.
func Build(ctx context.Context, src Source, now time.Time) (*Index, error) {
	var (
		wg       sync.WaitGroup
		metas    []palette.ClusterMeta
		projects []palette.Project
		merr     error
		perr     error
	)
	wg.Add(2)
	go func() { defer wg.Done(); metas, merr = src.Metadata(ctx) }()
	go func() { defer wg.Done(); projects, perr = src.Projects(ctx) }()
	wg.Wait()
	if merr != nil {
		return nil, fmt.Errorf("fetch cluster index: %w", merr)
	}
	names := map[string]string{}
	if perr == nil {
		for _, p := range projects {
			names[p.UID] = p.Name
		}
	}
	idx := &Index{UpdatedAt: now.UTC()}
	for _, m := range metas {
		if deleted(m) {
			continue
		}
		puid := m.Annotations["projectUid"]
		proj := names[puid]
		switch {
		case proj != "":
		case puid != "":
			proj = puid
		default:
			proj = "tenant"
		}
		var perms []string
		for _, p := range strings.Split(m.Annotations["permissions"], ",") {
			if p = strings.TrimSpace(p); p != "" {
				perms = append(perms, p)
			}
		}
		idx.Clusters = append(idx.Clusters, Cluster{Name: m.Name, UID: m.UID, ProjectUID: puid, Project: proj, Permissions: perms})
	}
	sort.Slice(idx.Clusters, func(i, j int) bool { return idx.Clusters[i].Ref() < idx.Clusters[j].Ref() })
	return idx, nil
}

func deleted(m palette.ClusterMeta) bool {
	if strings.EqualFold(m.Annotations["deleted"], "true") {
		return true
	}
	// Palette sends the zero time for live objects.
	return m.Deletion != "" && !strings.HasPrefix(m.Deletion, "0001-")
}

// Stale reports whether the index is older than StaleAfter.
func (i *Index) Stale(now time.Time) bool { return now.Sub(i.UpdatedAt) > StaleAfter }

// Load reads the index file.
func Load(path string) (*Index, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoIndex
	}
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	var i Index
	if err := json.Unmarshal(b, &i); err != nil {
		return nil, fmt.Errorf("parse index %s: %w", path, err)
	}
	return &i, nil
}

// Save writes the index atomically.
func (i *Index) Save(path string) error {
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b)
}

// ErrNotFound means a name matched no cluster.
var ErrNotFound = errors.New("no such cluster")

// AmbiguousError lists the candidates for a name that matched several clusters.
type AmbiguousError struct {
	Query      string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q is ambiguous, use project/cluster: %s", e.Query, strings.Join(e.Candidates, ", "))
}

// Resolve finds a cluster by name. Order: exact project/cluster, exact name, unique name
// prefix. Several matches at one step are an error naming project/cluster forms.
func (i *Index) Resolve(q string) (Cluster, error) {
	steps := []func(Cluster) bool{
		func(c Cluster) bool { return c.Ref() == q },
		func(c Cluster) bool { return c.Name == q },
		func(c Cluster) bool { return strings.HasPrefix(c.Name, q) || strings.HasPrefix(c.Ref(), q) },
	}
	for _, match := range steps {
		var hits []Cluster
		for _, c := range i.Clusters {
			if match(c) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], nil
		}
		amb := &AmbiguousError{Query: q}
		for _, h := range hits {
			amb.Candidates = append(amb.Candidates, h.Ref())
		}
		return Cluster{}, amb
	}
	return Cluster{}, fmt.Errorf("%w: %q", ErrNotFound, q)
}
