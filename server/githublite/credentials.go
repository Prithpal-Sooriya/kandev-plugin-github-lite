package githublite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// gitCredentialUsername is the username served with the token. GitHub accepts
// any non-empty username when the password is a personal access token;
// x-access-token is the conventional machine identity and never echoes a
// human login into clone logs.
const gitCredentialUsername = "x-access-token"

// supportedGitCredentialHost reports whether the adapters serve clone
// credentials for a host. github.com is the only host this provider claims,
// so it is the only host it authenticates.
func supportedGitCredentialHost(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "github.com")
}

// ResolveGitCredential serves the host's HTTPS clone authentication for
// github.com from the plugin's configured token: kandev resolves workspace
// Git credentials by asking the plugin that owns the repository provider,
// and fails closed when the plugin does not implement this extension. The
// secret is transient by contract — never logged, never persisted — and this
// log line records only that a credential was issued, never its value.
func (a *Adapters) ResolveGitCredential(ctx context.Context, request *pluginsdk.ResolveGitCredentialRequest) (*pluginsdk.ResolveGitCredentialResponse, error) {
	if request == nil {
		return nil, errors.New("github-lite: nil Git credential request")
	}
	if !supportedGitCredentialHost(request.Host) {
		logger.Info("git credential", "workspace", request.WorkspaceID, "host", request.Host, "path", request.Path, "outcome", "unsupported host")
		return nil, fmt.Errorf("github-lite: Git credentials are only served for github.com, not %q", strings.TrimSpace(request.Host))
	}
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(a.config.Token)
	if token == "" {
		logger.Info("git credential", "workspace", request.WorkspaceID, "host", request.Host, "path", request.Path, "outcome", "no token configured")
		return nil, errors.New("github-lite: no GitHub access token configured — set one at Settings > Plugins > GitHub Lite")
	}
	logger.Info("git credential", "workspace", request.WorkspaceID, "host", request.Host, "path", request.Path, "outcome", "issued")
	return &pluginsdk.ResolveGitCredentialResponse{
		Username: gitCredentialUsername,
		Secret:   token,
	}, nil
}

// GetGitCredentialBinding returns a non-secret revision of the configured
// token for the host's lease validation: an empty binding revokes every
// already-issued lease, so a token change (kandev restarts the plugin on
// config writes) invalidates leases issued under the previous token without
// ever re-reading the secret. The binding is a truncated SHA-256 digest — it
// is not reversible and leaks nothing about the token beyond sameness.
func (a *Adapters) GetGitCredentialBinding(ctx context.Context, request *pluginsdk.GitCredentialBindingRequest) (*pluginsdk.GitCredentialBindingResponse, error) {
	if request == nil {
		return nil, errors.New("github-lite: nil Git credential binding request")
	}
	if !supportedGitCredentialHost(request.Host) {
		return nil, fmt.Errorf("github-lite: Git credentials are only served for github.com, not %q", strings.TrimSpace(request.Host))
	}
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(a.config.Token)
	if token == "" {
		// No token: every lease previously issued under an older token is
		// revoked.
		return &pluginsdk.GitCredentialBindingResponse{Binding: ""}, nil
	}
	digest := sha256.Sum256([]byte("github-lite:" + token))
	binding := "token-" + hex.EncodeToString(digest[:8])
	logger.Info("git credential binding", "workspace", request.WorkspaceID, "host", request.Host, "path", request.Path, "binding", binding)
	return &pluginsdk.GitCredentialBindingResponse{Binding: binding}, nil
}
