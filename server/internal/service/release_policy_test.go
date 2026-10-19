package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const testReleaseSHA = "0123456789abcdef0123456789abcdef01234567"

var testReleaseScope = ReleaseApprovalScope{
	WorkspaceID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
	BindingID:   pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
}

func TestDecideReleasePlan(t *testing.T) {
	tests := []struct {
		name           string
		input          ReleasePolicyInput
		wantPreview    bool
		wantHuman      bool
		wantFailClosed bool
	}{
		{
			name:  "reversible internal is autonomous",
			input: ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true},
		},
		{
			name:        "public requires preview and exact approval",
			input:       ReleasePolicyInput{RiskPolicy: "tests_review", Public: true, Reversible: true},
			wantPreview: true, wantHuman: true,
		},
		{
			name:      "secret requires human",
			input:     ReleasePolicyInput{RiskPolicy: "security_integration", Private: true, Reversible: true, Secret: true},
			wantHuman: true,
		},
		{
			name:      "destructive requires human",
			input:     ReleasePolicyInput{RiskPolicy: "security_integration", Private: true, Reversible: true, Destructive: true},
			wantHuman: true,
		},
		{
			name:      "product decision requires human",
			input:     ReleasePolicyInput{RiskPolicy: "targeted_check", Private: true, Reversible: true, ProductDecision: true},
			wantHuman: true,
		},
		{
			name:        "unknown impact fails closed",
			input:       ReleasePolicyInput{RiskPolicy: "unknown", Private: true, Reversible: true},
			wantPreview: true, wantHuman: true, wantFailClosed: true,
		},
		{
			name:        "ambiguous visibility fails closed",
			input:       ReleasePolicyInput{RiskPolicy: "targeted_check", Reversible: true},
			wantPreview: true, wantHuman: true, wantFailClosed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := DecideReleasePlan(tc.input)
			if plan.FailClosed != tc.wantFailClosed {
				t.Fatalf("FailClosed = %v, want %v", plan.FailClosed, tc.wantFailClosed)
			}
			if len(plan.Transitions) != 3 {
				t.Fatalf("transitions = %d, want 3", len(plan.Transitions))
			}
			for _, transition := range plan.Transitions {
				if transition.PreviewRequired != tc.wantPreview || transition.HumanApprovalRequired != tc.wantHuman {
					t.Errorf("%s preview/human = %v/%v, want %v/%v", transition.Transition, transition.PreviewRequired, transition.HumanApprovalRequired, tc.wantPreview, tc.wantHuman)
				}
				for _, required := range []ReleaseGate{GateCodeReview, GateSecurity, GateQA, GateSnapshotRollback, GateSmoke} {
					if !containsReleaseGate(transition.Gates, required) {
						t.Errorf("%s gates %v omit %s", transition.Transition, transition.Gates, required)
					}
				}
			}
		})
	}
}

func TestReleaseOperatorRejectsSHAChangeBetweenGates(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, mismatchAction: string(GateSecurity)}
	op := ReleaseOperator{Runtime: runtime}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	_, err := op.Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if !errors.Is(err, ErrReleaseArtifactChanged) {
		t.Fatalf("Execute error = %v, want ErrReleaseArtifactChanged", err)
	}
	if len(runtime.transitions) != 0 {
		t.Fatalf("transitions after SHA mismatch = %v, want none", runtime.transitions)
	}
}

func TestReleaseOperatorRollsBackOnFirstRedSmoke(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, smokeErr: errors.New("red")}
	op := ReleaseOperator{Runtime: runtime}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	state, err := op.Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if !errors.Is(err, ErrReleaseSmokeFailed) {
		t.Fatalf("Execute error = %v, want ErrReleaseSmokeFailed", err)
	}
	if state != ReleaseStateRolledBack {
		t.Fatalf("state = %s, want %s", state, ReleaseStateRolledBack)
	}
	if !reflect.DeepEqual(runtime.rollbacks, []ReleaseTransition{TransitionPromotion}) {
		t.Fatalf("rollbacks = %v, want promotion only", runtime.rollbacks)
	}
	if !reflect.DeepEqual(runtime.transitions, []ReleaseTransition{TransitionPromotion}) {
		t.Fatalf("transitions = %v, want promotion only", runtime.transitions)
	}
}

