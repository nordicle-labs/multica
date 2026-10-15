package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/githubapp"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type bootstrapTestRemote struct {
	mu             sync.Mutex
	repositories   map[string]bool
	checks         int
	bootstraps     int
	deletes        int
	checkErr       error
	bootstrapErr   error
	deleteErr      error
	bootstrapState string
}

func (r *bootstrapTestRemote) Check(_ context.Context, _ int64, name, _, _ string) (bool, githubapp.BootstrapResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	return r.repositories[name], githubapp.BootstrapResult{State: "unchanged", RepositoryID: 123, RepositoryURL: "https://github.com/nordicle-labs/" + name, Branch: "main"}, r.checkErr
}

func (r *bootstrapTestRemote) Bootstrap(_ context.Context, _ int64, name, _, _ string) (githubapp.BootstrapResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bootstraps++
	if r.bootstrapErr != nil {
		return githubapp.BootstrapResult{}, r.bootstrapErr
	}
	state := r.bootstrapState
	if state == "" {
		state = "created"
	}
	if r.repositories[name] {
		state = "unchanged"
	} else if state == "created" {
		if r.repositories == nil {
			r.repositories = map[string]bool{}
		}
		r.repositories[name] = true
	}
	return githubapp.BootstrapResult{State: state, RepositoryID: 123, RepositoryURL: "https://github.com/nordicle-labs/" + name, Branch: "main"}, nil
}

func (r *bootstrapTestRemote) Delete(_ context.Context, _ int64, name string, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes++
	if r.deleteErr == nil {
		delete(r.repositories, name)
	}
	return r.deleteErr
}

func (r *bootstrapTestRemote) counts() (checks, bootstraps, deletes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks, r.bootstraps, r.deletes
}

type bootstrapCommitTx struct {
	pgx.Tx
	commit func(context.Context, pgx.Tx) error
}

func (t bootstrapCommitTx) Commit(ctx context.Context) error { return t.commit(ctx, t.Tx) }

type bootstrapTxStarter struct {
	txStarter
	commit func(context.Context, pgx.Tx) error
}

func (s bootstrapTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return bootstrapCommitTx{Tx: tx, commit: s.commit}, nil
}

func TestBootstrapProjectRequiresHumanActor(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap", nil)
	req.Header.Set("X-Actor-Source", "task_token")
	response := httptest.NewRecorder()
	(&Handler{}).BootstrapProject(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestBootstrapProjectRouteRequiresHumanActor(t *testing.T) {
	router := chi.NewRouter()
	router.With(RequireHumanActor).Post("/api/projects/bootstrap", (&Handler{}).BootstrapProject)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap", strings.NewReader(`{}`))
	req.Header.Set("X-Actor-Source", "task_token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

func TestBootstrapLockKeysSerializeProductAndRepositoryCollisions(t *testing.T) {
	first := bootstrapLockKeys("workspace", "Product", "repo-one")
	second := bootstrapLockKeys("workspace", "Product", "repo-two")
	third := bootstrapLockKeys("workspace", "Other", "repo-one")
	if !slices.Contains(first, "workspace:product:Product") || !slices.Contains(second, "workspace:product:Product") {
		t.Fatal("same product does not share an advisory lock")
	}
	if !slices.Contains(first, "workspace:repository:repo-one") || !slices.Contains(third, "workspace:repository:repo-one") {
		t.Fatal("same repository does not share an advisory lock")
	}
}

func TestBootstrapInstallationRequiresOneExactOwner(t *testing.T) {
	installation, err := bootstrapInstallation([]db.GithubInstallation{{InstallationID: 2, AccountLogin: "nordicle-labs", AccountType: "Organization"}})
	if err != nil || installation.InstallationID != 2 {
		t.Fatalf("installation=%+v err=%v", installation, err)
	}
	for _, installations := range [][]db.GithubInstallation{
		{{InstallationID: 1, AccountLogin: "Nordicle-Labs", AccountType: "Organization"}},
		{{InstallationID: 1, AccountLogin: "nordicle-labs", AccountType: "User"}},
		{{InstallationID: 1, AccountLogin: "other", AccountType: "Organization"}, {InstallationID: 2, AccountLogin: "nordicle-labs", AccountType: "Organization"}},
		{{InstallationID: 1, AccountLogin: "nordicle-labs", AccountType: "Organization"}, {InstallationID: 2, AccountLogin: "nordicle-labs", AccountType: "Organization"}},
	} {
		if _, err := bootstrapInstallation(installations); err == nil {
			t.Fatalf("installations %+v were accepted", installations)
		}
	}
}

func TestBootstrapProjectReadsBackAmbiguousCommitBeforeRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const product = "Commit Readback Product"
	seedBootstrapInstallation(t)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, product)
	})

	remote := &bootstrapTestRemote{}
	h := *testHandler
	h.ProjectBootstrap = remote
	h.TxStarter = bootstrapTxStarter{txStarter: testHandler.TxStarter, commit: func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("injected lost commit acknowledgement")
	}}
	req := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodPost, "/api/projects/bootstrap", map[string]string{"name": "commit-readback", "visibility": "private", "product": product}),
		"X-User-ID", testUserID,
		"X-Workspace-ID", testWorkspaceID,
	)
	testutil.Call(t, h.BootstrapProject, req).Want(http.StatusCreated)
	_, _, deletes := remote.counts()
	if deletes != 0 {
		t.Fatalf("deleted committed repository %d times", deletes)
	}
}

