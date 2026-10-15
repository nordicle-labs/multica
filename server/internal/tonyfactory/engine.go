package tonyfactory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type SourceKind string

type Kind string

type Risk string

type Status string

type EvidenceStage string

const (
	SourceTony     SourceKind = "tony"
	SourceTelegram SourceKind = "telegram"

	KindIdea     Kind = "idea"
	KindBug      Kind = "bug"
	KindFeature  Kind = "feature"
	KindIncident Kind = "incident"
	KindResearch Kind = "research"
	KindProject  Kind = "project"
	KindRelease  Kind = "release"

	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"

	StatusPending     Status = "pending"
	StatusActive      Status = "active"
	StatusAgentFailed Status = "agent_failed"
	StatusCancelled   Status = "cancelled"

	StageReview     EvidenceStage = "review"
	StageQA         EvidenceStage = "qa"
	StageMerge      EvidenceStage = "merge"
	StageActivation EvidenceStage = "activation"
	StageProduction EvidenceStage = "production"
)

var (
	ErrBindingMissing   = errors.New("tony factory: source binding missing")
	ErrBindingAmbiguous = errors.New("tony factory: source binding ambiguous")
	ErrBindingStale     = errors.New("tony factory: source binding stale")
	ErrUnclassified     = errors.New("tony factory: intake is not explicitly classified")
	ErrPolicyMissing    = errors.New("tony factory: risk policy missing")
	ErrRouteMissing     = errors.New("tony factory: capability route missing")
	ErrRoleDenied       = errors.New("tony factory: routed role is not allowed")
	ErrGateMissing      = errors.New("tony factory: required human gate missing")
	ErrRunInvalid       = errors.New("tony factory: dispatched run has no id or started_at")
	ErrStateMissing     = errors.New("tony factory: workflow state missing")
	ErrCancelled        = errors.New("tony factory: workflow cancelled")
	ErrWakeupLimit      = errors.New("tony factory: wakeup limit reached")
	ErrArtifactMismatch = errors.New("tony factory: evidence artifact SHA mismatch")
	ErrEvidenceStage    = errors.New("tony factory: invalid evidence stage")
	ErrEvidenceVerdict  = errors.New("tony factory: invalid evidence verdict")
	ErrEvidenceOrder    = errors.New("tony factory: evidence transition is out of order")
	ErrConfigInvalid    = errors.New("tony factory: invalid contract configuration")
)

type Source struct {
	Kind SourceKind `json:"kind"`
	ID   string     `json:"id"`
}

type Binding struct {
	Source     Source    `json:"source"`
	ProductID  string    `json:"product_id"`
	ProjectID  string    `json:"project_id"`
	PolicyID   string    `json:"policy_id"`
	ValidUntil time.Time `json:"valid_until"`
}

type RiskPolicy struct {
	Risk       Risk     `json:"risk"`
	EntryGates []string `json:"entry_gates,omitempty"`
}

type Route struct {
	Role      string `json:"role"`
	Squad     string `json:"squad"`
	DependsOn []Kind `json:"depends_on,omitempty"`
}

type Config struct {
	Version      int                   `json:"version"`
	MaxWakeups   int                   `json:"max_wakeups"`
	Bindings     []Binding             `json:"bindings"`
	Policies     map[string]RiskPolicy `json:"policies"`
	Routes       map[Kind]Route        `json:"routes"`
	AllowedRoles map[string]bool       `json:"allowed_roles"`
}

type Message struct {
	Source       Source
	ID           string
	CanonicalKey string
	Text         string
	GrantedGates []string
}

type Run struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
}

type Evidence struct {
	Verdict    string    `json:"verdict"`
	RecordedAt time.Time `json:"recorded_at"`
}

type State struct {
	Key            string                     `json:"key"`
	Source         Source                     `json:"source"`
	MessageID      string                     `json:"message_id"`
	ProductID      string                     `json:"product_id"`
	ProjectID      string                     `json:"project_id"`
	PolicyID       string                     `json:"policy_id"`
	Kind           Kind                       `json:"kind"`
	Risk           Risk                       `json:"risk"`
	Role           string                     `json:"role"`
	Squad          string                     `json:"squad"`
	IssueID        string                     `json:"issue_id"`
	IssueCreated   bool                       `json:"issue_created"`
	BoardStatus    string                     `json:"board_status"`
	Status         Status                     `json:"status"`
	Run            Run                        `json:"run"`
	ArtifactSHA    string                     `json:"artifact_sha,omitempty"`
	Evidence       map[EvidenceStage]Evidence `json:"evidence,omitempty"`
	Wakeups        int                        `json:"wakeups"`
	NotifiedStatus Status                     `json:"notified_status,omitempty"`
	LastError      string                     `json:"last_error,omitempty"`
	GrantedGates   []string                   `json:"granted_gates,omitempty"`
}

