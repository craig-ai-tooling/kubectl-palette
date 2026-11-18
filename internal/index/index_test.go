package index

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/palette"
)

type fakeSrc struct {
	metas    []palette.ClusterMeta
	projects []palette.Project
	perr     error
}

func (f fakeSrc) Metadata(context.Context) ([]palette.ClusterMeta, error) { return f.metas, nil }
func (f fakeSrc) Projects(context.Context) ([]palette.Project, error)     { return f.projects, f.perr }

func meta(name, uid, proj, perms string, ann map[string]string, del string) palette.ClusterMeta {
	a := map[string]string{"projectUid": proj, "permissions": perms}
	for k, v := range ann {
		a[k] = v
	}
	return palette.ClusterMeta{Name: name, UID: uid, Annotations: a, Deletion: del}
}

func sample() *Index {
	src := fakeSrc{
		metas: []palette.ClusterMeta{
			meta("alpha", "u1", "p1", "cluster.get,"+AdminPermission, nil, "0001-01-01T00:00:00.000Z"),
			meta("alpine", "u2", "p2", "cluster.get", nil, ""),
			meta("dup", "u3", "p1", "", nil, ""),
			meta("dup", "u4", "p2", "", nil, ""),
			meta("gone", "u5", "p1", "", map[string]string{"deleted": "true"}, ""),
			meta("gone2", "u6", "p1", "", nil, "2026-11-01T00:00:00Z"),
			meta("orphan", "u7", "", "", nil, ""),
		},
		projects: []palette.Project{{UID: "p1", Name: "Default"}, {UID: "p2", Name: "Dev"}},
	}
	i, _ := Build(context.Background(), src, time.Now())
	return i
}

func TestBuildFiltersDeletedAndMapsProjects(t *testing.T) {
	i := sample()
	if len(i.Clusters) != 5 {
		t.Fatalf("got %d clusters: %+v", len(i.Clusters), i.Clusters)
	}
	var alpha, orphan Cluster
	for _, c := range i.Clusters {
		switch c.Name {
		case "gone", "gone2":
			t.Errorf("deleted cluster %s kept", c.Name)
		case "alpha":
			alpha = c
		case "orphan":
			orphan = c
		}
	}
	if alpha.Project != "Default" || !alpha.CanAdmin() {
		t.Errorf("alpha: %+v", alpha)
	}
	if orphan.Project != "tenant" {
		t.Errorf("orphan project = %q", orphan.Project)
	}
	if i.Clusters[1].CanAdmin() {
		t.Errorf("admin permission leaked to %+v", i.Clusters[1])
	}
}

func TestBuildProjectsFailureFallsBackToUID(t *testing.T) {
	i, err := Build(context.Background(), fakeSrc{
		metas: []palette.ClusterMeta{meta("a", "u", "p1", "", nil, "")}, perr: errors.New("boom"),
	}, time.Now())
	if err != nil || i.Clusters[0].Project != "p1" {
		t.Fatalf("%+v %v", i, err)
	}
}

func TestResolve(t *testing.T) {
	i := sample()
	for _, tc := range []struct {
		name, q, want string // want is a ref, "ambiguous" or "missing"
	}{
		{"exact", "alpha", "Default/alpha"},
		{"unique prefix", "alp", "ambiguous"},
		{"longer prefix", "alph", "Default/alpha"},
		{"exact beats prefix", "alpine", "Dev/alpine"},
		{"collision", "dup", "ambiguous"},
		{"project/cluster", "Dev/dup", "Dev/dup"},
		{"missing", "zzz", "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := i.Resolve(tc.q)
			var amb *AmbiguousError
			switch tc.want {
			case "ambiguous":
				if !errors.As(err, &amb) {
					t.Fatalf("err = %v", err)
				}
			case "missing":
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err = %v", err)
				}
			default:
				if err != nil || c.Ref() != tc.want {
					t.Fatalf("got %v err %v", c.Ref(), err)
				}
			}
		})
	}
}

func TestSaveLoadStale(t *testing.T) {
	p := filepath.Join(t.TempDir(), "i.json")
	if _, err := Load(p); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("err = %v", err)
	}
	i := sample()
	if err := i.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || len(got.Clusters) != 5 {
		t.Fatal(err)
	}
	if got.Stale(time.Now()) || !got.Stale(time.Now().Add(11*time.Minute)) {
		t.Fatal("stale threshold wrong")
	}
}
