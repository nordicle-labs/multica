package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/githubapp"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestPublicationCredentialFailureState(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want string
	}{
		{"create installation token: github status 503", "pending"},
		{"create installation token: connection reset", "pending"},
		{"create installation token: repository scope mismatch", "blocked"},
		{"github App installation and permissions are required", "blocked"},
	} {
		if got := publicationCredentialFailureState(errors.New(tc.err)); got != tc.want {
			t.Errorf("state for %q = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestValidGitObjectID(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"0123456789012345678901234567890123456789", true},
		{"0123456789012345678901234567890123456789012345678901234567890123", true},
		{"012345678901234567890123456789012345678", false},
		{"012345678901234567890123456789012345678g", false},
	} {
		if got := validGitObjectID(tc.value); got != tc.want {
			t.Errorf("validGitObjectID(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestPublicationRecoveryClaimsTerminalTaskOnceThroughMiddleware(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "publication-recovery-daemon"
	runtimeID := createClaimReclaimRuntime(t, ctx, "Publication recovery runtime")
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, runtimeID, daemonID)
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Publication recovery agent")
	taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), context='{"delivery_intent":"publish"}', result='{"recovery_repository":"test-owner/test-repo","recovery_bundle_verified":true,"recovery_head_sha":"0123456789012345678901234567890123456789"}' WHERE id=$1`, taskID)
	minted, revoked := setupGitHubClaimRevocation(t, issueID)

	pat := "mul_publication_recovery"
	dbfx.Insert(t, "personal_access_token", testutil.Cols{
		"user_id": testUserID, "name": "Publication recovery", "token_hash": auth.HashToken(pat),
		"token_prefix": pat[:8], "expires_at": nil,
	})
	router := chi.NewRouter()
	router.Use(middleware.DaemonAuth(testHandler.Queries, nil, nil, nil))
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/claim", testHandler.ClaimTaskGitHubPublicationRecovery)
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/complete", testHandler.CompleteTaskGitHubPublicationRecovery)
	claimPath := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/claim"
	body := []byte(`{"daemon_id":"` + daemonID + `","repository":"test-owner/test-repo"}`)
	patReq := httptest.NewRequest(http.MethodPost, claimPath, bytes.NewReader(body))
	patReq.Header.Set("Authorization", "Bearer "+pat)
	patResponse := httptest.NewRecorder()
	router.ServeHTTP(patResponse, patReq)
	if patResponse.Code != http.StatusForbidden || minted.Load() != 0 {
		t.Fatalf("PAT claim status=%d minted=%d, want forbidden without mint", patResponse.Code, minted.Load())
	}
	daemonToken := "mdt_publication_recovery"
	dbfx.Exec(t, `INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`, auth.HashToken(daemonToken), testWorkspaceID, daemonID)
	type response struct {
		code int
		body []byte
	}
	start := make(chan struct{})
	responses := make(chan response, 2)
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest(http.MethodPost, claimPath, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+daemonToken)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			responses <- response{code: w.Code, body: w.Body.Bytes()}
		}()
	}
	close(start)
	first, second := <-responses, <-responses
	if first.code == http.StatusConflict {
		first, second = second, first
	}
	if first.code != http.StatusOK || second.code != http.StatusConflict || minted.Load() != 1 {
		t.Fatalf("claim statuses=%d/%d minted=%d", first.code, second.code, minted.Load())
	}
	var claimed struct {
		ClaimID     string                 `json:"claim_id"`
		Credentials []githubapp.Credential `json:"credentials"`
	}
	if err := json.Unmarshal(first.body, &claimed); err != nil || claimed.ClaimID == "" || len(claimed.Credentials) != 1 {
		t.Fatalf("decode recovery claim: claim_id=%q credentials=%d err=%v", claimed.ClaimID, len(claimed.Credentials), err)
	}
	if err := revokeGitHubClaimCredentials(githubAppBroker(), claimed.Credentials); err != nil {
		t.Fatal(err)
	}
	select {
	case <-revoked:
	default:
		t.Fatal("recovery credential was not revoked")
	}
	const publishedSHA = "0123456789012345678901234567890123456789"
	completeBody := []byte(`{"daemon_id":"` + daemonID + `","repository":"test-owner/test-repo","claim_id":"` + claimed.ClaimID + `","remote_sha":"` + publishedSHA + `","pull_request_number":42,"pull_request_url":"https://github.test/pull/42","readback_head_sha":"` + publishedSHA + `","revocation_confirmed":true}`)
	completePath := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/complete"
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, completePath, bytes.NewReader(completeBody))
		req.Header.Set("Authorization", "Bearer "+daemonToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := request(); w.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", w.Code, w.Body.String())
	}
	var persisted string
	dbfx.QueryRow(t, `SELECT context->'publication_recovery'->>'remote_sha' FROM agent_task_queue WHERE id=$1`, taskID).Scan(&persisted)
	if persisted != publishedSHA {
		t.Fatalf("persisted publication SHA = %q", persisted)
	}
	if w := request(); w.Code != http.StatusConflict {
		t.Fatalf("stale complete status=%d body=%s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, claimPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+daemonToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"completed":true`) || minted.Load() != 1 {
		t.Fatalf("readback status=%d minted=%d body=%s", w.Code, minted.Load(), w.Body.String())
	}
}
