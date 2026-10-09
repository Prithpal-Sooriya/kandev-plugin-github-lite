package githublite

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"

	recipe "kandev-plugin-github-lite/recipes/source-control/server"
)

// associationStateKey is the Host state key (scope "workspace") that stores a
// task's manually linked pull requests. One key per task; the value is a
// JSON document {task_id, associations: [{connection_scope, repository_id, number}]}.
// Only immutable identity is stored — titles/urls are re-resolved live from
// GitHub on every read, so a renamed repo or renumbered PR can never poison
// the display while the immutable numeric ids stay correct.
const associationStateKey = "github_lite_task_associations"

type associationRecord struct {
	ConnectionScope string `json:"connection_scope"`
	RepositoryID    string `json:"repository_id"`
	Number          int64  `json:"number"`
}

type associationDocument struct {
	TaskID       string              `json:"task_id"`
	Associations []associationRecord `json:"associations"`
	// Removed tombstones identities the user unlinked. Auto-link (the
	// reviews refresh that discovers open PRs on a task's checkout branch)
	// must never re-display or re-persist a tombstoned pull: without this,
	// unlink would be silently undone within one TTL window because the
	// cached pulls?head= match keeps firing. A manual link clears the
	// tombstone — re-linking on purpose always wins.
	Removed []associationRecord `json:"removed,omitempty"`
}

// tombstoned reports whether the repository+number pair was unlinked
// (scope-insensitive: the pair identifies the pull even if the token's
// owner changes).
func (d associationDocument) tombstoned(repositoryID string, number int64) bool {
	for _, removed := range d.Removed {
		if removed.RepositoryID == repositoryID && removed.Number == number {
			return true
		}
	}
	return false
}

// readAssociationDocument loads a task's association document (zero value
// when unset).
func (a *Adapters) readAssociationDocument(ctx context.Context, workspaceID, taskID string) (associationDocument, error) {
	host := a.host()
	if host == nil {
		return associationDocument{}, fmt.Errorf("github-lite: Host not injected yet")
	}
	value, found, err := host.GetState(ctx, "workspace", workspaceID, taskStateKey(taskID))
	if err != nil {
		return associationDocument{}, fmt.Errorf("github-lite: reading associations for task %s: %w", taskID, err)
	}
	if !found || value == nil {
		return associationDocument{TaskID: taskID}, nil
	}
	// Host state values round-trip through protobuf Structs, so the document
	// arrives as map[string]any; re-marshal through the typed shape.
	encoded, err := json.Marshal(value)
	if err != nil {
		return associationDocument{}, fmt.Errorf("github-lite: decoding associations for task %s: %w", taskID, err)
	}
	var document associationDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		return associationDocument{}, fmt.Errorf("github-lite: decoding associations for task %s: %w", taskID, err)
	}
	return document, nil
}

// writeAssociationDocument replaces a task's stored associations and
// tombstones atomically (one host-state key).
func (a *Adapters) writeAssociationDocument(ctx context.Context, workspaceID, taskID string, document associationDocument) error {
	host := a.host()
	if host == nil {
		return fmt.Errorf("github-lite: Host not injected yet")
	}
	document.TaskID = taskID
	if document.Associations == nil {
		document.Associations = []associationRecord{}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return err
	}
	return host.SetState(ctx, "workspace", workspaceID, taskStateKey(taskID), value)
}

func taskStateKey(taskID string) string { return associationStateKey + ":" + taskID }

// identityToRecord converts the recipe's immutable identity to storage.
func identityToRecord(identity recipe.ChangeRequestIdentity) associationRecord {
	return associationRecord{
		ConnectionScope: identity.ConnectionScope,
		RepositoryID:    identity.RepositoryID,
		Number:          identity.Number,
	}
}

func recordToIdentity(record associationRecord) recipe.ChangeRequestIdentity {
	return recipe.ChangeRequestIdentity{
		ConnectionScope: record.ConnectionScope,
		RepositoryID:    record.RepositoryID,
		Number:          record.Number,
	}
}

// Link associates a pull request with a task (idempotent). It also clears
// the identity's tombstone, if any: linking is an explicit instruction that
// always beats a past unlink.
func (a *Adapters) Link(ctx context.Context, taskID string, identity recipe.ChangeRequestIdentity) error {
	workspaceID := ctxWorkspaceID(ctx)
	document, err := a.readAssociationDocument(ctx, workspaceID, taskID)
	if err != nil {
		return err
	}
	candidate := identityToRecord(identity)
	linked := false
	for _, existing := range document.Associations {
		if existing == candidate {
			linked = true
			break
		}
	}
	if !linked {
		document.Associations = append(document.Associations, candidate)
	}
	if len(document.Removed) > 0 {
		kept := make([]associationRecord, 0, len(document.Removed))
		for _, removed := range document.Removed {
			if removed.RepositoryID != candidate.RepositoryID || removed.Number != candidate.Number {
				kept = append(kept, removed)
			}
		}
		document.Removed = kept
	}
	if linked && len(document.Removed) == 0 {
		return nil // nothing changed
	}
	return a.writeAssociationDocument(ctx, workspaceID, taskID, document)
}

