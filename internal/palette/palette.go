// Package palette is a small client for the Palette API calls the plugin needs.
//
// The API session is the OIDC refresh token sent as a raw Authorization header (no Bearer).
package palette

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrNotFound is returned for a 404, for example a cluster without OIDC.
var ErrNotFound = errors.New("not found")

// Client calls the Palette API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Token returns the current API session token.
	Token func() (string, error)
	// Renew is called once after a 401 with the rejected token and returns a replacement.
	Renew func(ctx context.Context, rejected string) (string, error)
}

// ClusterMeta is one item of the dashboard metadata listing.
type ClusterMeta struct {
	Name        string `json:"name"`
	UID         string `json:"uid"`
	Annotations map[string]string
	Deletion    string
}

// HostClusterAnnotation is set on a virtual cluster. Its value is the UID of the cluster
// that hosts it.
const HostClusterAnnotation = "hostClusterUid"

// HostUID returns the UID of the hosting cluster, empty for a cluster that is not virtual.
func (m ClusterMeta) HostUID() string { return strings.TrimSpace(m.Annotations[HostClusterAnnotation]) }

// Project is a project's uid and name.
type Project struct {
	UID  string
	Name string
}

// do sends one request, with a single renew-and-retry on 401.
func (c *Client) do(ctx context.Context, method, path string, projectUID string, body []byte) ([]byte, error) {
	tok, err := c.Token()
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", tok)
		// No Accept header: with one, the kubeconfig asset endpoints answer in another format.
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if projectUID != "" {
			req.Header.Set("ProjectUid", projectUID)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("%s %s: read response: %w", method, path, rerr)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.Renew != nil:
			ntok, err := c.Renew(ctx, tok)
			if err != nil {
				return nil, err
			}
			tok = ntok
			continue
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, fmt.Errorf("%s %s: session rejected (401): run: kubectl palette login", method, path)
		case resp.StatusCode/100 != 2:
			// The body is not echoed: it can carry request details.
			return nil, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
		}
		return data, nil
	}
}

// Metadata lists every cluster the caller can see, in one unpaginated call.
func (c *Client) Metadata(ctx context.Context) ([]ClusterMeta, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/dashboard/spectroclusters/metadata", "", nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Items []struct {
			Name        string            `json:"name"`
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
			Deletion    string            `json:"deletionTimestamp"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse cluster metadata: %w", err)
	}
	out := make([]ClusterMeta, 0, len(raw.Items))
	for _, i := range raw.Items {
		out = append(out, ClusterMeta{Name: i.Name, UID: i.UID, Annotations: i.Annotations, Deletion: i.Deletion})
	}
	return out, nil
}

// Projects lists projects so project uids can be shown as names.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	b, err := c.do(ctx, http.MethodPost, "/v1/dashboard/projects", "", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse projects: %w", err)
	}
	out := make([]Project, 0, len(raw.Items))
	for _, i := range raw.Items {
		out = append(out, Project{UID: i.Metadata.UID, Name: i.Metadata.Name})
	}
	return out, nil
}

// KubeconfigClient fetches the OIDC kubeconfig. ErrNotFound means the cluster has no OIDC.
func (c *Client) KubeconfigClient(ctx context.Context, clusterUID, projectUID string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/spectroclusters/"+clusterUID+"/assets/kubeconfigclient", projectUID, nil)
}

// AdminKubeconfig fetches the admin kubeconfig (a static client certificate).
func (c *Client) AdminKubeconfig(ctx context.Context, clusterUID, projectUID string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/spectroclusters/"+clusterUID+"/assets/adminKubeconfig", projectUID, nil)
}
