package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ReleaseTransition string

const (
	TransitionPromotion  ReleaseTransition = "promotion"
	TransitionActivation ReleaseTransition = "activation"
	TransitionDeployment ReleaseTransition = "deployment"
)

type ReleaseGate string

const (
	GateCodeReview       ReleaseGate = "code_review"
	GateSecurity         ReleaseGate = "security"
	GateQA               ReleaseGate = "qa"
	GateSnapshotRollback ReleaseGate = "snapshot_rollback"
	GateSmoke            ReleaseGate = "smoke"
	GateActiveReadback   ReleaseGate = "active_version_readback"
)

type ReleasePolicyInput struct {
	RiskPolicy      string
	Public          bool
	Private         bool
	Payment         bool
	SensitiveData   bool
	ExternalUsers   bool
	Reversible      bool
	Secret          bool
	Destructive     bool
	ProductDecision bool
}

type ReleaseTransitionPlan struct {
	Transition            ReleaseTransition
	PreviewRequired       bool
	HumanApprovalRequired bool
	Gates                 []ReleaseGate
}

type ReleasePlan struct {
	RiskPolicy  string
	FailClosed  bool
	Transitions []ReleaseTransitionPlan
}

func isRiskPolicy(policy string) bool {
	switch policy {
	case "targeted_check", "tests_review", "security_integration", "explicit_approval_rollback":
		return true
	default:
		return false
	}
}

// DecideReleasePlan is the single release matrix. It only strengthens the
// impact classification selected by product intake; it never reclassifies it.
func DecideReleasePlan(in ReleasePolicyInput) ReleasePlan {
	ambiguous := !isRiskPolicy(in.RiskPolicy) || in.Public == in.Private
	sensitive := in.Public || in.Payment || in.SensitiveData || in.ExternalUsers
	human := sensitive || !in.Reversible || in.Secret || in.Destructive || in.ProductDecision ||
		in.RiskPolicy == "explicit_approval_rollback"
	preview := sensitive
	if ambiguous {
		preview = true
		human = true
	}

	baseGates := []ReleaseGate{GateCodeReview, GateSecurity, GateQA, GateSnapshotRollback, GateSmoke}
	return ReleasePlan{
		RiskPolicy: in.RiskPolicy,
		FailClosed: ambiguous,
		Transitions: []ReleaseTransitionPlan{
			{Transition: TransitionPromotion, PreviewRequired: preview, HumanApprovalRequired: human, Gates: append([]ReleaseGate(nil), baseGates...)},
			{Transition: TransitionActivation, PreviewRequired: preview, HumanApprovalRequired: human, Gates: append([]ReleaseGate(nil), baseGates...)},
			{Transition: TransitionDeployment, PreviewRequired: preview, HumanApprovalRequired: human, Gates: append(append([]ReleaseGate(nil), baseGates...), GateActiveReadback)},
		},
	}
}

var (
	ErrReleaseArtifactChanged  = errors.New("release artifact changed between gates")
	ErrReleaseApprovalRequired = errors.New("release requires human approval for the exact artifact")
	ErrReleasePlanInvalid      = errors.New("release plan does not match policy")
	ErrReleaseSmokeFailed      = errors.New("release smoke failed and rollback was attempted")
	ErrReleaseRollbackFailed   = errors.New("release rollback failed; external state is indeterminate")
	ErrReleaseRuntimeMissing   = errors.New("release runtime is missing")
)

type ReleaseState string

const (
	ReleaseStatePending        ReleaseState = "pending"
	ReleaseStatePromoted       ReleaseState = "promoted"
	ReleaseStateActivated      ReleaseState = "activated"
	ReleaseStateDeployed       ReleaseState = "deployed"
	ReleaseStateRolledBack     ReleaseState = "rolled_back"
	ReleaseStateRollbackFailed ReleaseState = "rollback_failed"
)

// ReleaseRuntime is deliberately capability-limited. Runtime wiring is added
// separately; this policy operator has no credentials or production client.
type ReleaseRuntime interface {
	Preview(context.Context, ReleaseTransition, string) (string, error)
	Check(context.Context, ReleaseTransition, ReleaseGate, string) (string, error)
	Transition(context.Context, ReleaseTransition, string) (string, error)
	Smoke(context.Context, ReleaseTransition, string) (string, error)
	ActiveProduction(context.Context) (string, error)
	Rollback(context.Context, ReleaseTransition, string) error
}

type ReleaseApprovalScope struct {
	WorkspaceID pgtype.UUID
	BindingID   pgtype.UUID
}

type ReleaseApprovalKey struct {
	ReleaseApprovalScope
	Transition  ReleaseTransition
	ArtifactSHA string
}

type ReleaseOperator struct {
	Runtime   ReleaseRuntime
	Approvals *ProductIntakeService
}

