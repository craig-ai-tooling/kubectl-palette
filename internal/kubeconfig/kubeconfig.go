// Package kubeconfig reads Palette OIDC kubeconfigs and rewrites them so that no secret
// stays in the file.
package kubeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExecCommand is the credential plugin every rewritten kubeconfig calls.
const ExecCommand = "kubectl-palette"

// OIDC holds the values kubelogin (oidc-login) was configured with.
type OIDC struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	ExtraScopes  []string
}

// ErrNotOIDC means the kubeconfig has no oidc-login exec user.
var ErrNotOIDC = errors.New("kubeconfig has no oidc-login exec user")

type doc = map[string]any

func parse(raw []byte) (doc, error) {
	var d doc
	if err := yaml.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if d == nil {
		return nil, errors.New("parse kubeconfig: empty document")
	}
	return d, nil
}

func users(d doc) []any {
	us, _ := d["users"].([]any)
	return us
}

func execOf(u any) (map[string]any, bool) {
	um, _ := u.(map[string]any)
	user, _ := um["user"].(map[string]any)
	ex, ok := user["exec"].(map[string]any)
	return ex, ok
}

func argStrings(ex map[string]any) []string {
	var out []string
	if l, ok := ex["args"].([]any); ok {
		for _, a := range l {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func isOIDCLogin(args []string) bool {
	for _, a := range args {
		if a == "oidc-login" {
			return true
		}
	}
	return false
}

// ParseOIDC extracts the issuer, client id, client secret and extra scopes from the
// oidc-login get-token arguments of the first OIDC user.
func ParseOIDC(raw []byte) (OIDC, error) {
	var o OIDC
	d, err := parse(raw)
	if err != nil {
		return o, err
	}
	for _, u := range users(d) {
		ex, ok := execOf(u)
		if !ok {
			continue
		}
		args := argStrings(ex)
		if !isOIDCLogin(args) {
			continue
		}
		for i := 0; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "--") {
				continue
			}
			name, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				val, hasVal = args[i+1], true
				i++
			}
			if !hasVal {
				continue
			}
			switch name {
			case "oidc-issuer-url":
				o.Issuer = val
			case "oidc-client-id":
				o.ClientID = val
			case "oidc-client-secret":
				o.ClientSecret = val
			case "oidc-extra-scope":
				o.ExtraScopes = append(o.ExtraScopes, val)
			}
		}
		if o.Issuer == "" || o.ClientID == "" {
			return o, errors.New("oidc-login arguments lack --oidc-issuer-url or --oidc-client-id")
		}
		return o, nil
	}
	return o, ErrNotOIDC
}

// tokenArgs are the exec arguments of a rewritten file. A named tenant entry is spelled out
// so the file always asks for that entry's token. Plain `token` means the default entry and
// never the active one: a cluster file must not send one tenant's token to another tenant's
// cluster because the active entry changed.
func tokenArgs(tenant, issuerID string) []any {
	args := []any{"token"}
	if tenant != "" && tenant != "default" {
		args = append(args, "--tenant", tenant)
	}
	if issuerID != "" {
		args = append(args, "--issuer-id", issuerID)
	}
	return args
}

// RewriteOptions are the arguments of Rewrite. They are named fields so a secret can never
// be passed where the tenant goes.
type RewriteOptions struct {
	// Tenant is the entry the file belongs to ("" or "default" for the flat default entry).
	Tenant string
	// IssuerID is config.IssuerID of the entry's issuer. It is written as --issuer-id so
	// `token` can refuse the file once the entry holds a different login.
	IssuerID string
	// Secrets are values that must not appear in the output; finding one is an error.
	Secrets []string
}

// Rewrite points every OIDC exec user at `kubectl-palette token` and drops everything else
// from the user entry, so the file holds no client secret and no token.
func Rewrite(raw []byte, o RewriteOptions) ([]byte, error) {
	tenant, secrets := o.Tenant, o.Secrets
	d, err := parse(raw)
	if err != nil {
		return nil, err
	}
	us := users(d)
	n := 0
	for _, u := range us {
		ex, ok := execOf(u)
		if !ok || !isOIDCLogin(argStrings(ex)) {
			continue
		}
		um := u.(map[string]any)
		um["user"] = map[string]any{"exec": map[string]any{
			"apiVersion":      "client.authentication.k8s.io/v1beta1",
			"command":         ExecCommand,
			"args":            tokenArgs(tenant, o.IssuerID),
			"interactiveMode": "Never",
		}}
		n++
	}
	if n == 0 {
		return nil, ErrNotOIDC
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("encode kubeconfig: %w", err)
	}
	enc.Close()
	out := buf.Bytes()
	for _, s := range secrets {
		if s != "" && bytes.Contains(out, []byte(s)) {
			return nil, errors.New("rewritten kubeconfig still contains a secret; refusing to write it")
		}
	}
	return out, nil
}
