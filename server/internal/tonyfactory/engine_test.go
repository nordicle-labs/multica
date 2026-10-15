package tonyfactory

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeIssues struct {
	mu      sync.Mutex
	byKey   map[string]string
	creates int
}

func (f *fakeIssues) Resolve(_ context.Context, req IssueRequest) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := req.ProjectID + ":" + req.CanonicalKey
	if id := f.byKey[key]; id != "" {
		return id, false, nil
	}
	f.creates++
	id := "ISSUE-" + key
	f.byKey[key] = id
	return id, true, nil
}

type fakeDispatcher struct {
	mu    sync.Mutex
	runs  map[string]Run
	calls int
	fail  bool
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req DispatchRequest) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return Run{}, errors.New("agent unavailable")
	}
	if run, ok := f.runs[req.IdempotencyKey]; ok {
		return run, nil
	}
	run := Run{ID: "RUN-" + req.IdempotencyKey[:8], StartedAt: time.Unix(1_700_000_000, 0).UTC()}
	f.runs[req.IdempotencyKey] = run
	return run, nil
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []Notification
	byKey map[string]bool
}

func (f *fakeNotifier) Notify(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.byKey[n.IdempotencyKey] {
		f.calls = append(f.calls, n)
		f.byKey[n.IdempotencyKey] = true
	}
	return nil
}

func testConfig() Config {
	future := time.Unix(2_000_000_000, 0).UTC()
	return Config{
		Version:    1,
		MaxWakeups: 3,
		Bindings: []Binding{
			{Source: Source{Kind: SourceTony, ID: "central"}, ProductID: "operating-system", ProjectID: "project-os", PolicyID: "high", ValidUntil: future},
			{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ProductID: "product-a", ProjectID: "project-a", PolicyID: "normal", ValidUntil: future},
			{Source: Source{Kind: SourceTelegram, ID: "bot-product-b"}, ProductID: "product-b", ProjectID: "project-b", PolicyID: "normal", ValidUntil: future},
		},
		Policies: map[string]RiskPolicy{
			"high":   {Risk: RiskHigh, EntryGates: []string{"product-go"}},
			"normal": {Risk: RiskMedium},
		},
		Routes: map[Kind]Route{
			KindFeature: {Role: "Engineering Developer", Squad: "Engineering"},
			KindBug:     {Role: "Engineering Developer", Squad: "Engineering"},
			KindRelease: {Role: "Release", Squad: "Engineering"},
		},
		AllowedRoles: map[string]bool{"Engineering Developer": true, "Release": true},
	}
}

func newHarness(t *testing.T) (Config, string, *fakeIssues, *fakeDispatcher, *fakeNotifier) {
	t.Helper()
	return testConfig(), filepath.Join(t.TempDir(), "state.json"),
		&fakeIssues{byKey: map[string]string{}},
		&fakeDispatcher{runs: map[string]Run{}},
		&fakeNotifier{byKey: map[string]bool{}}
}

