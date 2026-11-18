// Package indextest builds synthetic indexes for scale tests and benchmarks.
package indextest

import (
	"fmt"

	"github.com/craig-ai-tooling/kubectl-palette/internal/index"
)

// Synthetic returns an index of n clusters spread over the given number of projects.
// Every tenth cluster reuses a name that exists in another project, so resolution and
// completion meet collisions. Every seventh cluster carries the admin permission.
func Synthetic(n, projects int) *index.Index {
	idx := &index.Index{}
	for k := 0; k < n; k++ {
		p := k % projects
		name := fmt.Sprintf("cluster-%05d", k)
		if k%10 == 0 && k >= projects {
			// Same name as the cluster one project over, so the pair collides.
			name = fmt.Sprintf("cluster-%05d", k-1)
		}
		c := index.Cluster{
			Name: name, UID: fmt.Sprintf("uid-%08d", k),
			ProjectUID: fmt.Sprintf("puid-%03d", p), Project: fmt.Sprintf("project-%02d", p),
		}
		if k%7 == 0 {
			c.Permissions = []string{index.AdminPermission}
		}
		if k%13 == 0 {
			c.NoOIDC = true
		}
		idx.Clusters = append(idx.Clusters, c)
	}
	return idx
}