type IssueRequest struct {
	ProjectID      string
	ProductID      string
	CanonicalKey   string
	Title          string
	Kind           Kind
	IdempotencyKey string
}

type DispatchRequest struct {
	IssueID        string
	ProjectID      string
	Role           string
	Squad          string
	Kind           Kind
	DependsOn      []Kind
	IdempotencyKey string
}

type Notification struct {
	Source         Source
	CorrelationKey string
	IssueID        string
	RunID          string
	Status         Status
	IdempotencyKey string
}

type IssueResolver interface {
	Resolve(context.Context, IssueRequest) (issueID string, created bool, err error)
}

type Dispatcher interface {
	Dispatch(context.Context, DispatchRequest) (Run, error)
}

type Notifier interface {
	Notify(context.Context, Notification) error
}

type Engine struct {
	config     Config
	store      *FileStore
	issues     IssueResolver
	dispatcher Dispatcher
	notifier   Notifier
	now        func() time.Time
}

func New(config Config, store *FileStore, issues IssueResolver, dispatcher Dispatcher, notifier Notifier) *Engine {
	return &Engine{config: config, store: store, issues: issues, dispatcher: dispatcher, notifier: notifier, now: time.Now}
}

func (e *Engine) Process(ctx context.Context, msg Message) (State, error) {
	binding, policy, route, kind, err := e.plan(msg)
	if err != nil {
		return State{}, err
	}
	key := workflowKey(e.config.Version, msg.Source, msg.ID)
	return e.store.withLock(func(records map[string]State, persist func() error) (State, error) {
		if state, ok := records[key]; ok {
			if state.Status == StatusCancelled {
				return state, ErrCancelled
			}
			if state.Status == StatusAgentFailed {
				return state, errors.New(state.LastError)
			}
			return e.finishDispatch(ctx, records, state, route, persist, false)
		}

		state := State{
			Key: key, Source: msg.Source, MessageID: msg.ID,
			ProductID: binding.ProductID, ProjectID: binding.ProjectID, PolicyID: binding.PolicyID,
			Kind: kind, Risk: policy.Risk, Role: route.Role, Squad: route.Squad,
			BoardStatus: "todo", Status: StatusPending, Evidence: map[EvidenceStage]Evidence{},
			GrantedGates: append([]string(nil), msg.GrantedGates...),
		}
		issueID, created, resolveErr := e.issues.Resolve(ctx, IssueRequest{
			ProjectID: binding.ProjectID, ProductID: binding.ProductID, CanonicalKey: msg.CanonicalKey,
			Title: classifiedTitle(msg.Text), Kind: kind, IdempotencyKey: key + ":issue",
		})
		if resolveErr != nil {
			delete(records, key)
			if persistErr := persist(); persistErr != nil {
				return state, errors.Join(resolveErr, persistErr)
			}
			return state, resolveErr
		}
		state.IssueID, state.IssueCreated = issueID, created
		records[key] = state
		if err := persist(); err != nil {
			return state, err
		}
		return e.finishDispatch(ctx, records, state, route, persist, false)
	})
}

func (e *Engine) Resume(ctx context.Context, key string) (State, error) {
	return e.store.withLock(func(records map[string]State, persist func() error) (State, error) {
		state, ok := records[key]
		if !ok {
			return State{}, ErrStateMissing
		}
		if state.Status == StatusCancelled {
			return state, ErrCancelled
		}
		route, err := e.revalidate(state)
		if err != nil {
			return state, err
		}
		if state.Status == StatusActive {
			return e.finishDispatch(ctx, records, state, route, persist, false)
		}
		if state.Wakeups >= e.config.MaxWakeups {
			return state, ErrWakeupLimit
		}
		state.Wakeups++
		state.Status = StatusPending
		state.LastError = ""
		records[key] = state
		if err := persist(); err != nil {
			return state, err
		}
		return e.finishDispatch(ctx, records, state, route, persist, true)
	})
}