// Unlink removes one immutable identity from a task's associations and
// tombstones it so auto-link cannot resurrect it on the next refresh.
func (a *Adapters) Unlink(ctx context.Context, taskID string, identity recipe.ChangeRequestIdentity) error {
	workspaceID := ctxWorkspaceID(ctx)
	document, err := a.readAssociationDocument(ctx, workspaceID, taskID)
	if err != nil {
		return err
	}
	target := identityToRecord(identity)
	kept := make([]associationRecord, 0, len(document.Associations))
	for _, existing := range document.Associations {
		if existing != target {
			kept = append(kept, existing)
		}
	}
	if !document.tombstoned(target.RepositoryID, target.Number) {
		document.Removed = append(document.Removed, target)
	}
	document.Associations = kept
	return a.writeAssociationDocument(ctx, workspaceID, taskID, document)
}

// workspaceIDContext is the context key type carrying the verified
// workspace id from the action wrapper into the AssociationStore adapters
// (whose recipe-defined interface carries only the task id).
type workspaceIDContext struct{}

var workspaceIDKey = workspaceIDContext{}

// WithWorkspaceID attaches the host-verified workspace id to a context.
// The plugin's action wrapper calls it before delegating to the recipe.
func WithWorkspaceID(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, workspaceIDKey, workspaceID)
}

func ctxWorkspaceID(ctx context.Context) string {
	if value, ok := ctx.Value(workspaceIDKey).(string); ok {
		return value
	}
	return ""
}

// ResolveAttached maps the host-verified action context (workspace, task,
// session, host repository row, verified head branch) to the provider's
// immutable Repository for create operations. Browser descriptors are never
// authority: the host repository row and its RemoteURL are.
func (a *Adapters) ResolveAttached(ctx context.Context, action pluginsdk.VerifiedActionContext) (recipe.Repository, error) {
	if err := a.init(ctx); err != nil {
		return recipe.Repository{}, err
	}
	if action.WorkspaceID == "" || action.RepositoryID == "" {
		return recipe.Repository{}, fmt.Errorf("github-lite: verified context is missing workspace/repository")
	}
	host := a.host()
	if host == nil {
		return recipe.Repository{}, fmt.Errorf("github-lite: Host not injected yet")
	}

	// Find the host repository row by id (the verified identity), reading the
	// workspace's rows through the Host data API (capability api_read:repositories).
	rows, err := a.workspaceRepositories(ctx, host, action.WorkspaceID)
	if err != nil {
		return recipe.Repository{}, err
	}
	var row *pluginsdk.Repository
	for index := range rows {
		if rows[index].ID == action.RepositoryID {
			row = &rows[index]
			break
		}
	}
	if row == nil {
		return recipe.Repository{}, fmt.Errorf("github-lite: repository row %s not found in workspace %s", action.RepositoryID, action.WorkspaceID)
	}

	owner, name, ok := parseGitHubURL(row.RemoteURL)
	if !ok {
		// Fall back to the row's provider slugs.
		owner, name, ok = row.OwnerOrProject, row.Name, true
	}
	if !ok || owner == "" || name == "" {
		return recipe.Repository{}, fmt.Errorf("github-lite: repository row %s has no usable owner/name", action.RepositoryID)
	}

	scope, err := a.ConnectionScope(ctx, action.WorkspaceID)
	if err != nil {
		return recipe.Repository{}, err
	}

	// Numeric immutable id: prefer the row's provider id when numeric,
	// otherwise resolve owner/name once (TTL-cached).
	repositoryID := row.ProviderRepositoryID
	if _, err := strconv.ParseInt(repositoryID, 10, 64); err != nil || repositoryID == "" {
		var payload apiRepository
		if err := a.client.getJSON(ctx, "/repos/"+owner+"/"+name, &payload); err != nil {
			return recipe.Repository{}, fmt.Errorf("github-lite: resolving %s/%s: %w", owner, name, err)
		}
		if payload.ID == 0 {
			return recipe.Repository{}, fmt.Errorf("github-lite: repository %s/%s not found", owner, name)
		}
		repositoryID = strconv.FormatInt(payload.ID, 10)
	}

	return recipe.Repository{
		ProviderID:      providerID,
		ProviderHost:    providerHost,
		ConnectionScope: scope,
		RepositoryID:    repositoryID,
		OwnerOrProject:  owner,
		Name:            name,
		CloneURL:        row.RemoteURL,
		DefaultBranch:   derefString(row.DefaultBranch),
	}, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// workspaceRepositories pages through a workspace's repository rows through
// the Host data API (api_read:repositories), bounded to 5 pages of 200 — a
// workspace with more repositories than that is pathological, and the read
// is on the local Host DB, not GitHub.
func (a *Adapters) workspaceRepositories(ctx context.Context, host pluginsdk.Host, workspaceID string) ([]pluginsdk.Repository, error) {
	var all []pluginsdk.Repository
	page := pluginsdk.Page{Limit: 200}
	for attempt := 0; attempt < 5; attempt++ {
		rows, info, err := host.Repositories().List(ctx, workspaceID, page)
		if err != nil {
			return nil, fmt.Errorf("github-lite: reading workspace repositories: %w", err)
		}
		all = append(all, rows...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	return all, nil
}
