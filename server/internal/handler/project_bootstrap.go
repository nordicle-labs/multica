package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubapp"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ProjectBootstrapper interface {
	Check(context.Context, int64, string, string, string) (bool, githubapp.BootstrapResult, error)
	Bootstrap(context.Context, int64, string, string, string) (githubapp.BootstrapResult, error)
	Delete(context.Context, int64, string, int64) error
}

func NewProjectBootstrapper() ProjectBootstrapper {
	return githubapp.Bootstrapper{Broker: githubAppBroker()}
}

type projectBootstrapRequest struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Product    string `json:"product"`
}

type projectBootstrapResponse struct {
	githubapp.BootstrapResult
	Project ProjectResponse `json:"project"`
}

type bootstrapCommitError struct{ err error }

func (e *bootstrapCommitError) Error() string { return e.err.Error() }
func (e *bootstrapCommitError) Unwrap() error { return e.err }

// BootstrapProject creates the canonical GitHub repository and its Multica
// project/resource. Every local collision is resolved before the first write.
func (h *Handler) BootstrapProject(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	var req projectBootstrapRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, err := githubapp.NormalizeRepositoryName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := githubapp.ValidateVisibility(req.Visibility); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Product = strings.TrimSpace(req.Product)
	if req.Product == "" {
		writeError(w, http.StatusBadRequest, "product is required")
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	requester, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	if h.ProjectBootstrap == nil {
		writeError(w, http.StatusServiceUnavailable, "GitHub App bootstrap is unavailable")
		return
	}
	installations, err := h.Queries.ListGitHubInstallationsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read GitHub App installation")
		return
	}
	installation, err := bootstrapInstallation(installations)
	if err != nil {
		writeError(w, http.StatusConflict, "workspace requires exactly one nordicle-labs GitHub App installation")
		return
	}

	repoURL := "https://github.com/" + githubapp.BootstrapOwner + "/" + name
	lockTx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to serialize project bootstrap")
		return
	}
	defer lockTx.Rollback(r.Context())
	for _, key := range bootstrapLockKeys(workspaceID, req.Product, repoURL) {
		if _, err := lockTx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to serialize project bootstrap")
			return
		}
	}
	existingProject, localState, err := h.bootstrapLocalState(r.Context(), wsUUID, req.Product, repoURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to preflight Multica project")
		return
	}
	if localState == "divergent" {
		writeError(w, http.StatusConflict, "Multica project or repository collision is partial or divergent")
		return
	}

	remoteExists, remote, err := h.ProjectBootstrap.Check(r.Context(), installation.InstallationID, name, req.Visibility, req.Product)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if localState == "exact" {
		if !remoteExists {
			writeError(w, http.StatusConflict, "Multica state exists but GitHub repository is missing")
			return
		}
		writeJSON(w, http.StatusOK, projectBootstrapResponse{BootstrapResult: remote, Project: projectToResponse(existingProject)})
		return
	}
	if remoteExists {
		writeError(w, http.StatusConflict, "GitHub repository exists without the matching Multica project")
		return
	}

	remote, err = h.ProjectBootstrap.Bootstrap(r.Context(), installation.InstallationID, name, req.Visibility, req.Product)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if remote.State != "created" {
		writeError(w, http.StatusConflict, "GitHub repository appeared during bootstrap; refusing to adopt it")
		return
	}
	project, err := h.createBootstrapProject(r.Context(), lockTx, wsUUID, requester.UserID, req.Product, repoURL)
	if err != nil {
		var commitErr *bootstrapCommitError
		if errors.As(err, &commitErr) && !errors.Is(err, pgx.ErrTxCommitRollback) {
			readProject, state, readErr := h.bootstrapLocalState(r.Context(), wsUUID, req.Product, repoURL)
			if readErr != nil || (state != "absent" && (state != "exact" || readProject.ID != project.ID)) {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("database commit outcome is ambiguous; reconcile repository %s/%s and Multica project", githubapp.BootstrapOwner, name))
				return
			}
			if state == "exact" {
				writeJSON(w, http.StatusCreated, projectBootstrapResponse{BootstrapResult: remote, Project: projectToResponse(readProject)})
				return
			}
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if rollbackErr := h.ProjectBootstrap.Delete(rollbackCtx, installation.InstallationID, name, remote.RepositoryID); rollbackErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("create Multica project failed; rollback incomplete; reconcile repository %s/%s", githubapp.BootstrapOwner, name))
			return
		}
		writeError(w, http.StatusInternalServerError, "create Multica project failed; GitHub repository rolled back")
		return
	}
	writeJSON(w, http.StatusCreated, projectBootstrapResponse{BootstrapResult: remote, Project: projectToResponse(project)})
}

