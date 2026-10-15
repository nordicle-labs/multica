package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestProjectBootstrapUsesRealRouterAuthAndRoleGuards(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}

	type actor struct {
		token    string
		want     int
		wantBody string
	}
	actors := map[string]actor{"owner": {token: testToken, want: http.StatusConflict, wantBody: "exactly one nordicle-labs"}}
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
		actors[role] = actor{token: token, want: want, wantBody: wantBody}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, id)
		})
	}

	post := func(t *testing.T, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/projects/bootstrap", bytes.NewBufferString(`{"name":"router-auth","visibility":"private","product":"Router Auth"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-User-ID", "00000000-0000-0000-0000-000000000099")
		req.Header.Set("X-Actor-Source", "task_token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for role, actor := range actors {
		t.Run(role+" human", func(t *testing.T) {
			resp := post(t, actor.token)
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
	taskToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)
	t.Run("non-human task token", func(t *testing.T) {
		resp := post(t, taskToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil || !strings.Contains(string(body), "only available to human actors") {
			t.Fatalf("body = %q, want human-actor denial (read error: %v)", body, err)
		}
	})
}
