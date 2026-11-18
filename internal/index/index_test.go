package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func virtualSample() *Index {
	src := fakeSrc{
		metas: []palette.ClusterMeta{
			meta("host", "h1", "p1", "", nil, ""),
			meta("v1", "v1", "p1", "", map[string]string{"hostClusterUid": "h1"}, ""),
			meta("v2", "v2", "p2", "", map[string]string{"hostClusterUid": "h1"}, ""),
			meta("lost", "v3", "p1", "", map[string]string{"hostClusterUid": "missing"}, ""),
			meta("plain", "c1", "p1", "", nil, ""),
		},
		projects: []palette.Project{{UID: "p1", Name: "Default"}, {UID: "p2", Name: "Dev"}},
	}
	i, _ := Build(context.Background(), src, time.Now())
	return i
}

func TestBuildMarksVirtualClusters(t *testing.T) {
	i := virtualSample()
	got := map[string]Cluster{}
	for _, c := range i.Clusters {
		got[c.Name] = c
	}
	for _, n := range []string{"v1", "v2"} {
		if !got[n].Virtual() || got[n].HostUID != "h1" || got[n].Host != "Default/host" {
			t.Errorf("%s: %+v", n, got[n])
		}
	}
	if !got["lost"].Virtual() || got["lost"].HostUID != "missing" || got["lost"].Host != "" {
		t.Errorf("lost: %+v", got["lost"])
	}
	for _, n := range []string{"host", "plain"} {
		if got[n].Virtual() || got[n].Host != "" {
			t.Errorf("%s marked virtual: %+v", n, got[n])
		}
	}
}

func TestFilter(t *testing.T) {
	i := virtualSample()
	v := i.Filter(Cluster.Virtual)
	if len(v.Clusters) != 3 || !v.UpdatedAt.Equal(i.UpdatedAt) {
		t.Fatalf("%+v", v)
	}
	if len(i.Clusters) != 5 {
		t.Fatal("original changed")
	}
	if n := i.Filter(func(Cluster) bool { return false }); len(n.Clusters) != 0 {
		t.Fatalf("%+v", n)
	}
}

func TestHostedBy(t *testing.T) {
	i := virtualSample()
	got, err := i.HostedBy("host")
	if err != nil || len(got.Clusters) != 2 || got.Clusters[0].Name != "v1" || got.Clusters[1].Name != "v2" {
		t.Fatalf("hit: %+v %v", got, err)
	}
	got, err = i.HostedBy("Default/plain")
	if err != nil || len(got.Clusters) != 0 {
		t.Fatalf("no virtual clusters: %+v %v", got, err)
	}
	if _, err := i.HostedBy("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
}

func TestVirtualFieldsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "i.json")
	i := virtualSample()
	if err := i.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for n, c := range got.Clusters {
		if !reflect.DeepEqual(c, i.Clusters[n]) {
			t.Fatalf("cluster %d: %+v vs %+v", n, c, i.Clusters[n])
		}
	}
	h, _ := got.HostedBy("host")
	if len(h.Clusters) != 2 {
		t.Fatalf("%+v", h)
	}
}

func TestLoadOldFormatIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.json")
	old := `{"updated_at":"2026-11-01T00:00:00Z","clusters":[{"name":"a","uid":"u","project_uid":"p","project":"Default","permissions":["x"],"no_oidc":true}]}`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || len(got.Clusters) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	c := got.Clusters[0]
	if c.Virtual() || c.HostUID != "" || c.Host != "" || !c.NoOIDC {
		t.Fatalf("%+v", c)
	}
	if h, err := got.HostedBy("a"); err != nil || len(h.Clusters) != 0 {
		t.Fatalf("%+v %v", h, err)
	}
}
