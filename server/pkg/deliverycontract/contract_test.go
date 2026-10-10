package deliverycontract

import (
	"strings"
	"testing"
	"time"
)

func validPreflight(t *testing.T) Preflight {
	t.Helper()
	profile := CurrentProfile()
	hash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return Preflight{
		ProfileVersion: profile.Version,
		ProfileHash:    hash,
		ResourceReady:  true,
		DiskWritable:   true,
		CLIAvailable:   true,
		Authenticated:  true,
		Toolsets:       append([]string(nil), profile.RequiredToolsets...),
		Services:       map[string]bool{"postgres": true, "pgvector": true},
	}
}

func TestNegotiateProfileAcceptsServerV1(t *testing.T) {
	profile, err := NegotiateProfile(ProfileVersionV1)
	if err != nil {
		t.Fatalf("negotiate v1: %v", err)
	}
	if profile.Version != ProfileVersionV1 {
		t.Fatalf("profile version = %q", profile.Version)
	}
	if CurrentProfile().Version != ProfileVersionV1 {
		t.Fatalf("emitted profile version = %q", CurrentProfile().Version)
	}

	// Services are an additive v1 extension: the legacy server hash remains
	// stable while updated servers can still require the probes.
	hash, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	const legacyHash = "d0b6878dcce8b3123baa43ea864882cbea4d80d27a6e9da29674172390b3be61"
	if hash != legacyHash {
		t.Fatalf("v1 profile hash = %q, want legacy hash %q", hash, legacyHash)
	}
	if len(profile.RequiredServices) == 0 {
		t.Fatal("v1 compatibility removed service preflight requirements")
	}
}

func TestNegotiateProfileRejectsUnknownVersion(t *testing.T) {
	if _, err := NegotiateProfile("delivery-v999"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestPreflightValidatesVersionedCapabilitiesAndRepository(t *testing.T) {
	profile := CurrentProfile()
	report := validPreflight(t)
	report.RepositoryRequired = true
	report.Repository = Repository{Git: true, Branch: "agent/developer/HERM-941", CommitSHA: strings.Repeat("a", 40), Worktree: true}
	if err := report.Validate(profile); err != nil {
		t.Fatalf("valid preflight rejected: %v", err)
	}

	report.ProfileHash = strings.Repeat("0", 64)
	if err := report.Validate(profile); err == nil || !strings.Contains(err.Error(), "capability hash") {
		t.Fatalf("hash mismatch error = %v", err)
	}

	report = validPreflight(t)
	report.Services["pgvector"] = false
	if err := report.Validate(profile); err == nil || !strings.Contains(err.Error(), "pgvector") {
		t.Fatalf("missing pgvector error = %v", err)
	}
}

func TestArtifactAndGateContract(t *testing.T) {
	sha := strings.Repeat("b", 40)
	artifact := Artifact{Branch: "agent/developer/HERM-941", CommitSHA: sha, RemoteSHA: sha, CleanTree: true}
	if err := artifact.ValidateReadyForReview(); err != nil {
		t.Fatal(err)
	}

	state := GateState{}
	state.SetArtifact(sha)
	if err := state.RecordSecurity(sha, VerdictApproved); err != nil {
		t.Fatal(err)
	}
	if err := state.RecordQA(sha, VerdictVerified); err != nil {
		t.Fatal(err)
	}
	if !state.ReadyForRelease() {
		t.Fatal("approved and verified artifact should be releasable")
	}

	newSHA := strings.Repeat("c", 40)
	state.SetArtifact(newSHA)
	if state.Security != VerdictPending || state.QA != VerdictPending || state.ReadyForRelease() {
		t.Fatalf("new artifact did not invalidate gates: %+v", state)
	}
	if err := state.RecordQA(sha, VerdictVerified); err == nil {
		t.Fatal("accepted verdict for stale artifact")
	}
}

func TestProgressAndReleaseContract(t *testing.T) {
	now := time.Unix(10, 0).UTC()
	progress := ProgressState{}
	progress.RecordAttempt(now)
	progress.RecordBlocked(now.Add(time.Minute))
	if !progress.LastProgressAt.IsZero() || !progress.LastArtifactAt.IsZero() {
		t.Fatalf("blocked attempt counted as progress: %+v", progress)
	}
	progress.RecordProgress(now.Add(2 * time.Minute))
	progress.RecordArtifact(now.Add(3 * time.Minute))
	if !progress.LastProgressAt.Equal(now.Add(3*time.Minute)) || !progress.LastArtifactAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("artifact timestamps = %+v", progress)
	}

	if err := (ReleaseEvidence{Security: VerdictApproved, QA: VerdictVerified}).Validate(); err == nil {
		t.Fatal("release passed without rollback/merge/CI/deploy/smoke/sync evidence")
	}
	complete := ReleaseEvidence{Security: VerdictApproved, QA: VerdictVerified, RollbackTag: true, Merged: true, CIPassed: true, Deployed: true, SmokePassed: true, SurfaceSynced: true}
	if err := complete.Validate(); err != nil {
		t.Fatalf("complete release rejected: %v", err)
	}
}