func (e *Engine) finishDispatch(ctx context.Context, records map[string]State, state State, route Route, persist func() error, resumed bool) (State, error) {
	if state.Status != StatusActive {
		run, err := e.dispatcher.Dispatch(ctx, DispatchRequest{
			IssueID: state.IssueID, ProjectID: state.ProjectID, Role: state.Role, Squad: state.Squad,
			Kind: state.Kind, DependsOn: append([]Kind(nil), route.DependsOn...), IdempotencyKey: state.Key + ":run",
		})
		if err != nil {
			state.Status, state.LastError = StatusAgentFailed, err.Error()
			records[state.Key] = state
			if persistErr := persist(); persistErr != nil {
				return state, errors.Join(err, persistErr)
			}
			if notifyErr := e.notify(ctx, records, &state, persist); notifyErr != nil {
				return state, errors.Join(err, notifyErr)
			}
			return state, err
		}
		if run.ID == "" || run.StartedAt.IsZero() {
			state.Status, state.LastError = StatusAgentFailed, ErrRunInvalid.Error()
			records[state.Key] = state
			if persistErr := persist(); persistErr != nil {
				return state, errors.Join(ErrRunInvalid, persistErr)
			}
			if notifyErr := e.notify(ctx, records, &state, persist); notifyErr != nil {
				return state, errors.Join(ErrRunInvalid, notifyErr)
			}
			return state, ErrRunInvalid
		}
		state.Run, state.Status, state.BoardStatus = run, StatusActive, "in_progress"
		state.LastError = ""
		records[state.Key] = state
		if err := persist(); err != nil {
			return state, err
		}
	}
	if err := e.notify(ctx, records, &state, persist); err != nil {
		return state, err
	}
	_ = resumed
	return state, nil
}

func (e *Engine) notify(ctx context.Context, records map[string]State, state *State, persist func() error) error {
	if state.NotifiedStatus == state.Status || e.notifier == nil {
		return nil
	}
	err := e.notifier.Notify(ctx, Notification{
		Source: state.Source, CorrelationKey: state.Key, IssueID: state.IssueID,
		RunID: state.Run.ID, Status: state.Status, IdempotencyKey: state.Key + ":notify:" + string(state.Status),
	})
	if err != nil {
		state.LastError = err.Error()
		records[state.Key] = *state
		return errors.Join(err, persist())
	}
	state.NotifiedStatus = state.Status
	state.LastError = ""
	records[state.Key] = *state
	return persist()
}

func (e *Engine) Cancel(ctx context.Context, key string) error {
	_, err := e.store.withLock(func(records map[string]State, persist func() error) (State, error) {
		state, ok := records[key]
		if !ok {
			return State{}, ErrStateMissing
		}
		state.Status, state.BoardStatus = StatusCancelled, "cancelled"
		records[key] = state
		if err := persist(); err != nil {
			return state, err
		}
		return state, e.notify(ctx, records, &state, persist)
	})
	return err
}

func (e *Engine) RecordEvidence(_ context.Context, key string, stage EvidenceStage, sha, verdict string) (State, error) {
	if !validSHA(sha) {
		return State{}, ErrArtifactMismatch
	}
	switch stage {
	case StageReview, StageQA, StageMerge, StageActivation, StageProduction:
	default:
		return State{}, ErrEvidenceStage
	}
	if verdict != "passed" && verdict != "failed" {
		return State{}, ErrEvidenceVerdict
	}
	return e.store.withLock(func(records map[string]State, persist func() error) (State, error) {
		state, ok := records[key]
		if !ok {
			return State{}, ErrStateMissing
		}
		if state.ArtifactSHA != "" && state.ArtifactSHA != sha {
			return state, ErrArtifactMismatch
		}
		if evidence, exists := state.Evidence[stage]; exists {
			if evidence.Verdict == verdict {
				return state, nil
			}
			return state, fmt.Errorf("%w: %s evidence is immutable", ErrEvidenceOrder, stage)
		}
		previous := map[EvidenceStage]EvidenceStage{
			StageQA: StageReview, StageMerge: StageQA, StageActivation: StageMerge, StageProduction: StageActivation,
		}
		if required := previous[stage]; required != "" {
			if evidence, exists := state.Evidence[required]; !exists || evidence.Verdict != "passed" {
				return state, fmt.Errorf("%w: %s requires passed %s", ErrEvidenceOrder, stage, required)
			}
		}
		state.ArtifactSHA = sha
		if state.Evidence == nil {
			state.Evidence = map[EvidenceStage]Evidence{}
		}
		state.Evidence[stage] = Evidence{Verdict: verdict, RecordedAt: e.now().UTC()}
		records[key] = state
		return state, persist()
	})
}