func TestCanaryTonyCreatesAndResumesAfterRestart(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	engine := New(cfg, NewFileStore(path), issues, dispatch, notify)
	msg := Message{Source: Source{Kind: SourceTony, ID: "central"}, ID: "tony-1", CanonicalKey: "feature-auth", Text: "feature: add passkeys", GrantedGates: []string{"product-go"}}

	first, err := engine.Process(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(cfg, NewFileStore(path), issues, dispatch, notify)
	second, err := restarted.Process(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if first.IssueID != second.IssueID || first.ProjectID != "project-os" || first.Squad != "Engineering" || first.Run.ID == "" || first.Run.StartedAt.IsZero() {
		t.Fatalf("bad Tony canary state: %#v / %#v", first, second)
	}
	if dispatch.calls != 1 || issues.creates != 1 || len(notify.calls) != 1 || notify.calls[0].CorrelationKey != first.Key {
		t.Fatalf("Tony canary was not idempotent: dispatch=%d create=%d notifications=%#v", dispatch.calls, issues.creates, notify.calls)
	}
}

func TestCanaryProductChannelHasNoCrossTalk(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	engine := New(cfg, NewFileStore(path), issues, dispatch, notify)

	a, err := engine.Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "tg-1", CanonicalKey: "bug-login", Text: "bug: login fails"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-b"}, ID: "tg-1", CanonicalKey: "bug-login", Text: "bug: login fails"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ProjectID != "project-a" || b.ProjectID != "project-b" || a.ProductID == b.ProductID || a.Key == b.Key || a.IssueID == b.IssueID {
		t.Fatalf("channel cross-talk: a=%#v b=%#v", a, b)
	}
}

func TestFailClosedBindingsClassificationPermissionsAndGate(t *testing.T) {
	base := testConfig()
	cases := []struct {
		name string
		cfg  Config
		msg  Message
		want error
	}{
		{"unknown channel", base, Message{Source: Source{Kind: SourceTelegram, ID: "missing"}, ID: "1", CanonicalKey: "a", Text: "bug: x"}, ErrBindingMissing},
		{"unclassified", base, Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "1", CanonicalKey: "a", Text: "please do something"}, ErrUnclassified},
		{"missing human gate", base, Message{Source: Source{Kind: SourceTony, ID: "central"}, ID: "1", CanonicalKey: "a", Text: "feature: x"}, ErrGateMissing},
	}
	ambiguous := testConfig()
	ambiguous.Bindings = append(ambiguous.Bindings, ambiguous.Bindings[1])
	cases = append(cases, struct {
		name string
		cfg  Config
		msg  Message
		want error
	}{"ambiguous binding", ambiguous, Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "1", CanonicalKey: "a", Text: "bug: x"}, ErrBindingAmbiguous})
	stale := testConfig()
	stale.Bindings[1].ValidUntil = time.Unix(1, 0)
	cases = append(cases, struct {
		name string
		cfg  Config
		msg  Message
		want error
	}{"stale binding", stale, Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "1", CanonicalKey: "a", Text: "bug: x"}, ErrBindingStale})
	denied := testConfig()
	denied.AllowedRoles["Engineering Developer"] = false
	cases = append(cases, struct {
		name string
		cfg  Config
		msg  Message
		want error
	}{"role denied", denied, Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "1", CanonicalKey: "a", Text: "bug: x"}, ErrRoleDenied})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, path, issues, dispatch, notify := newHarness(t)
			_, err := New(tc.cfg, NewFileStore(path), issues, dispatch, notify).Process(context.Background(), tc.msg)
			if !errors.Is(err, tc.want) || dispatch.calls != 0 || issues.creates != 0 {
				t.Fatalf("err=%v dispatch=%d creates=%d", err, dispatch.calls, issues.creates)
			}
		})
	}
}

func TestExistingIssueActiveRunConcurrencyFailureRetryAndCancel(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	issues.byKey["project-a:known"] = "ISSUE-existing"
	engine := New(cfg, NewFileStore(path), issues, dispatch, notify)
	msg := Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "same", CanonicalKey: "known", Text: "bug: x"}

	var wg sync.WaitGroup
	states := make(chan State, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := engine.Process(context.Background(), msg)
			if err != nil {
				t.Errorf("concurrent process: %v", err)
			}
			states <- state
		}()
	}
	wg.Wait()
	close(states)
	for state := range states {
		if state.IssueID != "ISSUE-existing" || state.Run.ID == "" {
			t.Fatalf("did not resume canonical issue: %#v", state)
		}
	}
	if issues.creates != 0 || dispatch.calls != 1 {
		t.Fatalf("duplicate work: creates=%d dispatch=%d", issues.creates, dispatch.calls)
	}

	failMsg := Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "failure", CanonicalKey: "failure", Text: "bug: x"}
	dispatch.fail = true
	failed, err := engine.Process(context.Background(), failMsg)
	if err == nil || failed.Status != StatusAgentFailed {
		t.Fatalf("agent failure did not close: state=%#v err=%v", failed, err)
	}
	dispatch.fail = false
	recovered, err := engine.Resume(context.Background(), failed.Key)
	if err != nil || recovered.Run.ID == "" || recovered.Wakeups != 1 {
		t.Fatalf("retry failed: state=%#v err=%v", recovered, err)
	}
	if err := engine.Cancel(context.Background(), recovered.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Resume(context.Background(), recovered.Key); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancelled workflow resumed: %v", err)
	}
	if got := []Status{notify.calls[len(notify.calls)-3].Status, notify.calls[len(notify.calls)-2].Status, notify.calls[len(notify.calls)-1].Status}; got[0] != StatusAgentFailed || got[1] != StatusActive || got[2] != StatusCancelled {
		t.Fatalf("notification transitions = %v", got)
	}
}

