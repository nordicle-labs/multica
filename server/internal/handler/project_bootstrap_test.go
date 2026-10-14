package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestBootstrapInstallationRequiresOneExactOwner(t *testing.T) {
	installation, err := bootstrapInstallation([]db.GithubInstallation{
		{InstallationID: 1, AccountLogin: "Nordicle-Labs"},
		{InstallationID: 2, AccountLogin: "nordicle-labs"},
	})
	if err != nil || installation.InstallationID != 2 {
		t.Fatalf("installation=%+v err=%v", installation, err)
	}
	for _, installations := range [][]db.GithubInstallation{
		{{InstallationID: 1, AccountLogin: "Nordicle-Labs"}},
		{{InstallationID: 1, AccountLogin: "nordicle-labs"}, {InstallationID: 2, AccountLogin: "nordicle-labs"}},
	} {
		if _, err := bootstrapInstallation(installations); err == nil {
			t.Fatalf("installations %+v were accepted", installations)
		}
	}
}
