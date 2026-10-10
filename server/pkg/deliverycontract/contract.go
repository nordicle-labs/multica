package deliverycontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const CurrentVersion = "delivery-v2"

var requiredToolsets = []string{"code_execution", "file", "terminal"}
var requiredServices = []string{"postgres", "pgvector"}

type CapabilityProfile struct {
	Version          string   `json:"version"`
	RequiredToolsets []string `json:"required_toolsets"`
	RequiredServices []string `json:"required_services"`
}

func CurrentProfile() CapabilityProfile {
	return CapabilityProfile{
		Version:          CurrentVersion,
		RequiredToolsets: append([]string(nil), requiredToolsets...),
		RequiredServices: append([]string(nil), requiredServices...),
	}
}

func (p CapabilityProfile) Hash() (string, error) {
	toolsets := append([]string(nil), p.RequiredToolsets...)
	slices.Sort(toolsets)
	services := append([]string(nil), p.RequiredServices...)
	slices.Sort(services)
	data, err := json.Marshal(CapabilityProfile{Version: p.Version, RequiredToolsets: toolsets, RequiredServices: services})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type Repository struct {
	Git       bool   `json:"git"`
	Branch    string `json:"branch,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Worktree  bool   `json:"worktree"`
}

type Preflight struct {
	ProfileVersion     string          `json:"profile_version"`
	ProfileHash        string          `json:"profile_hash"`
	ResourceReady      bool            `json:"resource_ready"`
	RepositoryRequired bool            `json:"repository_required"`
	Repository         Repository      `json:"repository"`
	DiskWritable       bool            `json:"disk_writable"`
	CLIAvailable       bool            `json:"cli_available"`
	Authenticated      bool            `json:"authenticated"`
	Toolsets           []string        `json:"toolsets"`
	Services           map[string]bool `json:"services"`
	CanonicalBranch    string          `json:"canonical_branch,omitempty"`
	RunBaseSHA         string          `json:"run_base_sha,omitempty"`
	CanonicalRefSHA    string          `json:"canonical_ref_sha,omitempty"`
}

func (p Preflight) Validate(profile CapabilityProfile) error {
	expectedHash, err := profile.Hash()
	if err != nil {
		return fmt.Errorf("hash capability profile: %w", err)
	}
	if p.ProfileVersion != profile.Version {
		return fmt.Errorf("capability profile version %q does not match %q", p.ProfileVersion, profile.Version)
	}
	if p.ProfileHash != expectedHash {
		return errors.New("capability hash does not match profile")
	}
	if !p.ResourceReady {
		return errors.New("project resource is unavailable")
	}
	if !p.DiskWritable {
		return errors.New("worktree disk is not writable")
	}
	if !p.CLIAvailable {
		return errors.New("multica CLI is unavailable")
	}
	if !p.Authenticated {
		return errors.New("task authentication is unavailable")
	}
	for _, required := range profile.RequiredToolsets {
		if !slices.Contains(p.Toolsets, required) {
			return fmt.Errorf("required toolset %q is unavailable", required)
		}
	}
	for _, required := range profile.RequiredServices {
		if !p.Services[required] {
			return fmt.Errorf("required service %q is unavailable", required)
		}
	}
	if p.RepositoryRequired {
		if !p.Repository.Git || !p.Repository.Worktree || strings.TrimSpace(p.Repository.Branch) == "" || !validSHA(p.Repository.CommitSHA) {
			return errors.New("git repository, branch, commit SHA, and worktree are required")
		}
	}
	if p.CanonicalBranch != "" {
		if p.Repository.Branch != p.CanonicalBranch || !validSHA(p.RunBaseSHA) || !validSHA(p.CanonicalRefSHA) {
			return errors.New("canonical branch, run base SHA, and expected ref SHA must match the prepared repository")
		}
	}
	return nil
}

type Artifact struct {
	Branch    string `json:"branch"`
	CommitSHA string `json:"commit_sha"`
	RemoteSHA string `json:"remote_sha"`
	CleanTree bool   `json:"clean_tree"`
}

func (a Artifact) ValidateReadyForReview() error {
	if strings.TrimSpace(a.Branch) == "" || !validSHA(a.CommitSHA) || !validSHA(a.RemoteSHA) {
		return errors.New("branch, commit SHA, and remote SHA are required")
	}
	if !strings.EqualFold(a.CommitSHA, a.RemoteSHA) {
		return errors.New("remote SHA does not match commit SHA")
	}
	if !a.CleanTree {
		return errors.New("working tree is not clean")
	}
	return nil
}

func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type ProgressState struct {
	LastAttemptAt  time.Time `json:"last_attempt_at,omitempty"`
	LastProgressAt time.Time `json:"last_progress_at,omitempty"`
	LastArtifactAt time.Time `json:"last_artifact_at,omitempty"`
}

func (s *ProgressState) RecordAttempt(at time.Time)  { s.LastAttemptAt = at }
func (s *ProgressState) RecordBlocked(time.Time)     {}
func (s *ProgressState) RecordProgress(at time.Time) { s.LastProgressAt = at }
func (s *ProgressState) RecordArtifact(at time.Time) {
	s.LastArtifactAt = at
	s.LastProgressAt = at
}

type Verdict string

const (
	VerdictPending  Verdict = "pending"
	VerdictApproved Verdict = "approved"
	VerdictVerified Verdict = "verified"
	VerdictRejected Verdict = "rejected"
)

type GateState struct {
	ArtifactSHA string  `json:"artifact_sha"`
	Security    Verdict `json:"security"`
	QA          Verdict `json:"qa"`
}

func (s *GateState) SetArtifact(sha string) {
	if strings.EqualFold(s.ArtifactSHA, sha) {
		return
	}
	s.ArtifactSHA = sha
	s.Security = VerdictPending
	s.QA = VerdictPending
}

func (s *GateState) RecordSecurity(sha string, verdict Verdict) error {
	if !strings.EqualFold(s.ArtifactSHA, sha) {
		return errors.New("security verdict targets a stale artifact")
	}
	s.Security = verdict
	return nil
}

func (s *GateState) RecordQA(sha string, verdict Verdict) error {
	if !strings.EqualFold(s.ArtifactSHA, sha) {
		return errors.New("QA verdict targets a stale artifact")
	}
	s.QA = verdict
	return nil
}

func (s GateState) ReadyForRelease() bool {
	return validSHA(s.ArtifactSHA) && s.Security == VerdictApproved && s.QA == VerdictVerified
}

type ReleaseEvidence struct {
	Security      Verdict `json:"security"`
	QA            Verdict `json:"qa"`
	RollbackTag   bool    `json:"rollback_tag"`
	Merged        bool    `json:"merged"`
	CIPassed      bool    `json:"ci_passed"`
	Deployed      bool    `json:"deployed"`
	SmokePassed   bool    `json:"smoke_passed"`
	SurfaceSynced bool    `json:"surface_synced"`
}

func (e ReleaseEvidence) Validate() error {
	if e.Security != VerdictApproved || e.QA != VerdictVerified {
		return errors.New("release requires APPROVED security and VERIFIED QA verdicts")
	}
	if !e.RollbackTag || !e.Merged || !e.CIPassed || !e.Deployed || !e.SmokePassed || !e.SurfaceSynced {
		return errors.New("release requires rollback tag, merge, CI, deployment, smoke test, and active-surface sync")
	}
	return nil
}