func TestReleaseOperatorRollsBackAfterTransitionSHAMismatch(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, mismatchAction: string(TransitionPromotion)}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	state, err := (ReleaseOperator{Runtime: runtime}).Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if !errors.Is(err, ErrReleaseArtifactChanged) || state != ReleaseStateRolledBack {
		t.Fatalf("Execute state/error = %s/%v, want rolled_back/ErrReleaseArtifactChanged", state, err)
	}
	if !reflect.DeepEqual(runtime.rollbacks, []ReleaseTransition{TransitionPromotion}) {
		t.Fatalf("rollbacks = %v, want promotion", runtime.rollbacks)
	}
}

func TestReleaseOperatorRollsBackAfterReadbackSHAMismatch(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, activeSHA: "1123456789abcdef0123456789abcdef01234567"}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	state, err := (ReleaseOperator{Runtime: runtime}).Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if !errors.Is(err, ErrReleaseArtifactChanged) || state != ReleaseStateRolledBack {
		t.Fatalf("Execute state/error = %s/%v, want rolled_back/ErrReleaseArtifactChanged", state, err)
	}
	if !reflect.DeepEqual(runtime.rollbacks, []ReleaseTransition{TransitionDeployment}) {
		t.Fatalf("rollbacks = %v, want deployment", runtime.rollbacks)
	}
}

func TestReleaseOperatorReportsRollbackFailure(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, smokeErr: errors.New("red"), rollbackErr: errors.New("rollback unavailable")}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	state, err := (ReleaseOperator{Runtime: runtime}).Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if !errors.Is(err, ErrReleaseRollbackFailed) || state != ReleaseStateRollbackFailed {
		t.Fatalf("Execute state/error = %s/%v, want rollback_failed/ErrReleaseRollbackFailed", state, err)
	}
}

func TestReleaseOperatorRejectsNonCanonicalPlansBeforeRuntime(t *testing.T) {
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Public: true, Reversible: true}
	tests := []struct {
		name   string
		mutate func(*ReleasePlan)
	}{
		{name: "empty plan", mutate: func(plan *ReleasePlan) { *plan = ReleasePlan{} }},
		{name: "removed gate", mutate: func(plan *ReleasePlan) { plan.Transitions[0].Gates = plan.Transitions[0].Gates[1:] }},
		{name: "changed transition order", mutate: func(plan *ReleasePlan) {
			plan.Transitions[0], plan.Transitions[1] = plan.Transitions[1], plan.Transitions[0]
		}},
		{name: "unknown transition", mutate: func(plan *ReleasePlan) { plan.Transitions[0].Transition = ReleaseTransition("unknown") }},
		{name: "weakened preview flag", mutate: func(plan *ReleasePlan) { plan.Transitions[0].PreviewRequired = false }},
		{name: "weakened approval flag", mutate: func(plan *ReleasePlan) { plan.Transitions[0].HumanApprovalRequired = false }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := DecideReleasePlan(input)
			tc.mutate(&plan)
			runtime := &fakeReleaseRuntime{observed: testReleaseSHA}
			state, err := (ReleaseOperator{Runtime: runtime}).Execute(context.Background(), testReleaseSHA, input, plan, testReleaseScope)
			if !errors.Is(err, ErrReleasePlanInvalid) || state != ReleaseStatePending {
				t.Fatalf("Execute state/error = %s/%v, want pending/ErrReleasePlanInvalid", state, err)
			}
			if len(runtime.previews)+len(runtime.transitions) != 0 {
				t.Fatalf("runtime called for invalid plan: previews=%v transitions=%v", runtime.previews, runtime.transitions)
			}
		})
	}
}

func TestReleaseOperatorRejectsStructurallyValidUnpersistedApproval(t *testing.T) {
	approvals, scope, _ := newReleaseApprovalFixture(t)
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Public: true, Reversible: true}
	fabricated := db.ProductIntakeApproval{
		ID: uuidPG(uuid.NewString()), WorkspaceID: scope.WorkspaceID, BindingID: scope.BindingID,
		MessageID: string(TransitionPromotion), ApproverID: uuidPG(uuid.NewString()),
		ArtifactSha: testReleaseSHA, Decision: "approved", CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA}

	_, err := (ReleaseOperator{Runtime: runtime, Approvals: approvals}).Execute(
		context.Background(), fabricated.ArtifactSha, input, DecideReleasePlan(input), scope,
	)
	if !errors.Is(err, ErrReleaseApprovalRequired) {
		t.Fatalf("Execute error = %v, want ErrReleaseApprovalRequired", err)
	}
}

