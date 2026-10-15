package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/githubapp"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type bootstrapTestRemote struct{ deletes int }

func (r *bootstrapTestRemote) Check(context.Context, int64, string, string, string) (bool, githubapp.BootstrapResult, error) {
	return false, githubapp.BootstrapResult{}, nil
}

func (r *bootstrapTestRemote) Bootstrap(_ context.Context, _ int64, name, _, _ string) (githubapp.BootstrapResult, error) {
	return githubapp.BootstrapResult{State: "created", RepositoryID: 123, RepositoryURL: "https://github.com/nordicle-labs/" + name, Branch: "main"}, nil
}

func (r *bootstrapTestRemote) Delete(context.Context, int64, string, int64) error {
	r.deletes++
	return nil
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
	if remote.deletes != 0 {
		t.Fatalf("deleted committed repository %d times", remote.deletes)
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
	if remote.deletes != 1 {
		t.Fatalf("repository deletes = %d, want 1 after absent DB readback", remote.deletes)
	}
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
