package kubeconfig

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sample = `apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster:
    server: https://1.2.3.4:6443
    certificate-authority-data: Q0E=
contexts:
- name: ctx
  context: {cluster: c1, user: oidc}
current-context: ctx
users:
- name: oidc
  user:
    token: leaked-token
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: kubectl
      args:
      - oidc-login
      - get-token
      - --oidc-issuer-url=https://console.example.com/v1/oidc/tenant/T
      - --oidc-client-id=T
      - --oidc-client-secret=SeCrEt
      - --oidc-extra-scope=profile
      - --oidc-extra-scope=email
`

func TestParseOIDC(t *testing.T) {
	o, err := ParseOIDC([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if o.Issuer != "https://console.example.com/v1/oidc/tenant/T" || o.ClientID != "T" || o.ClientSecret != "SeCrEt" || len(o.ExtraScopes) != 2 {
		t.Fatalf("%+v", o)
	}
	spaced := strings.Replace(sample, "--oidc-client-secret=SeCrEt", "--oidc-client-secret\n      - Sp aced", 1)
	o, err = ParseOIDC([]byte(spaced))
	if err != nil || o.ClientSecret != "Sp aced" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestParseOIDCErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not yaml":    ":::\n\t-",
		"no users":    "apiVersion: v1\nusers: []\n",
		"cert user":   "users:\n- name: a\n  user:\n    client-certificate-data: abc\n",
		"missing url": "users:\n- name: a\n  user:\n    exec:\n      command: kubectl\n      args: [oidc-login, get-token]\n",
	} {
		if _, err := ParseOIDC([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRewriteLeavesNoSecret(t *testing.T) {
	out, err := Rewrite([]byte(sample), "", "SeCrEt", "leaked-token")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"SeCrEt", "leaked-token", "oidc-login", "--oidc-client-secret"} {
		if strings.Contains(s, bad) {
			t.Errorf("output still contains %q", bad)
		}
	}
	var d struct {
		Clusters []struct {
			Cluster struct{ Server string }
		}
		CurrentContext string `yaml:"current-context"`
		Users          []struct {
			User map[string]any
		}
	}
	if err := yaml.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	if d.Clusters[0].Cluster.Server != "https://1.2.3.4:6443" || d.CurrentContext != "ctx" {
		t.Fatalf("cluster data lost: %+v", d)
	}
	ex := d.Users[0].User["exec"].(map[string]any)
	if ex["command"] != "kubectl-palette" || ex["args"].([]any)[0] != "token" || len(d.Users[0].User) != 1 {
		t.Fatalf("user entry: %+v", d.Users[0].User)
	}
}

func TestRewriteRefusesWhenSecretSurvives(t *testing.T) {
	// the server URL carries the "secret": it is outside the user entry so it survives
	if _, err := Rewrite([]byte(sample), "", "1.2.3.4"); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := Rewrite([]byte("users:\n- name: a\n  user:\n    token: x\n"), ""); err != ErrNotOIDC {
		t.Fatalf("err = %v", err)
	}
}

// The default entry keeps the plain ["token"] arguments. A named entry spells itself out,
// so a cluster file never depends on which entry is active.
func TestRewriteExecArgsPerTenant(t *testing.T) {
	for _, tc := range []struct {
		tenant string
		want   []string
	}{
		{"", []string{"token"}},
		{"default", []string{"token"}},
		{"acme", []string{"token", "--tenant", "acme"}},
	} {
		out, err := Rewrite([]byte(sample), tc.tenant, "SeCrEt")
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Users []struct {
				User struct {
					Exec struct {
						Args []string `yaml:"args"`
					} `yaml:"exec"`
				} `yaml:"user"`
			} `yaml:"users"`
		}
		if err := yaml.Unmarshal(out, &d); err != nil {
			t.Fatal(err)
		}
		got := d.Users[0].User.Exec.Args
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("tenant %q: args %v, want %v", tc.tenant, got, tc.want)
		}
	}
}