func TestCurrentReleaseApprovalReturnsLatestPersistentVerdict(t *testing.T) {
	approvals, scope, approverID := newReleaseApprovalFixture(t)
	ctx := context.Background()
	approved, err := approvals.CreateApproval(ctx, ProductIntakeApprovalRequest{
		WorkspaceID: scope.WorkspaceID, BindingID: scope.BindingID, ApproverID: approverID,
		MessageID: string(TransitionPromotion), ArtifactSHA: testReleaseSHA, Decision: "approved",
	})
	if err != nil {
		t.Fatalf("create approved verdict: %v", err)
	}
	denied, err := approvals.CreateApproval(ctx, ProductIntakeApprovalRequest{
		WorkspaceID: scope.WorkspaceID, BindingID: scope.BindingID, ApproverID: approverID,
		MessageID: string(TransitionPromotion), ArtifactSHA: testReleaseSHA, Decision: "denied",
	})
	if err != nil {
		t.Fatalf("create denied verdict: %v", err)
	}

	got, err := approvals.CurrentReleaseApproval(ctx, ReleaseApprovalKey{ReleaseApprovalScope: scope, Transition: TransitionPromotion, ArtifactSHA: testReleaseSHA})
	if err != nil {
		t.Fatalf("resolve current verdict: %v", err)
	}
	if got.ID != denied.ID || got.ID == approved.ID || got.Decision != "denied" {
		t.Fatalf("current verdict = %s/%v, want denied/%v", got.Decision, got.ID, denied.ID)
	}
}

func TestApprovalVerdictResolutionSeparatesProvenanceFromAuthorization(t *testing.T) {
	key := ReleaseApprovalKey{ReleaseApprovalScope: testReleaseScope, Transition: TransitionPromotion, ArtifactSHA: testReleaseSHA}
	denied := db.ProductIntakeApproval{
		ID: uuidPG(uuid.NewString()), WorkspaceID: key.WorkspaceID, BindingID: key.BindingID,
		MessageID: string(key.Transition), ApproverID: uuidPG(uuid.NewString()),
		ArtifactSha: key.ArtifactSHA, Decision: "denied",
	}

	if !approvalMatchesKey(denied, key) {
		t.Fatal("latest denied verdict should resolve for the exact release key")
	}
	if approvalAllows(denied, key) {
		t.Fatal("latest denied verdict must not authorize a transition")
	}
}

func TestReleaseOperatorRereadsApprovalAfterGates(t *testing.T) {
	approvals, scope, approverID := newReleaseApprovalFixture(t)
	ctx := context.Background()
	if _, err := approvals.CreateApproval(ctx, ProductIntakeApprovalRequest{
		WorkspaceID: scope.WorkspaceID, BindingID: scope.BindingID, ApproverID: approverID,
		MessageID: string(TransitionPromotion), ArtifactSHA: testReleaseSHA, Decision: "approved",
	}); err != nil {
		t.Fatalf("create approved verdict: %v", err)
	}
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA}
	runtime.onCheck = func(gate ReleaseGate) {
		if gate != GateSnapshotRollback {
			return
		}
		if _, err := approvals.CreateApproval(ctx, ProductIntakeApprovalRequest{
			WorkspaceID: scope.WorkspaceID, BindingID: scope.BindingID, ApproverID: approverID,
			MessageID: string(TransitionPromotion), ArtifactSHA: testReleaseSHA, Decision: "denied",
		}); err != nil {
			t.Fatalf("revoke during gates: %v", err)
		}
	}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Public: true, Reversible: true}

	state, err := (ReleaseOperator{Runtime: runtime, Approvals: approvals}).Execute(ctx, testReleaseSHA, input, DecideReleasePlan(input), scope)
	if !errors.Is(err, ErrReleaseApprovalRequired) || state != ReleaseStatePending {
		t.Fatalf("Execute state/error = %s/%v, want pending/ErrReleaseApprovalRequired", state, err)
	}
	if len(runtime.transitions) != 0 {
		t.Fatalf("transitions after revocation = %v, want none", runtime.transitions)
	}
}

func TestReleaseOperatorReadsBackActiveProduction(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, activeSHA: testReleaseSHA}
	op := ReleaseOperator{Runtime: runtime}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	state, err := op.Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if state != ReleaseStateDeployed || runtime.readbacks != 1 {
		t.Fatalf("state/readbacks = %s/%d, want deployed/1", state, runtime.readbacks)
	}
}

