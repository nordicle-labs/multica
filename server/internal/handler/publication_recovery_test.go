package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	daemonpkg "github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
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
	states := []string{"credential_minting", "credential_unknown", "credential_issued", "credential_received", "revocation_pending"}
	expiries := []struct {
		name  string
		value string
		want  publicationCredentialExpiry
	}{
		{"empty", "", publicationCredentialExpiryUnknown},
		{"malformed", "forged", publicationCredentialExpiryUnknown},
		{"future", now.Add(time.Second).Format(time.RFC3339Nano), publicationCredentialExpiryFuture},
		{"expired", now.Add(-time.Second).Format(time.RFC3339Nano), publicationCredentialExpiryExpired},
	}
	for _, stage := range []string{"before_CAS", "loser_CAS"} {
		for _, state := range states {
			for _, expiry := range expiries {
				t.Run(stage+"/"+state+"/"+expiry.name, func(t *testing.T) {
					recovery := publicationRecoveryState{State: state, CredentialHandle: "credential", CredentialExpiresAt: expiry.value}
					if got := recovery.credentialExpiry(now); got != expiry.want {
						t.Fatalf("expiry = %q, want %q", got, expiry.want)
					}
					if !recovery.hasActiveCredential() || recovery.credentialExpired(now) != (expiry.want == publicationCredentialExpiryExpired) {
						t.Fatalf("active/expired classification = %t/%t", recovery.hasActiveCredential(), recovery.credentialExpired(now))
					}
					if expiry.want != publicationCredentialExpiryExpired {
						w := httptest.NewRecorder()
						writePublicationCredentialOutstanding(w, recovery, expiry.want)
						var response struct {
							Code    string    `json:"code"`
							Status  string    `json:"status"`
							RetryAt time.Time `json:"retry_at"`
						}
						if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != http.StatusConflict {
							t.Fatalf("response status=%d body=%s", w.Code, w.Body.String())
						}
						if expiry.want == publicationCredentialExpiryUnknown && (response.Code != "publication_credential_expiry_unknown" || response.Status != "blocked" || !response.RetryAt.IsZero()) {
							t.Fatalf("unknown response = %+v", response)
						}
						if expiry.want == publicationCredentialExpiryFuture && (response.Code != "publication_credential_outstanding" || response.Status != "waiting" || !response.RetryAt.Equal(now.Add(time.Second))) {
							t.Fatalf("future response = %+v", response)
						}
					}
				})
			}
		}
	}
}

func TestPublicationRecoveryUnknownCredentialExpiryNeverRemints(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Unknown publication expiry runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Unknown publication expiry agent")
	daemonID := "publication-expiry-unknown-" + runtimeID
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, runtimeID, daemonID)
	minted, _ := setupGitHubClaimRevocation(t, issueID)
	token := "mdt_publication_expiry_unknown"
	dbfx.Exec(t, `INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`, auth.HashToken(token), testWorkspaceID, daemonID)
	router := chi.NewRouter()
	router.Use(middleware.DaemonAuth(testHandler.Queries, nil, nil, nil))
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/claim", testHandler.ClaimTaskGitHubPublicationRecovery)

	for _, state := range []string{"credential_minting", "credential_unknown", "credential_issued", "credential_received", "revocation_pending"} {
		for _, expiry := range []string{"", "forged"} {
			t.Run(state+"/"+expiry, func(t *testing.T) {
				taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), context=jsonb_build_object('delivery_intent','publish','publication_recovery',jsonb_build_object('state',$2::text,'claims',1,'credential_handle',$3::text,'credential_expires_at',$4::text)), result='{"recovery_repository":"test-owner/test-repo","recovery_bundle_verified":true,"recovery_head_sha":"0123456789012345678901234567890123456789"}' WHERE id=$1`, taskID, state, "handle-"+taskID, expiry)
				path := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/claim"
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"daemon_id":"`+daemonID+`","repository":"test-owner/test-repo"}`))
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				var response struct {
					Code    string    `json:"code"`
					Status  string    `json:"status"`
					RetryAt time.Time `json:"retry_at"`
				}
				if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != http.StatusConflict || response.Code != "publication_credential_expiry_unknown" || response.Status != "blocked" || !response.RetryAt.IsZero() {
					t.Fatalf("status=%d response=%+v body=%s", w.Code, response, w.Body.String())
				}
				var claims int
				var reason string
				dbfx.QueryRow(t, `SELECT (context->'publication_recovery'->>'claims')::int, context->>'publication_recovery_error' FROM agent_task_queue WHERE id=$1`, taskID).Scan(&claims, &reason)
				if claims != 1 || reason == "" || minted.Load() != 0 {
					t.Fatalf("claims=%d reason=%q minted=%d", claims, reason, minted.Load())
				}
			})
		}
	}
}

