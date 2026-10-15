package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestBootstrapProjectRequiresHumanActor(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap", nil)
	req.Header.Set("X-Actor-Source", "task_token")
	response := httptest.NewRecorder()
	(&Handler{}).BootstrapProject(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestBootstrapInstallationRequiresOneExactOwner(t *testing.T) {
	installation, err := bootstrapInstallation([]db.GithubInstallation{
		{InstallationID: 1, AccountLogin: "Nordicle-Labs", AccountType: "Organization"},
		{InstallationID: 2, AccountLogin: "nordicle-labs", AccountType: "Organization"},
	})
	if err != nil || installation.InstallationID != 2 {
		t.Fatalf("installation=%+v err=%v", installation, err)
	}
	for _, installations := range [][]db.GithubInstallation{
		{{InstallationID: 1, AccountLogin: "Nordicle-Labs", AccountType: "Organization"}},
		{{InstallationID: 1, AccountLogin: "nordicle-labs", AccountType: "User"}},
		{{InstallationID: 1, AccountLogin: "nordicle-labs", AccountType: "Organization"}, {InstallationID: 2, AccountLogin: "nordicle-labs", AccountType: "Organization"}},
	} {
		if _, err := bootstrapInstallation(installations); err == nil {
			t.Fatalf("installations %+v were accepted", installations)
		}
	}
}
