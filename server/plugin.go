// Command kandev-plugin-github-lite is the backend half of the plugin.
package main

import (
	"context"

	"github.com/kandev/kandev/pkg/pluginsdk"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
	githublite "kandev-plugin-github-lite/server/githublite"
)

// githubLitePlugin implements pluginsdk.Plugin (via UnimplementedPlugin) and
// delegates every action to the vendored source-control recipe Extension
// wired to the concrete GitHub adapters.
type githubLitePlugin struct {
	pluginsdk.UnimplementedPlugin
	adapters  *githublite.Adapters
	extension *recipe.Extension
}

var _ pluginsdk.Plugin = (*githubLitePlugin)(nil)
var _ pluginsdk.ActionHandler = (*githubLitePlugin)(nil)
var _ pluginsdk.EntityReferenceSearcher = (*githubLitePlugin)(nil)
var _ pluginsdk.EntityReferenceAuthorizer = (*githubLitePlugin)(nil)

func newPlugin() *githubLitePlugin {
	plugin := &githubLitePlugin{}
	// The Host is injected after Serve; adapters resolve it lazily on every
	// call through UnimplementedPlugin.Host().
	plugin.adapters = githublite.NewAdapters(plugin.Host)
	plugin.extension = &recipe.Extension{
		ProviderID:           "github-lite",
		ReferenceSource:      "github_lite_pull_requests",
		Repositories:         plugin.adapters,
		RepositoryDetails:    plugin.adapters,
		AttachedRepositories: plugin.adapters,
		ChangeRequests:       plugin.adapters,
		Associations:         plugin.adapters,
		Reviews:              plugin.adapters,
		References:           plugin.adapters,
	}
	return plugin
}

// HandleAction delegates to the recipe Extension with the verified workspace
// injected into the context. The recipe's AssociationStore interface
// (Link/Unlink) deliberately carries only the task id; the workspace arrives
// here, verified, and adapters read it back through githublite's context key.
func (p *githubLitePlugin) HandleAction(ctx context.Context, request *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if request != nil && request.Context.WorkspaceID != "" {
		ctx = githublite.WithWorkspaceID(ctx, request.Context.WorkspaceID)
	}
	return p.extension.HandleAction(ctx, request)
}

// SearchEntityReferences delegates to the recipe's reference search.
func (p *githubLitePlugin) SearchEntityReferences(ctx context.Context, request *pluginsdk.SearchEntityReferencesRequest) (*pluginsdk.SearchEntityReferencesResponse, error) {
	if request != nil && request.WorkspaceID != "" {
		ctx = githublite.WithWorkspaceID(ctx, request.WorkspaceID)
	}
	return p.extension.SearchEntityReferences(ctx, request)
}

// AuthorizeEntityReference delegates to the recipe's reference authorization.
func (p *githubLitePlugin) AuthorizeEntityReference(ctx context.Context, request *pluginsdk.AuthorizeEntityReferenceRequest) (*pluginsdk.AuthorizeEntityReferenceResponse, error) {
	if request != nil && request.WorkspaceID != "" {
		ctx = githublite.WithWorkspaceID(ctx, request.WorkspaceID)
	}
	return p.extension.AuthorizeEntityReference(ctx, request)
}
