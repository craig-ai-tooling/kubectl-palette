package complete

import (
	"reflect"
	"testing"

	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
)

func sample() *index.Index {
	return &index.Index{Clusters: []index.Cluster{
		{Name: "payments-prod", Project: "Default"},
		{Name: "payments-dev", Project: "Default"},
		{Name: "web", Project: "Default"},
		{Name: "web", Project: "Team-B"},
		{Name: "solo", Project: "Team-B"},
	}}
}

func TestComplete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []string
		want  []string
	}{
		{"subcommands", []string{"s"}, []string{"sync", "shell-init"}},
		{"all subcommands", []string{""}, Commands},
		{"unique names, collisions as refs", []string{"use", ""},
			[]string{"Default/web", "Team-B/web", "payments-dev", "payments-prod", "solo"}},
		{"prefix", []string{"use", "pay"}, []string{"payments-dev", "payments-prod"}},
		{"colliding name by prefix gives refs", []string{"use", "we"}, []string{"Default/web", "Team-B/web"}},
		{"project prefix", []string{"use", "Team-B/"}, []string{"Team-B/solo", "Team-B/web"}},
		{"exec first positional", []string{"exec", "so"}, []string{"solo"}},
		{"flag before name", []string{"exec", "--admin", "so"}, []string{"solo"}},
		{"second positional is not a cluster", []string{"use", "solo", ""}, nil},
		{"after double dash is kubectl's", []string{"exec", "solo", "--", "ge"}, nil},
		{"use flags", []string{"use", "--"}, []string{"--admin", "--no-fuzzy", "--refresh", "--tenant"}},
		{"exec flags", []string{"exec", "--a"}, []string{"--admin"}},
		{"completion", []string{"completion", ""}, []string{"install"}},
		{"completion flag", []string{"completion", "install", "--"}, []string{"--dir"}},
		{"shell-init", []string{"shell-init", "z"}, []string{"zsh"}},
		{"unknown", []string{"nope", ""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Complete(sample(), tc.words)
			if tc.name == "subcommands" || tc.name == "all subcommands" {
				// command order is the declared order, not sorted
				if !reflect.DeepEqual(got, filter(tc.want, tc.words[0])) {
					t.Fatalf("got %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestNilIndex(t *testing.T) {
	if got := Complete(nil, []string{"use", ""}); got != nil {
		t.Fatal(got)
	}
}

func TestNoPluginFlagsAfterDoubleDash(t *testing.T) {
	for _, words := range [][]string{
		{"exec", "solo", "--", "--a"},
		{"exec", "solo", "--", "-"},
		{"use", "--", "--a"},
	} {
		if got := Complete(sample(), words); got != nil {
			t.Errorf("Complete(%v) = %v, want nothing", words, got)
		}
	}
}

func TestCompleteTenants(t *testing.T) {
	entries := []string{"default", "acme", "globex"}
	for _, tc := range []struct {
		name  string
		words []string
		want  []string
	}{
		{"tenant positional", []string{"tenant", ""}, entries},
		{"tenant positional prefix", []string{"tenant", "g"}, []string{"globex"}},
		{"tenant takes one name", []string{"tenant", "acme", ""}, nil},
		{"--tenant value on use", []string{"use", "--tenant", ""}, entries},
		{"--tenant value prefix on exec", []string{"exec", "--tenant", "a"}, []string{"acme"}},
		{"--tenant value on logout", []string{"logout", "--tenant", "g"}, []string{"globex"}},
		{"--tenant value on login", []string{"login", "--tenant", "a"}, []string{"acme"}},
		{"--tenant=value", []string{"use", "--tenant=g"}, []string{"--tenant=globex"}},
		{"cluster after --tenant value", []string{"use", "--tenant", "acme", "pay"}, []string{"payments-dev", "payments-prod"}},
		{"sync flags", []string{"sync", "--t"}, []string{"--tenant"}},
		{"logout flags", []string{"logout", "--"}, []string{"--all", "--tenant"}},
		{"tenants flags", []string{"tenants", "--"}, []string{"--available"}},
		{"login server flag", []string{"login", "--s"}, []string{"--server"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompleteWith(sample(), entries, tc.words)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