func (e *Engine) plan(msg Message) (Binding, RiskPolicy, Route, Kind, error) {
	if e.config.Version != 1 || e.config.MaxWakeups < 1 {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrConfigInvalid
	}
	matches := make([]Binding, 0, 1)
	for _, binding := range e.config.Bindings {
		if binding.Source == msg.Source {
			matches = append(matches, binding)
		}
	}
	if len(matches) == 0 {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrBindingMissing
	}
	if len(matches) != 1 {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrBindingAmbiguous
	}
	binding := matches[0]
	if binding.ValidUntil.IsZero() || !binding.ValidUntil.After(e.now()) {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrBindingStale
	}
	policy, ok := e.config.Policies[binding.PolicyID]
	if !ok || binding.ProductID == "" || binding.ProjectID == "" {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrPolicyMissing
	}
	kind, ok := Classify(msg.Text)
	if !ok {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrUnclassified
	}
	route, ok := e.config.Routes[kind]
	if !ok || route.Role == "" || route.Squad == "" {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrRouteMissing
	}
	if !e.config.AllowedRoles[route.Role] {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrRoleDenied
	}
	granted := make(map[string]bool, len(msg.GrantedGates))
	for _, gate := range msg.GrantedGates {
		granted[gate] = true
	}
	for _, gate := range policy.EntryGates {
		if !granted[gate] {
			return Binding{}, RiskPolicy{}, Route{}, "", fmt.Errorf("%w: %s", ErrGateMissing, gate)
		}
	}
	if msg.ID == "" || msg.CanonicalKey == "" {
		return Binding{}, RiskPolicy{}, Route{}, "", ErrUnclassified
	}
	return binding, policy, route, kind, nil
}

func (e *Engine) revalidate(state State) (Route, error) {
	matches := make([]Binding, 0, 1)
	for _, binding := range e.config.Bindings {
		if binding.Source == state.Source {
			matches = append(matches, binding)
		}
	}
	if len(matches) == 0 {
		return Route{}, ErrBindingMissing
	}
	if len(matches) != 1 {
		return Route{}, ErrBindingAmbiguous
	}
	binding := matches[0]
	if binding.ValidUntil.IsZero() || !binding.ValidUntil.After(e.now()) {
		return Route{}, ErrBindingStale
	}
	if binding.ProductID != state.ProductID || binding.ProjectID != state.ProjectID || binding.PolicyID != state.PolicyID {
		return Route{}, ErrConfigInvalid
	}
	policy, ok := e.config.Policies[binding.PolicyID]
	if !ok || policy.Risk != state.Risk {
		return Route{}, ErrPolicyMissing
	}
	granted := make(map[string]bool, len(state.GrantedGates))
	for _, gate := range state.GrantedGates {
		granted[gate] = true
	}
	for _, gate := range policy.EntryGates {
		if !granted[gate] {
			return Route{}, fmt.Errorf("%w: %s", ErrGateMissing, gate)
		}
	}
	route, ok := e.config.Routes[state.Kind]
	if !ok || route.Role == "" || route.Squad == "" {
		return Route{}, ErrRouteMissing
	}
	if !e.config.AllowedRoles[route.Role] {
		return Route{}, ErrRoleDenied
	}
	if route.Role != state.Role || route.Squad != state.Squad {
		return Route{}, ErrConfigInvalid
	}
	return route, nil
}

func Classify(text string) (Kind, bool) {
	prefix, _, ok := strings.Cut(strings.TrimSpace(strings.ToLower(text)), ":")
	if !ok || strings.TrimSpace(classifiedTitle(text)) == "" {
		return "", false
	}
	kind := Kind(strings.TrimSpace(prefix))
	switch kind {
	case KindIdea, KindBug, KindFeature, KindIncident, KindResearch, KindProject, KindRelease:
		return kind, true
	default:
		return "", false
	}
}

func classifiedTitle(text string) string {
	_, title, ok := strings.Cut(strings.TrimSpace(text), ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(title)
}

func workflowKey(version int, source Source, messageID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("v%d\x00%s\x00%s\x00%s", version, source.Kind, source.ID, messageID)))
	return hex.EncodeToString(sum[:])
}

func validSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type FileStore struct {
	path     string
	saveHook func(map[string]State) error
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (s *FileStore) withLock(fn func(map[string]State, func() error) (State, error)) (State, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return State{}, err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return State{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return State{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	records, err := s.load()
	if err != nil {
		return State{}, err
	}
	persist := func() error {
		if s.saveHook != nil {
			return s.saveHook(records)
		}
		return s.save(records)
	}
	return fn(records, persist)
}

func (s *FileStore) load() (map[string]State, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records map[string]State
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode Tony factory state: %w", err)
	}
	return records, nil
}

func (s *FileStore) save(records map[string]State) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