func TestBootstrapProjectDeletesRepositoryAfterCommitReadbackProvesAbsent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const product = "Commit Rollback Product"
	seedBootstrapInstallation(t)

	remote := &bootstrapTestRemote{}
	h := *testHandler
	h.ProjectBootstrap = remote
	h.TxStarter = bootstrapTxStarter{txStarter: testHandler.TxStarter, commit: func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return errors.New("injected ambiguous commit failure")
	}}
	req := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodPost, "/api/projects/bootstrap", map[string]string{"name": "commit-rollback", "visibility": "private", "product": product}),
		"X-User-ID", testUserID,
		"X-Workspace-ID", testWorkspaceID,
	)
	testutil.Call(t, h.BootstrapProject, req).Want(http.StatusInternalServerError)
	_, _, deletes := remote.counts()
	if deletes != 1 {
		t.Fatalf("repository deletes = %d, want 1 after absent DB readback", deletes)
	}
}

func TestBootstrapProjectAllowsOwnerAndAdminButRejectsMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedBootstrapInstallation(t)
	adminID := dbfx.User(t, "Bootstrap Admin", "bootstrap-admin@multica.test")
	dbfx.Member(t, testWorkspaceID, adminID, "admin")
	memberID := dbfx.User(t, "Bootstrap Member", "bootstrap-member@multica.test")
	dbfx.Member(t, testWorkspaceID, memberID, "member")

	for _, tc := range []struct {
		role, userID, name, product string
		want                        int
	}{
		{"owner", testUserID, "role-owner", "Bootstrap Role Owner", http.StatusCreated},
		{"admin", adminID, "role-admin", "Bootstrap Role Admin", http.StatusCreated},
		{"member", memberID, "role-member", "Bootstrap Role Member", http.StatusForbidden},
	} {
		t.Run(tc.role, func(t *testing.T) {
			remote := &bootstrapTestRemote{}
			h := *testHandler
			h.ProjectBootstrap = remote
			response := callBootstrap(t, &h, tc.userID, tc.name, tc.product)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.want == http.StatusCreated {
				var out projectBootstrapResponse
				if err := json.NewDecoder(response.Body).Decode(&out); err != nil || out.State != "created" || out.Project.ID == "" {
					t.Fatalf("malformed bootstrap response: %+v, %v", out, err)
				}
			}
			_, bootstraps, _ := remote.counts()
			if bootstraps != boolInt(tc.want == http.StatusCreated) {
				t.Fatalf("GitHub bootstraps = %d", bootstraps)
			}
		})
	}
	cleanupBootstrapProducts(t, "Bootstrap Role Owner", "Bootstrap Role Admin")
}