func TestWakeupsAreBounded(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	cfg.MaxWakeups = 2
	cfg.Routes[KindIncident] = Route{Role: "Engineering Developer", Squad: "Engineering"}
	dispatch.fail = true
	engine := New(cfg, NewFileStore(path), issues, dispatch, notify)
	state, err := engine.Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "bounded", CanonicalKey: "bounded", Text: "incident: outage"})
	if err == nil || state.Status != StatusAgentFailed {
		t.Fatalf("initial failure = %#v, %v", state, err)
	}
	for i := 0; i < cfg.MaxWakeups; i++ {
		state, err = engine.Resume(context.Background(), state.Key)
		if err == nil || state.Wakeups != i+1 {
			t.Fatalf("retry %d = %#v, %v", i, state, err)
		}
	}
	if _, err := engine.Resume(context.Background(), state.Key); !errors.Is(err, ErrWakeupLimit) {
		t.Fatalf("unbounded retry accepted: %v", err)
	}
}

func TestArtifactEvidenceUsesOneExactSHA(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	engine := New(cfg, NewFileStore(path), issues, dispatch, notify)
	state, err := engine.Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "sha", CanonicalKey: "sha", Text: "feature: x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []EvidenceStage{StageReview, StageQA, StageMerge, StageActivation, StageProduction} {
		if _, err := engine.RecordEvidence(context.Background(), state.Key, stage, "0123456789abcdef0123456789abcdef01234567", "passed"); err != nil {
			t.Fatalf("stage %s: %v", stage, err)
		}
	}
	if _, err := engine.RecordEvidence(context.Background(), state.Key, StageQA, "ffffffffffffffffffffffffffffffffffffffff", "passed"); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("mismatched artifact accepted: %v", err)
	}
}

func TestResumeRevalidatesBindingAndRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   error
	}{
		{"revoked role", func(cfg *Config) { cfg.AllowedRoles["Engineering Developer"] = false }, ErrRoleDenied},
		{"stale binding", func(cfg *Config) { cfg.Bindings[1].ValidUntil = time.Unix(1, 0) }, ErrBindingStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, path, issues, dispatch, notify := newHarness(t)
			dispatch.fail = true
			engine := New(cfg, NewFileStore(path), issues, dispatch, notify)
			state, err := engine.Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: tc.name, CanonicalKey: tc.name, Text: "bug: x"})
			if err == nil {
				t.Fatal("initial dispatch unexpectedly succeeded")
			}
			dispatch.fail = false
			tc.mutate(&engine.config)
			if _, err := engine.Resume(context.Background(), state.Key); !errors.Is(err, tc.want) {
				t.Fatalf("Resume error = %v, want %v", err, tc.want)
			}
			if dispatch.calls != 1 {
				t.Fatalf("dispatch calls = %d, want 1", dispatch.calls)
			}
		})
	}
}

func TestEvidenceIsMonotoneAndVerdictsAreBounded(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	engine := New(cfg, NewFileStore(path), issues, dispatch, notify)
	state, err := engine.Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "evidence", CanonicalKey: "evidence", Text: "feature: x"})
	if err != nil {
		t.Fatal(err)
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	if _, err := engine.RecordEvidence(context.Background(), state.Key, StageReview, sha, "passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RecordEvidence(context.Background(), state.Key, StageQA, sha, "passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RecordEvidence(context.Background(), state.Key, StageReview, sha, "failed"); !errors.Is(err, ErrEvidenceOrder) {
		t.Fatalf("rewrote review evidence: %v", err)
	}
	if _, err := engine.RecordEvidence(context.Background(), state.Key, StageMerge, sha, "approved"); !errors.Is(err, ErrEvidenceVerdict) {
		t.Fatalf("accepted unknown verdict: %v", err)
	}
}

func TestDispatchFailureReturnsPersistenceFailure(t *testing.T) {
	cfg, path, issues, dispatch, notify := newHarness(t)
	store := NewFileStore(path)
	dispatch.fail = true
	persistErr := errors.New("disk unavailable")
	saves := 0
	store.saveHook = func(records map[string]State) error {
		saves++
		if saves == 2 {
			return persistErr
		}
		return store.save(records)
	}
	state, err := New(cfg, store, issues, dispatch, notify).Process(context.Background(), Message{Source: Source{Kind: SourceTelegram, ID: "bot-product-a"}, ID: "persist", CanonicalKey: "persist", Text: "bug: x"})
	if !errors.Is(err, persistErr) || state.Status != StatusAgentFailed {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}