func TestReleaseOperatorDoesNotPublishDeployedBeforeReadback(t *testing.T) {
	runtime := &fakeReleaseRuntime{observed: testReleaseSHA, activeSHA: "1123456789abcdef0123456789abcdef01234567"}
	input := ReleasePolicyInput{RiskPolicy: "tests_review", Private: true, Reversible: true}

	state, err := (ReleaseOperator{Runtime: runtime}).Execute(context.Background(), testReleaseSHA, input, DecideReleasePlan(input), ReleaseApprovalScope{})
	if !errors.Is(err, ErrReleaseArtifactChanged) || state != ReleaseStateRolledBack {
		t.Fatalf("Execute state/error = %s/%v, want rolled_back/ErrReleaseArtifactChanged", state, err)
	}
}

func newReleaseApprovalFixture(t *testing.T) (*ProductIntakeService, ReleaseApprovalScope, pgtype.UUID) {
	t.Helper()
	pool := sharedTestPool(t)
	suffix := time.Now().UnixNano()
	fx := testutil.New(pool, "", "")
	actorID := fx.User(t, "Release actor", fmt.Sprintf("release-actor-%d@example.com", suffix))
	approver := fx.User(t, "Release approver", fmt.Sprintf("release-approver-%d@example.com", suffix))
	workspaceID := fx.Workspace(t, "Release workspace", fmt.Sprintf("release-%d", suffix))
	fx.Member(t, workspaceID, approver, "admin")
	bindingID := fx.Insert(t, "product_intake_binding", testutil.Cols{
		"workspace_id": workspaceID, "source_type": IntakeSourceMulticaChat, "source_id": "release-test",
		"conversation_id": fmt.Sprintf("release-%d", suffix), "actor_id": actorID, "target_agent_id": uuid.NewString(),
		"product_key": "release", "project_id": uuid.NewString(), "squad_id": uuid.NewString(),
		"required_capability": "developer", "risk_policy": "explicit_approval_rollback", "revision": 1,
		"status": "active", "valid_until": time.Now().Add(time.Hour), "created_by": approver, "change_reason": "release test",
	})
	return NewProductIntakeService(db.New(pool), nil, nil, nil), ReleaseApprovalScope{
		WorkspaceID: uuidPG(workspaceID), BindingID: uuidPG(bindingID),
	}, uuidPG(approver)
}

func uuidPG(value string) pgtype.UUID {
	return util.MustParseUUID(value)
}

type fakeReleaseRuntime struct {
	observed       string
	activeSHA      string
	mismatchAction string
	smokeErr       error
	rollbackErr    error
	onCheck        func(ReleaseGate)
	previews       []ReleaseTransition
	transitions    []ReleaseTransition
	rollbacks      []ReleaseTransition
	readbacks      int
}

func (f *fakeReleaseRuntime) Preview(_ context.Context, transition ReleaseTransition, _ string) (string, error) {
	f.previews = append(f.previews, transition)
	return f.observe("preview"), nil
}

func (f *fakeReleaseRuntime) Check(_ context.Context, _ ReleaseTransition, gate ReleaseGate, _ string) (string, error) {
	if f.onCheck != nil {
		f.onCheck(gate)
	}
	return f.observe(string(gate)), nil
}

func (f *fakeReleaseRuntime) Transition(_ context.Context, transition ReleaseTransition, _ string) (string, error) {
	f.transitions = append(f.transitions, transition)
	return f.observe(string(transition)), nil
}

func (f *fakeReleaseRuntime) Smoke(_ context.Context, _ ReleaseTransition, _ string) (string, error) {
	return f.observe(string(GateSmoke)), f.smokeErr
}

func (f *fakeReleaseRuntime) ActiveProduction(context.Context) (string, error) {
	f.readbacks++
	return f.activeSHA, nil
}

func (f *fakeReleaseRuntime) Rollback(_ context.Context, transition ReleaseTransition, _ string) error {
	f.rollbacks = append(f.rollbacks, transition)
	return f.rollbackErr
}

func (f *fakeReleaseRuntime) observe(action string) string {
	if action == f.mismatchAction {
		return "1123456789abcdef0123456789abcdef01234567"
	}
	return f.observed
}

func containsReleaseGate(gates []ReleaseGate, want ReleaseGate) bool {
	for _, gate := range gates {
		if gate == want {
			return true
		}
	}
	return false
}