func bootstrapInstallation(installations []db.GithubInstallation) (db.GithubInstallation, error) {
	if len(installations) != 1 || strings.TrimSpace(installations[0].AccountLogin) != githubapp.BootstrapOwner || installations[0].AccountType != "Organization" || installations[0].InstallationID <= 0 {
		return db.GithubInstallation{}, errors.New("workspace must have exactly one nordicle-labs Organization GitHub App installation")
	}
	return installations[0], nil
}

func bootstrapLockKeys(workspaceID, product, repoURL string) []string {
	keys := []string{workspaceID + ":product:" + product, workspaceID + ":repository:" + repoURL}
	sort.Strings(keys)
	return keys
}

func (h *Handler) bootstrapLocalState(ctx context.Context, workspaceID pgtype.UUID, product, repoURL string) (db.Project, string, error) {
	projects, err := h.Queries.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: workspaceID})
	if err != nil {
		return db.Project{}, "", err
	}
	var exact *db.Project
	collision := false
	for i := range projects {
		resources, err := h.Queries.ListProjectResourcesInWorkspace(ctx, db.ListProjectResourcesInWorkspaceParams{ProjectID: projects[i].ID, WorkspaceID: workspaceID})
		if err != nil {
			return db.Project{}, "", err
		}
		titleMatch := projects[i].Title == product
		repoMatch := false
		for _, resource := range resources {
			if resource.ResourceType != "github_repo" {
				continue
			}
			var ref struct{ URL, DefaultBranchHint string }
			if json.Unmarshal(resource.ResourceRef, &ref) == nil && strings.EqualFold(strings.TrimSuffix(ref.URL, ".git"), repoURL) {
				repoMatch = ref.DefaultBranchHint == githubapp.DefaultBranch
				if !repoMatch {
					collision = true
				}
			}
		}
		if titleMatch && repoMatch {
			if exact != nil {
				collision = true
			}
			exact = &projects[i]
		} else if titleMatch || repoMatch {
			collision = true
		}
	}
	if collision {
		return db.Project{}, "divergent", nil
	}
	if exact != nil {
		return *exact, "exact", nil
	}
	return db.Project{}, "absent", nil
}

func (h *Handler) createBootstrapProject(ctx context.Context, tx pgx.Tx, workspaceID, userID pgtype.UUID, product, repoURL string) (db.Project, error) {
	qtx := h.Queries.WithTx(tx)
	project, err := qtx.CreateProject(ctx, db.CreateProjectParams{WorkspaceID: workspaceID, Title: product, Status: "planned", Priority: "none"})
	if err != nil {
		return db.Project{}, err
	}
	ref, _ := json.Marshal(map[string]string{"url": repoURL, "default_branch_hint": githubapp.DefaultBranch})
	_, err = qtx.CreateProjectResource(ctx, db.CreateProjectResourceParams{ProjectID: project.ID, WorkspaceID: workspaceID, ResourceType: "github_repo", ResourceRef: ref, CreatedBy: userID})
	if err != nil {
		return db.Project{}, err
	}
	readProject, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: project.ID, WorkspaceID: workspaceID})
	if err != nil || readProject.Title != product {
		return db.Project{}, errors.New("project readback mismatch")
	}
	resources, err := qtx.ListProjectResourcesInWorkspace(ctx, db.ListProjectResourcesInWorkspaceParams{ProjectID: project.ID, WorkspaceID: workspaceID})
	if err != nil || len(resources) != 1 || resources[0].ResourceType != "github_repo" || string(resources[0].ResourceRef) != string(ref) {
		return db.Project{}, errors.New("project resource readback mismatch")
	}
	if err := tx.Commit(ctx); err != nil {
		return readProject, &bootstrapCommitError{err: err}
	}
	return readProject, nil
}
