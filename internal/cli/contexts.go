package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
	"github.com/craig-ai-tooling/kubectl-palette/internal/kubectx"
	"github.com/craig-ai-tooling/kubectl-palette/internal/palette"
)

const (
	// maxAutoContexts is the largest tenant that login adds every cluster of. A bigger
	// one adds contexts as clusters are used, so login stays fast.
	maxAutoContexts = 25
	// fetchParallelism bounds the concurrent kubeconfig downloads of a bulk add.
	fetchParallelism = 8
)

// contextsEnabled reports whether persistent contexts are on. A Paths with no context
// file counts as off, so nothing can reach a kubeconfig that was never chosen.
func (a *App) contextsEnabled() bool {
	if a.Paths.ContextFile() == "" {
		return false
	}
	v := a.getenv(config.EnvNoContexts)
	return v != "1" && !strings.EqualFold(v, "true")
}

// shortPath writes a path under the home directory as ~/...
func shortPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (a *App) warnf(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "warning: "+format+"\n", args...)
}

// ctxEntry is the kubectx identity of a cluster in the selected tenant entry.
func (a *App) ctxEntry(c index.Cluster) kubectx.Entry {
	return kubectx.Entry{Tenant: a.Paths.Entry(), Project: c.Project, Cluster: c.Name, UID: c.UID, IssuerID: a.entryIssuerID()}
}

// recordContext adds one cluster to the user's kubeconfig as a context and returns the
// context name, "" when there is none. path is the cluster's OIDC file and fetched says
// whether this call downloaded it. A warm use with the context still in the file, as
// written, writes nothing. A context the user removed or re-pointed is written again, under
// a free name when the old one is taken, so `use` never selects someone else's cluster. A failure is a
// warning, never an error: the command that called it already did its job. Admin mode must
// not call this.
func (a *App) recordContext(c index.Cluster, path string, fetched bool) string {
	if !a.contextsEnabled() {
		return ""
	}
	entry := a.Paths.Entry()
	reg, file := a.Paths.ContextRegistry(), a.Paths.ContextFile()
	old, had, err := kubectx.Lookup(file, reg, entry, c.UID)
	if err == nil && had && !fetched && kubectx.OwnsContext(file, old) {
		return old.Context
	}
	src, err := os.ReadFile(path)
	if err != nil {
		a.warnf("could not add a context for %s: %v", c.Ref(), err)
		return ""
	}
	rec, err := kubectx.Upsert(file, reg, a.ctxEntry(c), src, a.now())
	if err != nil {
		a.warnf("could not add a context for %s: %v", c.Ref(), err)
		return ""
	}
	if !had {
		fmt.Fprintf(a.Stderr, "added context %s to %s\n", rec.Context, shortPath(file))
		a.noteKubeconfigEnv()
	}
	return rec.Context
}

// noteKubeconfigEnv prints one note when a context was added to a file the shell's
// $KUBECONFIG does not list, because kubectl in that shell will not see it. A terminal
// pinned with `use` points $KUBECONFIG at a plugin file and needs no note.
func (a *App) noteKubeconfigEnv() {
	kc := a.getenv("KUBECONFIG")
	if a.envNoted || kc == "" {
		return
	}
	file := a.Paths.ContextFile()
	kube := a.Paths.Root().KubeDir
	for _, p := range filepath.SplitList(kc) {
		if p == "" {
			continue
		}
		if ap, err := filepath.Abs(p); err == nil {
			p = ap
		}
		if af, err := filepath.Abs(file); err == nil && p == af {
			return
		}
		if rel, err := filepath.Rel(kube, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
	}
	a.envNoted = true
	fmt.Fprintf(a.Stderr, "note: $KUBECONFIG is set, so kubectl in this shell reads it and will not see contexts in %s; add that file to $KUBECONFIG or unset it\n", shortPath(file))
}

// clusterFile is the outcome of getting one cluster's OIDC kubeconfig.
type clusterFile struct {
	Cluster index.Cluster
	Path    string
	Fetched bool
	NoOIDC  bool
	Err     error
}

// ensureOIDCFiles makes sure every cluster has its OIDC kubeconfig on disk, downloading
// the missing ones fetchParallelism at a time. It never writes the index: the caller
// passes the result to recordOIDCBatch.
func (a *App) ensureOIDCFiles(ctx context.Context, clusters []index.Cluster, refresh bool) []clusterFile {
	out := make([]clusterFile, len(clusters))
	var (
		once sync.Once
		cl   *palette.Client
		cerr error
	)
	client := func() (*palette.Client, error) {
		once.Do(func() { cl, cerr = a.apiClient() })
		return cl, cerr
	}
	sem := make(chan struct{}, fetchParallelism)
	var wg sync.WaitGroup
	for i, c := range clusters {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			out[i] = a.oneOIDCFile(ctx, client, c, refresh)
		}()
	}
	wg.Wait()
	return out
}

