package palette

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientCalls(t *testing.T) {
	var gotAuth, gotProject, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotProject = r.Header.Get("Authorization"), r.Header.Get("ProjectUid")
		b := make([]byte, 16)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		switch r.URL.Path {
		case "/v1/dashboard/spectroclusters/metadata":
			w.Write([]byte(`{"items":[{"name":"a","uid":"u","annotations":{"projectUid":"p","permissions":"x,y"},"deletionTimestamp":"0001-01-01T00:00:00.000Z"}]}`))
		case "/v1/dashboard/projects":
			w.Write([]byte(`{"items":[{"metadata":{"name":"Default","uid":"p"}}]}`))
		case "/v1/spectroclusters/u/assets/kubeconfigclient":
			w.Write([]byte("kc"))
		case "/v1/spectroclusters/nooidc/assets/kubeconfigclient":
			http.NotFound(w, r)
		default:
			http.Error(w, "secret-detail", 500)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", HTTP: srv.Client(), Token: func() (string, error) { return "RT", nil }}
	ctx := context.Background()

	m, err := c.Metadata(ctx)
	if err != nil || len(m) != 1 || m[0].Annotations["permissions"] != "x,y" || gotAuth != "RT" || gotProject != "" {
		t.Fatalf("metadata %+v %v auth=%q", m, err, gotAuth)
	}
	p, err := c.Projects(ctx)
	if err != nil || p[0].Name != "Default" || gotBody != "{}" {
		t.Fatalf("projects %+v %v body=%q", p, err, gotBody)
	}
	b, err := c.KubeconfigClient(ctx, "u", "p")
	if err != nil || string(b) != "kc" || gotProject != "p" {
		t.Fatalf("kubeconfig %q %v project=%q", b, err, gotProject)
	}
	if _, err := c.KubeconfigClient(ctx, "nooidc", "p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	_, err = c.AdminKubeconfig(ctx, "u", "p")
	if err == nil || strings.Contains(err.Error(), "secret-detail") {
		t.Fatalf("error must not echo the body: %v", err)
	}
}

func TestRenewOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "new" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	renews := 0
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: func() (string, error) { return "old", nil },
		Renew: func(_ context.Context, rejected string) (string, error) {
			renews++
			if rejected != "old" {
				t.Errorf("rejected = %q", rejected)
			}
			return "new", nil
		}}
	if _, err := c.Metadata(context.Background()); err != nil || calls.Load() != 2 || renews != 1 {
		t.Fatalf("err %v calls %d renews %d", err, calls.Load(), renews)
	}
	// a token that is still rejected after renewing fails instead of looping
	c.Renew = func(context.Context, string) (string, error) { return "still-bad", nil }
	calls.Store(0)
	if _, err := c.Metadata(context.Background()); err == nil || !strings.Contains(err.Error(), "kubectl palette login") || calls.Load() != 2 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
}

func TestMetadataHostClusterAnnotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"name":"v","uid":"v1","annotations":{"hostClusterUid":"h1"}},{"name":"h","uid":"h1","annotations":{}},{"name":"n","uid":"n1"}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: func() (string, error) { return "RT", nil }}
	m, err := c.Metadata(context.Background())
	if err != nil || len(m) != 3 {
		t.Fatalf("%+v %v", m, err)
	}
	if m[0].HostUID() != "h1" || m[1].HostUID() != "" || m[2].HostUID() != "" {
		t.Fatalf("%+v", m)
	}
}
