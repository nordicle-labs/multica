package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	apphandler "github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestProjectBootstrapRouteRejectsCloudPAT(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	fleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"valid":true,"owner_id":%q}`, testUserID)
	}))
	defer fleet.Close()

	queries := db.New(testPool)
	router := chi.NewRouter()
	router.Use(middleware.Auth(queries, nil, auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{FleetBaseURL: fleet.URL}), nil))
	router.Use(middleware.RequireWorkspaceMember(queries))
	router.Post("/api/projects/bootstrap", (&apphandler.Handler{}).BootstrapProject)

	req := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer mcn_project_bootstrap_test")
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "unavailable to cloud-node credentials") {
		t.Fatalf("body = %q", response.Body.String())
	}
}

func TestProjectBootstrapUsesRealRouterAuthAndRoleGuards(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	var workspaceSlug string
	if err := testPool.QueryRow(ctx, `SELECT slug FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&workspaceSlug); err != nil {
		t.Fatal(err)
	}

	type actor struct {
		token    string
		userID   string
		want     int
		wantBody string
	}
	actors := map[string]actor{"owner": {token: testToken, userID: testUserID, want: http.StatusConflict, wantBody: "exactly one nordicle-labs"}}
	for _, role := range []string{"admin", "member"} {
		var id string
		email := fmt.Sprintf("project-bootstrap-%s@multica.test", role)
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "Bootstrap "+role, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, testWorkspaceID, id, role); err != nil {
			t.Fatal(err)
		}
		token, err := generateTestJWT(id, email, "Bootstrap "+role)
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusConflict
		if role == "member" {
			want = http.StatusForbidden
		}
		wantBody := "exactly one nordicle-labs"
		if role == "member" {
			wantBody = "insufficient permissions"
		}
		actors[role] = actor{token: token, userID: id, want: want, wantBody: wantBody}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, id)
		})
	}

	post := func(t *testing.T, token, divergentSelector string) *http.Response {
		t.Helper()
		path := "/api/projects/bootstrap"
		switch divergentSelector {
		case "query id":
			path += "?workspace_id=00000000-0000-0000-0000-000000000099"
		case "query slug":
			path += "?workspace_slug=not-the-token-workspace"
		case "repeated query id":
			path += "?workspace_id=" + testWorkspaceID + "&workspace_id=00000000-0000-0000-0000-000000000099"
		case "repeated query slug":
			path += "?workspace_slug=" + workspaceSlug + "&workspace_slug=not-the-token-workspace"
		}
		req, err := http.NewRequest(http.MethodPost, testServer.URL+path, bytes.NewBufferString(`{"name":"router-auth","visibility":"private","product":"Router Auth"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-User-ID", "00000000-0000-0000-0000-000000000099")
		req.Header.Set("X-Actor-Source", "task_token")
		switch divergentSelector {
		case "header id":
			req.Header.Set("X-Workspace-ID", "00000000-0000-0000-0000-000000000099")
		case "header slug":
			req.Header.Set("X-Workspace-Slug", "not-the-token-workspace")
		case "repeated header id":
			req.Header.Add("X-Workspace-ID", "00000000-0000-0000-0000-000000000099")
		case "repeated header slug":
			req.Header.Add("X-Workspace-Slug", workspaceSlug)
			req.Header.Add("X-Workspace-Slug", "not-the-token-workspace")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for role, actor := range actors {
		t.Run(role+" human", func(t *testing.T) {
			resp := post(t, actor.token, "")
			defer resp.Body.Close()
			if resp.StatusCode != actor.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, actor.want)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || !strings.Contains(string(body), actor.wantBody) {
				t.Fatalf("body = %q, want %q (read error: %v)", body, actor.wantBody, err)
			}
		})
	}

	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	taskID := ensureAgentTask(t, agentID)
	for _, role := range []string{"owner", "admin"} {
		role := role
		t.Run(role+" task token", func(t *testing.T) {
			taskToken := mintAgentTaskToken(t, agentID, taskID, actors[role].userID)
			resp := post(t, taskToken, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || !strings.Contains(string(body), "exactly one nordicle-labs") {
				t.Fatalf("body = %q, want installation preflight (read error: %v)", body, err)
			}
		})
		for _, selector := range []string{
			"header id", "query id", "header slug", "query slug",
			"repeated header id", "repeated query id", "repeated header slug", "repeated query slug",
		} {
			selector := selector
			t.Run(role+" task token rejects divergent "+selector, func(t *testing.T) {
				taskToken := mintAgentTaskToken(t, agentID, taskID, actors[role].userID)
				resp := post(t, taskToken, selector)
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", resp.StatusCode)
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil || !strings.Contains(string(body), "task token is bound to a different workspace") {
					t.Fatalf("body = %q, want workspace-bound denial (read error: %v)", body, err)
				}
			})
		}
	}

	t.Run("expired task token", func(t *testing.T) {
		token := mintAgentTaskToken(t, agentID, taskID, testUserID)
		if _, err := testPool.Exec(ctx, `UPDATE task_token SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, auth.HashToken(token)); err != nil {
			t.Fatal(err)
		}
		resp := post(t, token, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("revoked task token", func(t *testing.T) {
		token := mintAgentTaskToken(t, agentID, taskID, testUserID)
		if _, err := testPool.Exec(ctx, `DELETE FROM task_token WHERE token_hash = $1`, auth.HashToken(token)); err != nil {
			t.Fatal(err)
		}
		resp := post(t, token, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}