func (a *App) oneOIDCFile(ctx context.Context, client func() (*palette.Client, error), c index.Cluster, refresh bool) clusterFile {
	r := clusterFile{Cluster: c}
	path, err := a.Paths.ClusterFile(c.Project, c.Name, false)
	if err != nil {
		r.Err = err
		return r
	}
	if _, serr := os.Stat(path); serr == nil {
		if !refresh && c.UID != "" && config.CachedUID(path) == c.UID {
			r.Path = path
			return r
		}
		if rerr := config.RemoveCluster(path); rerr != nil {
			r.Err = fmt.Errorf("remove stale kubeconfig: %w", rerr)
			return r
		}
	}
	cl, err := client()
	if err != nil {
		r.Err = err
		return r
	}
	raw, err := a.fetchOIDC(ctx, cl, c)
	switch {
	case errors.Is(err, errNoOIDC):
		r.NoOIDC = true
		return r
	case err != nil:
		r.Err = err
		return r
	}
	if err := config.WriteFileAtomic(path, raw); err != nil {
		r.Err = err
		return r
	}
	if err := config.WriteCachedUID(path, c.UID); err != nil {
		_ = config.RemoveCluster(path)
		r.Err = err
		return r
	}
	r.Path, r.Fetched = path, true
	return r
}

// recordOIDCBatch saves what a bulk fetch learned about OIDC in one index write.
func (a *App) recordOIDCBatch(files []clusterFile) {
	idx, err := index.Load(a.Paths.IndexFile())
	if err != nil {
		return
	}
	changed := false
	for _, f := range files {
		switch {
		case f.NoOIDC:
			changed = idx.SetNoOIDC(f.Cluster.UID, true) || changed
		case f.Err == nil && f.Cluster.NoOIDC:
			changed = idx.SetNoOIDC(f.Cluster.UID, false) || changed
		}
	}
	if changed {
		_ = idx.Save(a.Paths.IndexFile())
	}
}

// ctxOutcome counts what a bulk add did.
type ctxOutcome struct {
	Added, Present, NoOIDC int
	Failed                 []string // "project/cluster: reason"
}

// recordContexts upserts every file that has an OIDC kubeconfig with one UpsertAll.
func (a *App) recordContexts(files []clusterFile) (ctxOutcome, error) {
	var out ctxOutcome
	reg, file := a.Paths.ContextRegistry(), a.Paths.ContextFile()
	entry := a.Paths.Entry()
	prior, err := kubectx.ListFor(file, reg)
	if err != nil {
		return out, err
	}
	had := map[string]bool{}
	for _, r := range prior {
		if r.Tenant == entry {
			had[r.UID] = true
		}
	}
	var items []kubectx.Item
	var owners []clusterFile
	for _, f := range files {
		switch {
		case f.Err != nil:
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", f.Cluster.Ref(), f.Err))
		case f.NoOIDC:
			out.NoOIDC++
		default:
			src, rerr := os.ReadFile(f.Path)
			if rerr != nil {
				out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", f.Cluster.Ref(), rerr))
				continue
			}
			items = append(items, kubectx.Item{Entry: a.ctxEntry(f.Cluster), Source: src})
			owners = append(owners, f)
		}
	}
	res, err := kubectx.UpsertAll(file, reg, items, a.now())
	if err != nil {
		return out, err
	}
	for i, r := range res {
		switch {
		case r.Err != nil:
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", owners[i].Cluster.Ref(), r.Err))
		case had[owners[i].Cluster.UID]:
			out.Present++
		default:
			out.Added++
		}
	}
	return out, nil
}