func TestBootstrapProjectFailsClosedOnLocalCollisionsAndAdoption(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedBootstrapInstallation(t)

	t.Run("product collision", func(t *testing.T) {
		dbfx.Project(t, "Bootstrap Product Collision")
		remote := &bootstrapTestRemote{}
		h := *testHandler
		h.ProjectBootstrap = remote
		response := callBootstrap(t, &h, testUserID, "product-collision", "Bootstrap Product Collision")
		if response.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		checks, bootstraps, _ := remote.counts()
		if checks != 0 || bootstraps != 0 {
			t.Fatalf("remote called before local collision check: checks=%d bootstraps=%d", checks, bootstraps)
		}
	})

	t.Run("resource collision", func(t *testing.T) {
		projectID := dbfx.Project(t, "Other Bootstrap Product")
		dbfx.Insert(t, "project_resource", testutil.Cols{
			"project_id": projectID, "workspace_id": testWorkspaceID, "resource_type": "github_repo",
			"resource_ref": `{"url":"https://github.com/nordicle-labs/resource-collision","default_branch_hint":"main"}`,
		})
		remote := &bootstrapTestRemote{}
		h := *testHandler
		h.ProjectBootstrap = remote
		response := callBootstrap(t, &h, testUserID, "resource-collision", "Bootstrap Resource Collision")
		if response.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	})

	t.Run("repository appears after preflight", func(t *testing.T) {
		remote := &bootstrapTestRemote{bootstrapState: "unchanged"}
		h := *testHandler
		h.ProjectBootstrap = remote
		response := callBootstrap(t, &h, testUserID, "appeared", "Bootstrap Appeared")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "refusing to adopt") {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		if got := dbfx.Count(t, `SELECT count(*) FROM project WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, "Bootstrap Appeared"); got != 0 {
			t.Fatalf("adopted repository created %d projects", got)
		}
	})
}

func TestBootstrapProjectRejectsEquivalentGitHubRepositoryURLs(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedBootstrapInstallation(t)
	for i, repoURL := range []string{
		"https://github.com/nordicle-labs/equivalent-repository/",
		"https://github.com/nordicle-labs/equivalent-repository.git",
		"git@github.com:nordicle-labs/equivalent-repository.git",
	} {
		t.Run(repoURL, func(t *testing.T) {
			projectID := dbfx.Project(t, fmt.Sprintf("Equivalent Repository %d", i))
			ref, err := json.Marshal(map[string]string{"url": repoURL, "default_branch_hint": "main"})
			if err != nil {
				t.Fatal(err)
			}
			dbfx.Insert(t, "project_resource", testutil.Cols{
				"project_id": projectID, "workspace_id": testWorkspaceID, "resource_type": "github_repo", "resource_ref": string(ref),
			})
			remote := &bootstrapTestRemote{}
			h := *testHandler
			h.ProjectBootstrap = remote
			response := callBootstrap(t, &h, testUserID, "equivalent-repository", fmt.Sprintf("New Product %d", i))
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			checks, bootstraps, _ := remote.counts()
			if checks != 0 || bootstraps != 0 {
				t.Fatalf("remote calls = check %d, bootstrap %d", checks, bootstraps)
			}
		})
	}
}

func TestBootstrapProjectSerializesDuplicateSubmissions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedBootstrapInstallation(t)
	const product = "Bootstrap Concurrent Product"
	cleanupBootstrapProducts(t, product)
	remote := &bootstrapTestRemote{}
	h := *testHandler
	h.ProjectBootstrap = remote
	start := make(chan struct{})
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			statuses <- callBootstrap(t, &h, testUserID, "concurrent-bootstrap", product).Code
		}()
	}
	close(start)
	got := []int{<-statuses, <-statuses}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusOK, http.StatusCreated}) {
		t.Fatalf("statuses = %v", got)
	}
	_, bootstraps, _ := remote.counts()
	if bootstraps != 1 || dbfx.Count(t, `SELECT count(*) FROM project WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, product) != 1 {
		t.Fatalf("bootstraps = %d; duplicate local project created", bootstraps)
	}
}

func TestBootstrapProjectSerializesSameProductAcrossRepositories(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedBootstrapInstallation(t)
	const product = "Bootstrap Concurrent Product Collision"
	cleanupBootstrapProducts(t, product)
	remote := &bootstrapTestRemote{}
	h := *testHandler
	h.ProjectBootstrap = remote
	start := make(chan struct{})
	statuses := make(chan int, 2)
	for _, name := range []string{"concurrent-product-one", "concurrent-product-two"} {
		go func() {
			<-start
			statuses <- callBootstrap(t, &h, testUserID, name, product).Code
		}()
	}
	close(start)
	got := []int{<-statuses, <-statuses}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusCreated, http.StatusConflict}) {
		t.Fatalf("statuses = %v", got)
	}
	_, bootstraps, _ := remote.counts()
	if bootstraps != 1 || dbfx.Count(t, `SELECT count(*) FROM project WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, product) != 1 {
		t.Fatalf("bootstraps = %d; product collision created duplicates", bootstraps)
	}
}

func TestBootstrapProjectReportsIncompleteRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedBootstrapInstallation(t)
	remote := &bootstrapTestRemote{deleteErr: errors.New("injected delete failure")}
	h := *testHandler
	h.ProjectBootstrap = remote
	h.TxStarter = bootstrapTxStarter{txStarter: testHandler.TxStarter, commit: func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return pgx.ErrTxCommitRollback
	}}
	response := callBootstrap(t, &h, testUserID, "rollback-incomplete", "Bootstrap Rollback Incomplete")
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "rollback incomplete; reconcile repository") {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	_, _, deletes := remote.counts()
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1", deletes)
	}
}

func callBootstrap(t *testing.T, h *Handler, userID, name, product string) *httptest.ResponseRecorder {
	t.Helper()
	req := testutil.WithHeaders(
		testutil.JSONRequest(http.MethodPost, "/api/projects/bootstrap", map[string]string{"name": name, "visibility": "private", "product": product}),
		"X-User-ID", userID,
		"X-Workspace-ID", testWorkspaceID,
	)
	response := httptest.NewRecorder()
	h.BootstrapProject(response, req)
	return response
}

func cleanupBootstrapProducts(t *testing.T, products ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project_resource WHERE project_id IN (SELECT id FROM project WHERE workspace_id = $1 AND title = ANY($2))`, testWorkspaceID, products)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE workspace_id = $1 AND title = ANY($2)`, testWorkspaceID, products)
	})
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func seedBootstrapInstallation(t *testing.T) {
	t.Helper()
	const installationID = int64(901019)
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type, connected_by_id)
		VALUES ($1, $2, 'nordicle-labs', 'Organization', $3)
	`, testWorkspaceID, installationID, testUserID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
	})
}