// Execute carries one immutable SHA through preview, gates, transition, smoke,
// and production readback. Human decisions are records created by
// ProductIntakeService.CreateApproval, not issue statuses such as in_review.
func (o ReleaseOperator) Execute(ctx context.Context, artifactSHA string, input ReleasePolicyInput, plan ReleasePlan, scope ReleaseApprovalScope) (ReleaseState, error) {
	if o.Runtime == nil {
		return ReleaseStatePending, ErrReleaseRuntimeMissing
	}
	if !gitSHA.MatchString(artifactSHA) {
		return ReleaseStatePending, ErrReleaseArtifactChanged
	}
	if !releasePlanEqual(plan, DecideReleasePlan(input)) {
		return ReleaseStatePending, ErrReleasePlanInvalid
	}

	state := ReleaseStatePending
	for _, step := range plan.Transitions {
		if step.PreviewRequired {
			observed, err := o.Runtime.Preview(ctx, step.Transition, artifactSHA)
			if err != nil {
				return state, fmt.Errorf("preview %s: %w", step.Transition, err)
			}
			if observed != artifactSHA {
				return state, ErrReleaseArtifactChanged
			}
		}
		for _, gate := range step.Gates {
			if gate == GateSmoke || gate == GateActiveReadback {
				continue
			}
			observed, err := o.Runtime.Check(ctx, step.Transition, gate, artifactSHA)
			if err != nil {
				return state, fmt.Errorf("%s gate for %s: %w", gate, step.Transition, err)
			}
			if observed != artifactSHA {
				return state, ErrReleaseArtifactChanged
			}
		}
		if step.HumanApprovalRequired {
			if o.Approvals == nil || !scope.WorkspaceID.Valid || !scope.BindingID.Valid {
				return state, ErrReleaseApprovalRequired
			}
			key := ReleaseApprovalKey{ReleaseApprovalScope: scope, Transition: step.Transition, ArtifactSHA: artifactSHA}
			approval, err := o.Approvals.CurrentReleaseApproval(ctx, key)
			if err != nil || !approvalAllows(approval, key) {
				return state, errors.Join(ErrReleaseApprovalRequired, err)
			}
		}

		observed, err := o.Runtime.Transition(ctx, step.Transition, artifactSHA)
		if err != nil {
			return state, fmt.Errorf("%s: %w", step.Transition, err)
		}
		if observed != artifactSHA {
			return o.rollback(ctx, step.Transition, artifactSHA, ErrReleaseArtifactChanged)
		}
		observed, smokeErr := o.Runtime.Smoke(ctx, step.Transition, artifactSHA)
		if smokeErr != nil || observed != artifactSHA {
			if observed != artifactSHA {
				smokeErr = errors.Join(smokeErr, ErrReleaseArtifactChanged)
			}
			return o.rollback(ctx, step.Transition, artifactSHA, errors.Join(ErrReleaseSmokeFailed, smokeErr))
		}
		if step.Transition == TransitionDeployment {
			activeSHA, err := o.Runtime.ActiveProduction(ctx)
			if err != nil {
				return state, fmt.Errorf("read active production: %w", err)
			}
			if activeSHA != artifactSHA {
				return o.rollback(ctx, step.Transition, artifactSHA, ErrReleaseArtifactChanged)
			}
		}
		state = stateAfter(step.Transition)
	}
	return state, nil
}

func (o ReleaseOperator) rollback(ctx context.Context, transition ReleaseTransition, artifactSHA string, cause error) (ReleaseState, error) {
	if err := o.Runtime.Rollback(ctx, transition, artifactSHA); err != nil {
		return ReleaseStateRollbackFailed, errors.Join(cause, ErrReleaseRollbackFailed, err)
	}
	return ReleaseStateRolledBack, cause
}

// CurrentReleaseApproval resolves the latest persisted human verdict for one
// exact transition and SHA. Callers cannot supply an approval record directly.
func (s *ProductIntakeService) CurrentReleaseApproval(ctx context.Context, key ReleaseApprovalKey) (db.ProductIntakeApproval, error) {
	if s == nil || s.Queries == nil || !key.WorkspaceID.Valid || !key.BindingID.Valid ||
		!gitSHA.MatchString(key.ArtifactSHA) || stateAfter(key.Transition) == ReleaseStatePending {
		return db.ProductIntakeApproval{}, ErrReleaseApprovalRequired
	}
	approval, err := s.Queries.GetCurrentProductIntakeApproval(ctx, db.GetCurrentProductIntakeApprovalParams{
		WorkspaceID: key.WorkspaceID, BindingID: key.BindingID,
		MessageID: string(key.Transition), ArtifactSha: key.ArtifactSHA,
	})
	if err != nil {
		return db.ProductIntakeApproval{}, fmt.Errorf("resolve current release approval: %w", err)
	}
	if !approvalMatchesKey(approval, key) {
		return db.ProductIntakeApproval{}, ErrReleaseApprovalRequired
	}
	return approval, nil
}

func releasePlanEqual(got, want ReleasePlan) bool {
	if got.RiskPolicy != want.RiskPolicy || got.FailClosed != want.FailClosed || len(got.Transitions) != len(want.Transitions) {
		return false
	}
	for i := range want.Transitions {
		gotStep, wantStep := got.Transitions[i], want.Transitions[i]
		if gotStep.Transition != wantStep.Transition || gotStep.PreviewRequired != wantStep.PreviewRequired ||
			gotStep.HumanApprovalRequired != wantStep.HumanApprovalRequired || !slices.Equal(gotStep.Gates, wantStep.Gates) {
			return false
		}
	}
	return true
}

func approvalMatchesKey(approval db.ProductIntakeApproval, key ReleaseApprovalKey) bool {
	return approval.ID.Valid && approval.WorkspaceID == key.WorkspaceID && approval.BindingID == key.BindingID &&
		approval.MessageID == string(key.Transition) && approval.ApproverID.Valid &&
		approval.ArtifactSha == key.ArtifactSHA
}

func approvalAllows(approval db.ProductIntakeApproval, key ReleaseApprovalKey) bool {
	return approvalMatchesKey(approval, key) && approval.Decision == "approved"
}

func stateAfter(transition ReleaseTransition) ReleaseState {
	switch transition {
	case TransitionPromotion:
		return ReleaseStatePromoted
	case TransitionActivation:
		return ReleaseStateActivated
	case TransitionDeployment:
		return ReleaseStateDeployed
	default:
		return ReleaseStatePending
	}
}