// summary is the one line printed after a bulk add.
func (o ctxOutcome) summary(file string) string {
	var extra []string
	if o.NoOIDC > 0 {
		if o.NoOIDC == 1 {
			extra = append(extra, "1 cluster has no OIDC")
		} else {
			extra = append(extra, fmt.Sprintf("%d clusters have no OIDC", o.NoOIDC))
		}
	}
	if o.Present > 0 {
		extra = append(extra, fmt.Sprintf("%d already present", o.Present))
	}
	if n := len(o.Failed); n > 0 {
		extra = append(extra, fmt.Sprintf("%d failed", n))
	}
	line := "no new contexts in " + shortPath(file)
	if o.Added > 0 {
		line = fmt.Sprintf("added %s to %s", plural(o.Added, "context", "contexts"), shortPath(file))
	}
	if len(extra) > 0 {
		line += " (" + strings.Join(extra, ", ") + ")"
	}
	return line
}

// autoContexts adds every cluster of a freshly indexed tenant to the kubeconfig when the
// tenant is small enough. Nothing here fails the login.
func (a *App) autoContexts(ctx context.Context, idx *index.Index, skip bool) {
	if skip || !a.contextsEnabled() || len(idx.Clusters) == 0 {
		return
	}
	if n := len(idx.Clusters); n > maxAutoContexts {
		fmt.Fprintf(a.Stderr, "%d clusters: contexts are added as you use them, or run: kubectl palette contexts add --all\n", n)
		return
	}
	files := a.ensureOIDCFiles(ctx, idx.Clusters, false)
	a.recordOIDCBatch(files)
	out, err := a.recordContexts(files)
	if err != nil {
		a.warnf("could not add contexts: %v", err)
		return
	}
	fmt.Fprintln(a.Stderr, out.summary(a.Paths.ContextFile()))
	if out.Added > 0 {
		a.noteKubeconfigEnv()
	}
	for _, f := range out.Failed {
		fmt.Fprintf(a.Stderr, "  %s\n", f)
	}
}

// removeContexts drops the records match accepts, with their kubeconfig entries.
func (a *App) removeContexts(match func(kubectx.Record) bool) ([]kubectx.Removal, error) {
	if !a.contextsEnabled() {
		return nil, nil
	}
	return kubectx.RemoveDetailed(a.Paths.ContextFile(), a.Paths.ContextRegistry(), match)
}

// deletedCount is how many removals deleted kubeconfig entries.
func deletedCount(rs []kubectx.Removal) int {
	n := 0
	for _, r := range rs {
		if r.EntriesDeleted {
			n++
		}
	}
	return n
}

// removeEntryContexts removes every context of one tenant entry. A failure is a warning.
func (a *App) removeEntryContexts(entry string) int {
	recs, err := a.removeContexts(func(r kubectx.Record) bool { return r.Tenant == entry })
	if err != nil {
		a.warnf("could not remove the contexts of %s from %s: %v\n  later: kubectl palette contexts remove --all --all-tenants", entry, shortPath(a.Paths.ContextFile()), err)
	}
	return deletedCount(recs)
}

// --- the contexts command ---

const contextsUsage = `usage: kubectl palette contexts [list]
       kubectl palette contexts add <name>... | --all [--host <cluster>] [--tenant <name>]
       kubectl palette contexts remove <context>... | --all [--all-tenants] [--tenant <name>]
       kubectl palette contexts prune [--tenant <name>]`

func (a *App) contextsCmd(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if a.Paths.ContextFile() == "" || !a.contextsEnabled() {
		return fmt.Errorf("contexts are turned off (%s=1 or no context file)", config.EnvNoContexts)
	}
	switch sub {
	case "list":
		return a.contextsList(args)
	case "add":
		return a.contextsAdd(ctx, args)
	case "remove":
		return a.contextsRemove(args)
	case "prune":
		return a.contextsPrune(ctx, args)
	}
	return fmt.Errorf("unknown contexts command %q\n%s", sub, contextsUsage)
}

