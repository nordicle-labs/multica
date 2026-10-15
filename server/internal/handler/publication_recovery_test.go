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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/githubapp"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestPublicationCredentialFailureState(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&githubapp.MintFailure{Err: errors.New("create installation token: github status 503"), NoCredential: true, Retryable: true}, "pending"},
		{&githubapp.MintFailure{Err: errors.New("create installation token: connection reset"), Retryable: true}, "credential_unknown"},
		{errors.New("create installation token: repository scope mismatch"), "blocked"},
		{errors.New("github App installation and permissions are required"), "blocked"},
	} {
		if got := publicationCredentialFailureState(tc.err); got != tc.want {
			t.Errorf("state for %q = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestValidGitHubPullRequestURL(t *testing.T) {
	valid := "https://github.com/test-owner/test-repo/pull/42"
	if !validGitHubPullRequestURL(valid, "test-owner/test-repo", 42) {
		t.Fatal("canonical GitHub pull request URL was rejected")
	}
	for _, invalid := range []string{
		"http://github.com/test-owner/test-repo/pull/42",
		"https://user@github.com/test-owner/test-repo/pull/42",
		"https://github.com.evil/test-owner/test-repo/pull/42",
		"https://github.com/other/test-repo/pull/42",
		"https://github.com/test-owner/test-repo/pull/41",
		valid + "/files",
		valid + "?redirect=https://example.com",
	} {
		if validGitHubPullRequestURL(invalid, "test-owner/test-repo", 42) {
			t.Errorf("invalid pull request URL accepted: %q", invalid)
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

func TestPublicationRecoveryCredentialLeaseExpiry(t *testing.T) {
	now := time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		expires string
		want    bool
	}{
		{now.Add(-time.Second).Format(time.RFC3339Nano), true},
		{now.Add(time.Second).Format(time.RFC3339Nano), false},
		{"2026-10-15 11:59:59+00", true},
		{"", false},
		{"forged", false},
	} {
		if got := (publicationRecoveryState{CredentialExpiresAt: tc.expires}).credentialExpired(now); got != tc.want {
			t.Errorf("credentialExpired(%q) = %v, want %v", tc.expires, got, tc.want)
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
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/credential", testHandler.UpdateTaskGitHubPublicationCredential)
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
		ClaimID          string                 `json:"claim_id"`
		CredentialHandle string                 `json:"credential_handle"`
		Credentials      []githubapp.Credential `json:"credentials"`
	}
	if err := json.Unmarshal(first.body, &claimed); err != nil || claimed.ClaimID == "" || claimed.CredentialHandle == "" || len(claimed.Credentials) != 1 {
		t.Fatalf("decode recovery claim: claim_id=%q handle=%q credentials=%d err=%v", claimed.ClaimID, claimed.CredentialHandle, len(claimed.Credentials), err)
	}
	credentialPath := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/credential"
	credentialUpdate := func(action string) *httptest.ResponseRecorder {
		updateBody := []byte(`{"daemon_id":"` + daemonID + `","claim_id":"` + claimed.ClaimID + `","credential_handle":"` + claimed.CredentialHandle + `","credential_action":"` + action + `"}`)
		req := httptest.NewRequest(http.MethodPost, credentialPath, bytes.NewReader(updateBody))
		req.Header.Set("Authorization", "Bearer "+daemonToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := credentialUpdate("ack"); w.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", w.Code, w.Body.String())
	}
	if w := credentialUpdate("revoke"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revocation_confirmed":true`) {
		t.Fatalf("revoke status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-revoked:
	default:
		t.Fatal("recovery credential was not revoked")
	}
	const publishedSHA = "0123456789012345678901234567890123456789"
	completeBody := []byte(`{"daemon_id":"` + daemonID + `","repository":"test-owner/test-repo","claim_id":"` + claimed.ClaimID + `","credential_handle":"` + claimed.CredentialHandle + `","remote_sha":"` + publishedSHA + `","pull_request_number":42,"pull_request_url":"https://github.com/test-owner/test-repo/pull/42","readback_head_sha":"` + publishedSHA + `","revocation_confirmed":true}`)
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

	limitedTaskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), context='{"delivery_intent":"publish","publication_recovery":{"state":"pending","claims":3}}', result='{"recovery_repository":"test-owner/test-repo","recovery_bundle_verified":true,"recovery_head_sha":"0123456789012345678901234567890123456789"}' WHERE id=$1`, limitedTaskID)
	limitedPath := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + limitedTaskID + "/github-publication-recovery/claim"
	limitedReq := httptest.NewRequest(http.MethodPost, limitedPath, bytes.NewReader(body))
	limitedReq.Header.Set("Authorization", "Bearer "+daemonToken)
	limited := httptest.NewRecorder()
	router.ServeHTTP(limited, limitedReq)
	if limited.Code != http.StatusConflict || minted.Load() != 1 {
		t.Fatalf("retry limit status=%d minted=%d body=%s", limited.Code, minted.Load(), limited.Body.String())
	}
	var limitedState string
	dbfx.QueryRow(t, `SELECT context->'publication_recovery'->>'state' FROM agent_task_queue WHERE id=$1`, limitedTaskID).Scan(&limitedState)
	if limitedState != "blocked" {
		t.Fatalf("retry limit state=%q, want blocked", limitedState)
	}
}

func TestPublicationRecoveryRetriesProvenNoCredentialAfterBackoff(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "publication-recovery-transient"
	runtimeID := createClaimReclaimRuntime(t, ctx, "Transient publication recovery runtime")
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, runtimeID, daemonID)
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Transient publication recovery agent")
	taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), context='{"delivery_intent":"publish"}', result='{"recovery_repository":"test-owner/test-repo","recovery_bundle_verified":true,"recovery_head_sha":"0123456789012345678901234567890123456789"}' WHERE id=$1`, taskID)
	setupGitHubClaimRevocation(t, issueID)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls++
		if calls == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"github-test-token","repositories":[{"full_name":"test-owner/test-repo"}]}`))
	}))
	t.Cleanup(srv.Close)
	previousBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = previousBase })

	token := "mdt_publication_transient"
	dbfx.Exec(t, `INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`, auth.HashToken(token), testWorkspaceID, daemonID)
	router := chi.NewRouter()
	router.Use(middleware.DaemonAuth(testHandler.Queries, nil, nil, nil))
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/claim", testHandler.ClaimTaskGitHubPublicationRecovery)
	path := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/claim"
	claim := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"daemon_id":"`+daemonID+`","repository":"test-owner/test-repo"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := claim(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first claim status=%d body=%s", w.Code, w.Body.String())
	}
	if w := claim(); w.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("backoff claim status=%d mint calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET context=jsonb_set(context,'{publication_recovery,retry_after}',to_jsonb((clock_timestamp()-interval '1 second')::text),true) WHERE id=$1`, taskID)
	if w := claim(); w.Code != http.StatusOK || calls != 2 {
		t.Fatalf("retried claim status=%d mint calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	var state string
	var claims int
	dbfx.QueryRow(t, `SELECT context->'publication_recovery'->>'state', (context->'publication_recovery'->>'claims')::int FROM agent_task_queue WHERE id=$1`, taskID).Scan(&state, &claims)
	if state != "credential_issued" || claims != 2 {
		t.Fatalf("recovery state=%q claims=%d, want credential_issued/2", state, claims)
	}
}
