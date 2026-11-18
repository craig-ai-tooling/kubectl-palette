// Package complete answers shell completion from the local index. It makes no network
// call and touches no file except the index, so it stays inside the 50 ms budget.
package complete

import (
	"sort"
	"strings"

	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
)

// DirectiveNoFileComp is cobra's ShellCompDirectiveNoFileComp. kubectl passes the
// directive through to the shell, and the value 4 stops the shell offering file names.
const DirectiveNoFileComp = 4

// Commands are the subcommands offered at the first word. Hidden ones are left out.
var Commands = []string{"login", "tenants", "tenant", "use", "exec", "current", "sync", "logout", "token", "shell-init", "completion", "version"}

// flags lists the flags completed after a command name.
var flags = map[string][]string{
	"use":     {"--admin", "--no-fuzzy", "--refresh", "--tenant"},
	"exec":    {"--admin", "--refresh", "--tenant"},
	"sync":    {"--tenant"},
	"logout":  {"--all", "--tenant"},
	"token":   {"--tenant"},
	"tenants": {"--available"},
	"login": {"--server", "--tenant", "--name", "--ca-file", "--from-kubeconfig", "--issuer", "--client-id",
		"--client-secret", "--api-url", "--no-browser"},
}

// tenantFlag is the flag whose value is a tenant entry name.
const tenantFlag = "--tenant"

// Complete returns the candidates for the last word in words. words are the arguments
// typed after the plugin name, ending with the word under the cursor (possibly empty).
// idx may be nil when no index exists yet.
func Complete(idx *index.Index, words []string) []string {
	return CompleteWith(idx, nil, words)
}

// CompleteWith is Complete plus the saved tenant entry names, offered after --tenant and
// for `tenant <TAB>`.
func CompleteWith(idx *index.Index, entries []string, words []string) []string {
	if len(words) == 0 {
		return filter(Commands, "")
	}
	cur := words[len(words)-1]
	if len(words) == 1 {
		return filter(Commands, cur)
	}
	cmd, prior := words[0], words[1:len(words)-1]
	if _, ok := flags[cmd]; ok || cmd == "tenant" {
		if v, ok := strings.CutPrefix(cur, tenantFlag+"="); ok {
			var out []string
			for _, e := range filter(entries, v) {
				out = append(out, tenantFlag+"="+e)
			}
			return out
		}
		if len(prior) > 0 && prior[len(prior)-1] == tenantFlag && cmd != "tenant" {
			return filter(entries, cur)
		}
	}
	switch cmd {
	case "tenant":
		if len(prior) == 0 && !strings.HasPrefix(cur, "-") {
			return filter(entries, cur)
		}
	case "tenants", "sync", "logout", "token":
		if strings.HasPrefix(cur, "-") {
			return filter(flags[cmd], cur)
		}
	case "use", "exec":
		for _, w := range prior {
			if w == "--" {
				return nil // everything after the boundary belongs to kubectl
			}
		}
		if strings.HasPrefix(cur, "-") {
			return filter(flags[cmd], cur)
		}
		if !firstName(prior) {
			return nil
		}
		return Clusters(idx, cur)
	case "completion":
		if len(prior) == 0 {
			return filter([]string{"install"}, cur)
		}
		if strings.HasPrefix(cur, "-") {
			return filter([]string{"--dir"}, cur)
		}
	case "shell-init":
		if len(prior) == 0 {
			return filter([]string{"bash", "zsh"}, cur)
		}
	case "login":
		if strings.HasPrefix(cur, "-") {
			return filter(flags[cmd], cur)
		}
	}
	return nil
}

// firstName reports whether the words before the cursor leave the cluster name still to
// be typed: no positional yet, and no "--" (what follows it belongs to kubectl).
func firstName(prior []string) bool {
	for i := 0; i < len(prior); i++ {
		w := prior[i]
		if w == tenantFlag {
			i++ // its value is not a cluster name
			continue
		}
		if w == "--" || !strings.HasPrefix(w, "-") {
			return false
		}
	}
	return true
}

// Clusters returns the names that start with prefix. A name used in more than one
// project is offered only as project/cluster, because the bare name would be ambiguous.
// A prefix containing "/" matches project/cluster forms.
func Clusters(idx *index.Index, prefix string) []string {
	if idx == nil {
		return nil
	}
	count := make(map[string]int, len(idx.Clusters))
	for _, c := range idx.Clusters {
		count[c.Name]++
	}
	seen := make(map[string]bool, len(idx.Clusters))
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, c := range idx.Clusters {
		ref := c.Ref()
		switch {
		case strings.Contains(prefix, "/"):
			if strings.HasPrefix(ref, prefix) {
				add(ref)
			}
		case count[c.Name] > 1:
			// Offer the full form when the user typed part of the name or the project.
			if strings.HasPrefix(c.Name, prefix) || strings.HasPrefix(ref, prefix) {
				add(ref)
			}
		default:
			if strings.HasPrefix(c.Name, prefix) {
				add(c.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func filter(in []string, prefix string) []string {
	var out []string
	for _, s := range in {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}