func (a *App) contextsList(args []string) error {
	fs := newFlags("contexts list", a.Stderr)
	if pos, err := parseFlags(fs, args); err != nil {
		return exitError(2)
	} else if len(pos) > 0 {
		return errors.New(contextsUsage)
	}
	recs, err := kubectx.List(a.Paths.ContextRegistry())
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Fprintln(a.Stderr, "no contexts yet: they are added at login and as you use clusters")
		return nil
	}
	cur, _ := filepath.Abs(a.Paths.ContextFile())
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	for _, r := range recs {
		line := fmt.Sprintf("%s\t%s\t%s/%s", r.Context, r.Tenant, r.Project, r.Cluster)
		if r.File != "" && r.File != cur {
			line += "\t" + shortPath(r.File)
		}
		fmt.Fprintln(tw, line)
	}
	return tw.Flush()
}

func (a *App) contextsAdd(ctx context.Context, args []string) error {
	fs := newFlags("contexts add", a.Stderr)
	all := fs.Bool("all", false, "add every cluster of the tenant")
	host := fs.String("host", "", "only the virtual clusters hosted by this cluster")
	tenant := fs.String("tenant", "", "tenant entry to use")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitError(2)
	}
	if *all == (len(pos) > 0) {
		return errors.New("name the clusters to add, or pass --all\n" + contextsUsage)
	}
	if err := a.selectTenant(*tenant); err != nil {
		return err
	}
	idx, fresh, err := a.loadOrBuildIndex(ctx)
	if err != nil {
		return err
	}
	var clusters []index.Cluster
	if *all {
		scope := idx
		if *host != "" {
			if scope, err = hostedIndex(idx, *host); err != nil {
				return err
			}
		}
		clusters = scope.Clusters
		if len(clusters) == 0 {
			return errors.New("the index has no clusters: run: kubectl palette sync")
		}
	} else {
		seen := map[string]bool{}
		for _, name := range pos {
			var c index.Cluster
			c, idx, fresh, err = a.resolve(ctx, idx, fresh, name, *host)
			if err != nil {
				return err
			}
			if !seen[c.UID] {
				seen[c.UID] = true
				clusters = append(clusters, c)
			}
		}
	}
	files := a.ensureOIDCFiles(ctx, clusters, false)
	a.recordOIDCBatch(files)
	out, err := a.recordContexts(files)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stderr, a.entryPrefix()+out.summary(a.Paths.ContextFile()))
	if out.Added > 0 {
		a.noteKubeconfigEnv()
	}
	for _, f := range out.Failed {
		fmt.Fprintf(a.Stderr, "  failed: %s\n", f)
	}
	if len(out.Failed) > 0 {
		return exitError(1)
	}
	return nil
}

func (a *App) contextsRemove(args []string) error {
	fs := newFlags("contexts remove", a.Stderr)
	all := fs.Bool("all", false, "remove every context of the tenant")
	allTenants := fs.Bool("all-tenants", false, "with --all, remove the contexts of every tenant")
	tenant := fs.String("tenant", "", "tenant entry to use with --all")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitError(2)
	}
	if *all == (len(pos) > 0) {
		return errors.New("name the contexts to remove, or pass --all\n" + contextsUsage)
	}
	if *allTenants && !*all {
		return errors.New("--all-tenants goes with --all")
	}
	var match func(kubectx.Record) bool
	var missing []string
	switch {
	case *all && *allTenants:
		match = func(kubectx.Record) bool { return true }
	case *all:
		entry, err := a.entryOrOrphan(*tenant)
		if err != nil {
			return err
		}
		match = func(r kubectx.Record) bool { return r.Tenant == entry }
	default:
		recs, err := kubectx.ListFor(a.Paths.ContextFile(), a.Paths.ContextRegistry())
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, r := range recs {
			known[r.Context] = true
		}
		want := map[string]bool{}
		for _, n := range pos {
			if known[n] {
				want[n] = true
			} else {
				missing = append(missing, n)
			}
		}
		match = func(r kubectx.Record) bool { return want[r.Context] }
	}
	removed, err := a.removeContexts(match)
	if err != nil {
		return err
	}
	a.printRemoved(removed)
	if len(missing) > 0 {
		return fmt.Errorf("no such context recorded by this plugin: %s (see: kubectl palette contexts)", strings.Join(missing, ", "))
	}
	return nil
}