func TestPublicationRecoveryClaimsTerminalTaskOnceThroughMiddleware(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Publication recovery runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Publication recovery agent")
	taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	daemonID := "publication-recovery-" + taskID
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, runtimeID, daemonID)
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
	daemonToken := "mdt_" + strings.ReplaceAll(taskID, "-", "")
	dbfx.Exec(t, `INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`, auth.HashToken(daemonToken), testWorkspaceID, daemonID)
	blocker := beginWarmedTx(t, ctx)
	if _, err := blocker.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE`, taskID); err != nil {
		t.Fatalf("lock publication recovery task: %v", err)
	}
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
	if !waitForPublicationRecoveryCASWaiters(t, 2, 10*time.Second) {
		t.Fatal("both recovery claims did not reach the CAS")
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release publication recovery task: %v", err)
	}
	first, second := <-responses, <-responses
	if first.code == http.StatusConflict {
		first, second = second, first
	}
	if first.code != http.StatusOK || second.code != http.StatusConflict || minted.Load() != 1 {
		t.Fatalf("claim statuses=%d/%d minted=%d", first.code, second.code, minted.Load())
	}
	var deferred struct {
		Code    string    `json:"code"`
		RetryAt time.Time `json:"retry_at"`
	}
	if err := json.Unmarshal(second.body, &deferred); err != nil || deferred.Code != "publication_credential_outstanding" || deferred.RetryAt.Before(time.Now().Add(64*time.Minute)) {
		t.Fatalf("outstanding claim response=%s decoded=%+v err=%v", second.body, deferred, err)
	}
	var claims int
	dbfx.QueryRow(t, `SELECT (context->'publication_recovery'->>'claims')::int FROM agent_task_queue WHERE id=$1`, taskID).Scan(&claims)
	if claims != 1 {
		t.Fatalf("publication recovery claims=%d, want 1", claims)
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

func TestPublicationRecoveryCASLoserDefersRealDaemonJournal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Integrated publication recovery runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Integrated publication recovery agent")
	taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	daemonID := "publication-recovery-integrated-" + taskID
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, runtimeID, daemonID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now(), context='{"delivery_intent":"publish"}', result='{"recovery_repository":"test-owner/test-repo","recovery_bundle_verified":true,"recovery_head_sha":"0123456789012345678901234567890123456789"}' WHERE id=$1`, taskID)
	setupGitHubClaimRevocation(t, issueID)
	token := "mdt_publication_integrated_" + strings.ReplaceAll(taskID, "-", "")
	dbfx.Exec(t, `INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`, auth.HashToken(token), testWorkspaceID, daemonID)

	router := chi.NewRouter()
	router.Use(middleware.DaemonAuth(testHandler.Queries, nil, nil, nil))
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/claim", testHandler.ClaimTaskGitHubPublicationRecovery)
	router.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/github-publication-recovery/credential", testHandler.UpdateTaskGitHubPublicationCredential)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	repo, head := publicationRecoveryGitRepo(t)
	recoveryRoot := filepath.Join(t.TempDir(), "recovery")
	manifest := execenv.RecoveryManifest{TaskID: taskID, RuntimeID: runtimeID, DeliveryIntent: "publish", Repository: "test-owner/test-repo", GitRoot: repo, CanonicalBranch: "main", BaseSHA: head, ExpectedRefSHA: head, HeadSHA: head}
	if err := execenv.WriteRecoveryManifest(recoveryRoot, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := execenv.CreateRecoveryArtifact(repo, recoveryRoot, taskID, head, slog.Default()); err != nil {
		t.Fatal(err)
	}
	d := daemonpkg.New(daemonpkg.Config{ServerBaseURL: server.URL, DaemonID: daemonID, WorkspacesRoot: filepath.Dir(recoveryRoot)}, slog.Default())

	blocker := beginWarmedTx(t, ctx)
	if _, err := blocker.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE`, taskID); err != nil {
		t.Fatalf("lock publication recovery task: %v", err)
	}
	claimPath := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/claim"
	body := []byte(`{"daemon_id":"` + daemonID + `","repository":"test-owner/test-repo"}`)
	direct := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, claimPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		direct <- w
	}()
	if !waitForPublicationRecoveryCASWaiters(t, 1, 10*time.Second) {
		t.Fatal("winning request did not reach the CAS")
	}
	type reconcileResult struct {
		resumed int
		err     error
	}
	reconciled := make(chan reconcileResult, 1)
	go func() {
		resumed, err := d.ReconcileRecoveryManifests(ctx, recoveryRoot)
		reconciled <- reconcileResult{resumed: resumed, err: err}
	}()
	if !waitForPublicationRecoveryCASWaiters(t, 2, 10*time.Second) {
		t.Fatal("daemon request did not reach the CAS")
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release publication recovery task: %v", err)
	}
	response := <-direct
	if response.Code != http.StatusOK {
		t.Fatalf("winning claim status=%d body=%s", response.Code, response.Body.String())
	}
	if result := <-reconciled; result.err != nil || result.resumed != 1 {
		t.Fatalf("daemon reconcile resumed=%d err=%v", result.resumed, result.err)
	}
	journal, exists, err := execenv.ReadRecoveryManifest(recoveryRoot, taskID)
	if err != nil || !exists || journal.RecoveryAttempts != 0 || journal.NextAttemptAt.IsZero() {
		t.Fatalf("daemon journal exists=%v err=%v manifest=%+v", exists, err, journal)
	}
	var claims int
	dbfx.QueryRow(t, `SELECT (context->'publication_recovery'->>'claims')::int FROM agent_task_queue WHERE id=$1`, taskID).Scan(&claims)
	if claims != 1 {
		t.Fatalf("publication recovery claims=%d, want 1", claims)
	}
	var claim struct {
		ClaimID          string `json:"claim_id"`
		CredentialHandle string `json:"credential_handle"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}
	credentialPath := "/api/daemon/runtimes/" + runtimeID + "/tasks/" + taskID + "/github-publication-recovery/credential"
	revoke := httptest.NewRequest(http.MethodPost, credentialPath, strings.NewReader(`{"daemon_id":"`+daemonID+`","claim_id":"`+claim.ClaimID+`","credential_handle":"`+claim.CredentialHandle+`","credential_action":"revoke"}`))
	revoke.Header.Set("Authorization", "Bearer "+token)
	revoked := httptest.NewRecorder()
	router.ServeHTTP(revoked, revoke)
	if revoked.Code != http.StatusOK {
		t.Fatalf("credential cleanup status=%d body=%s", revoked.Code, revoked.Body.String())
	}
}

func publicationRecoveryGitRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "delivery.txt"), []byte("delivery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "delivery.txt")
	run("commit", "-m", "delivery")
	return repo, run("rev-parse", "HEAD")
}

func waitForPublicationRecoveryCASWaiters(t *testing.T, want int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var waiting int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND wait_event IN ('transactionid', 'tuple')
			  AND query LIKE '%UPDATE agent_task_queue%publication_recovery%'
		`).Scan(&waiting); err != nil {
			t.Fatalf("probe publication recovery waiters: %v", err)
		}
		if waiting >= want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
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