// entryOrOrphan resolves a --tenant value for a removal. A name with no saved entry is
// accepted when the registry still holds contexts for it, so they can be cleaned up.
func (a *App) entryOrOrphan(flagVal string) (string, error) {
	entries := a.Paths.Root().Entries()
	if flagVal == "" {
		return a.pickEntry("")
	}
	if m, ok := config.MatchEntry(entries, flagVal); ok {
		return m, nil
	}
	lower := strings.ToLower(flagVal)
	recs, err := kubectx.ListFor(a.Paths.ContextFile(), a.Paths.ContextRegistry())
	if err != nil {
		return "", err
	}
	for _, r := range recs {
		if r.Tenant == lower {
			return lower, nil
		}
	}
	return "", unknownEntryErr(flagVal, entries)
}

func (a *App) printRemoved(rs []kubectx.Removal) {
	var names, left []string
	for _, r := range rs {
		if r.EntriesDeleted {
			names = append(names, r.Record.Context)
		} else {
			left = append(left, r.Record.Context)
		}
	}
	sort.Strings(names)
	sort.Strings(left)
	if len(names) > 0 || len(left) == 0 {
		line := "removed " + plural(len(names), "context", "contexts") + " from " + shortPath(a.Paths.ContextFile())
		if len(names) > 0 && len(names) <= 10 {
			line += ": " + strings.Join(names, ", ")
		}
		fmt.Fprintln(a.Stderr, line)
	}
	for _, n := range left {
		fmt.Fprintf(a.Stderr, "left %s in place (you changed it), no longer tracked\n", n)
	}
}

func (a *App) contextsPrune(ctx context.Context, args []string) error {
	fs := newFlags("contexts prune", a.Stderr)
	tenant := fs.String("tenant", "", "tenant entry to use")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitError(2)
	}
	if len(pos) > 0 {
		return errors.New(contextsUsage)
	}
	root := a.Paths.Root()
	entries := root.Entries()
	// Contexts of a tenant entry that is gone, or of a login the entry no longer holds, are
	// always stale. Only the selected entry's clusters are checked against a fresh index.
	ids := map[string]string{}
	for _, e := range entries {
		if c, err := root.ForTenant(e).Load(); err == nil && c.Issuer != "" {
			ids[e] = config.IssuerID(c.Issuer)
		}
	}
	entry, indexed := "", false
	live := map[string]bool{}
	if *tenant != "" {
		if _, ok := config.MatchEntry(entries, *tenant); !ok {
			// No saved entry of that name: only the orphan cleanup applies.
			if _, err := a.entryOrOrphan(*tenant); err != nil {
				return err
			}
		}
	}
	if *tenant != "" || len(entries) > 0 {
		if _, ok := config.MatchEntry(entries, *tenant); ok || *tenant == "" {
			if err := a.selectTenant(*tenant); err != nil {
				return err
			}
			idx, err := a.buildIndex(ctx)
			if err != nil {
				return err
			}
			entry, indexed = a.Paths.Entry(), true
			for _, c := range idx.Clusters {
				live[c.UID] = true
			}
		}
	}
	removed, err := kubectx.PruneDetailed(a.Paths.ContextFile(), a.Paths.ContextRegistry(), func(r kubectx.Record) bool {
		if !contains(entries, r.Tenant) {
			return false
		}
		if id, ok := ids[r.Tenant]; ok && r.IssuerID != id && !(r.IssuerID == "" && r.Tenant == config.DefaultEntry) {
			return false
		}
		return !indexed || r.Tenant != entry || live[r.UID]
	})
	if err != nil {
		return err
	}
	a.printRemoved(removed)
	return nil
}
