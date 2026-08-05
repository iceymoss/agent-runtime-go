package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

type hostModel struct{}

func (hostModel) Name() string                     { return "model" }
func (hostModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (hostModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	return nil, nil
}

type hostResolver struct {
	definition *agent.RuntimeDefinition
}

func (r hostResolver) Resolve(context.Context, session.DefinitionRequest) (session.ResolvedExecution, error) {
	return session.ResolvedExecution{Definition: r.definition, DefinitionDigest: "definition-v1"}, nil
}

func (r hostResolver) ResolveGeneration(_ context.Context, _ agent.TenantKey, digest string) (session.ResolvedExecution, error) {
	if digest != "definition-v1" {
		return session.ResolvedExecution{}, session.ErrGenerationUnavailable
	}
	return session.ResolvedExecution{Definition: r.definition, DefinitionDigest: digest}, nil
}

type hostRunner struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

type sequentialHostRunner struct {
	mu    sync.Mutex
	calls int
}

type retryableHostRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *retryableHostRunner) Resume(_ context.Context, request session.AttemptRequest) (session.AttemptResult, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 1 {
		failure := &session.Failure{Code: "provider_retryable", Message: "provider unavailable", Retryable: true}
		return session.AttemptResult{Fence: 1, Outcome: agent.OutcomeSuspended, Failure: failure}, nil
	}
	result := &agent.RunResult{Text: string(request.RunKey), Outcome: agent.OutcomeCompleted}
	return session.AttemptResult{Fence: uint64(call), Outcome: result.Outcome, Result: result}, nil
}

func (*retryableHostRunner) Interrupt(context.Context, session.RunKey, session.CancelMode) error {
	return nil
}

func (r *sequentialHostRunner) Resume(_ context.Context, request session.AttemptRequest) (session.AttemptResult, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == 1 {
		return session.AttemptResult{}, errors.New("first run failed")
	}
	result := &agent.RunResult{Text: string(request.RunKey), Outcome: agent.OutcomeCompleted}
	return session.AttemptResult{Fence: uint64(call), Outcome: result.Outcome, Result: result}, nil
}

func (*sequentialHostRunner) Interrupt(context.Context, session.RunKey, session.CancelMode) error {
	return nil
}

func (r *hostRunner) Resume(ctx context.Context, request session.AttemptRequest) (session.AttemptResult, error) {
	if r.started != nil {
		r.startOnce.Do(func() { close(r.started) })
	}
	if r.release != nil {
		select {
		case <-ctx.Done():
			return session.AttemptResult{}, ctx.Err()
		case <-r.release:
		}
	}
	return session.AttemptResult{Fence: 1, Outcome: agent.OutcomeCompleted, Result: &agent.RunResult{Text: string(request.RunKey), Outcome: agent.OutcomeCompleted}}, nil
}

func (*hostRunner) Interrupt(context.Context, session.RunKey, session.CancelMode) error { return nil }

type inspectingNotifier struct {
	store *session.MemoryRunStore
	seen  chan session.RunResult
}

func (n inspectingNotifier) PublishAfterCommit(ctx context.Context, key session.RunKey) error {
	result, err := n.store.Get(ctx, key)
	if err == nil && n.seen != nil {
		n.seen <- result
	}
	return err
}

func newHost(t *testing.T, store *session.MemoryRunStore, runner session.AttemptRunner, events session.EventSink) *session.SessionAgent {
	t.Helper()
	tools, err := agent.NewToolSet(agent.NewRegistry(), []string{})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := agent.NewRuntimeDefinition(agent.RuntimeDefinitionSpec{
		Key: "agent", Model: agent.ModelMetadata{Name: "model", Version: "model-v1"},
		Execution: agent.ExecutionSettings{MaxSteps: 4}, PromptVersion: "prompt-v1", PolicyVersion: "policy-v1",
	}, hostModel{}, tools)
	if err != nil {
		t.Fatal(err)
	}
	host, err := session.New(session.Options{
		Store: store, Definitions: hostResolver{definition: definition}, Attempts: runner, Events: events,
		Limits: runLimits, WorkerID: "worker", LeaseDuration: time.Minute, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func hostRequest(id string, merge session.MergeStrategy) session.RunRequest {
	return session.RunRequest{TenantKey: "opaque-tenant", SessionKey: "session", RequestID: id, AgentKey: "agent", Messages: []agent.Message{agent.NewUserMessage(id)}, Merge: merge}
}

func TestSessionAgentPersistsBeforeReceiptAndCallerDisconnectDoesNotCancel(t *testing.T) {
	store := session.NewMemoryRunStore()
	notifier := inspectingNotifier{store: store, seen: make(chan session.RunResult, 2)}
	host := newHost(t, store, &hostRunner{}, notifier)
	caller, cancel := context.WithCancel(context.Background())
	receipt, err := host.Run(caller, hostRequest("request", session.MergeFastForward))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case persisted := <-notifier.seen:
		if persisted.Receipt.RunKey != receipt.RunKey || persisted.Receipt.AdmissionState != session.AdmissionQueued {
			t.Fatalf("notified before durable admission: %#v", persisted)
		}
	default:
		t.Fatal("post-commit notifier was not called")
	}
	cancel()
	if worked, err := host.RunNext(context.Background()); err != nil || !worked {
		t.Fatalf("RunNext() = %v, %v", worked, err)
	}
	result, err := host.Await(context.Background(), receipt.RunKey)
	if err != nil || result.Receipt.ExecutionState != session.ExecutionReleased || result.CoreResult == nil {
		t.Fatalf("Await() = %#v, %v", result, err)
	}
}

func TestSessionAgentDuplicateConflictAwaitAndMergeNone(t *testing.T) {
	store := session.NewMemoryRunStore()
	host := newHost(t, store, &hostRunner{}, nil)
	request := hostRequest("same", session.MergeNone)
	first, err := host.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.Run(context.Background(), request)
	if err != nil || second != first {
		t.Fatalf("duplicate Run() = %#v, %v", second, err)
	}
	request.Messages = []agent.Message{agent.NewUserMessage("different")}
	if _, err := host.Run(context.Background(), request); !errors.Is(err, session.ErrRunConflict) {
		t.Fatalf("conflicting Run() error = %v", err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.Await(waitCtx, first.RunKey); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Await() error = %v", err)
	}
	if worked, err := host.RunNext(context.Background()); err != nil || !worked {
		t.Fatalf("RunNext() = %v, %v", worked, err)
	}
	result, err := host.Await(context.Background(), first.RunKey)
	if err != nil || result.Receipt.ExecutionState != session.ExecutionMergePending {
		t.Fatalf("MergeNone Await() = %#v, %v", result, err)
	}
}

func TestSessionAgentWorkerContinuesAfterPerRunError(t *testing.T) {
	store := session.NewMemoryRunStore()
	host := newHost(t, store, &sequentialHostRunner{}, nil)
	first, err := host.Run(context.Background(), hostRequest("first", session.MergeNone))
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.Run(context.Background(), hostRequest("second", session.MergeNone))
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.RunWorker(workerCtx) }()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	failed, err := host.Await(waitCtx, first.RunKey)
	if err != nil || failed.Receipt.ExecutionState != session.ExecutionFailed {
		t.Fatalf("first run = %#v, %v", failed, err)
	}
	completed, err := host.Await(waitCtx, second.RunKey)
	if err != nil || completed.Receipt.ExecutionState != session.ExecutionMergePending {
		t.Fatalf("second run = %#v, %v", completed, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker stop error = %v", err)
	}
}

func TestSessionAgentSuspendedRunRequiresExplicitResume(t *testing.T) {
	store := session.NewMemoryRunStore()
	host := newHost(t, store, &retryableHostRunner{}, nil)
	receipt, err := host.Run(context.Background(), hostRequest("retryable", session.MergeNone))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := host.RunNext(context.Background()); err != nil || !worked {
		t.Fatalf("first RunNext() = %v, %v", worked, err)
	}
	stored, err := host.Await(context.Background(), receipt.RunKey)
	if err != nil || stored.Receipt.ExecutionState != session.ExecutionSuspended {
		t.Fatalf("suspended = %#v, %v", stored, err)
	}
	if worked, err := host.RunNext(context.Background()); err != nil || worked {
		t.Fatalf("silent resume RunNext() = %v, %v", worked, err)
	}
	resumed, err := host.Resume(context.Background(), session.ResumeRequest{TenantKey: "opaque-tenant", RunKey: receipt.RunKey})
	if err != nil || !resumed.Resumed || resumed.State != session.ExecutionQueued {
		t.Fatalf("Resume() = %#v, %v", resumed, err)
	}
	if worked, err := host.RunNext(context.Background()); err != nil || !worked {
		t.Fatalf("resumed RunNext() = %v, %v", worked, err)
	}
	completed, err := host.Await(context.Background(), receipt.RunKey)
	if err != nil || completed.Receipt.ExecutionState != session.ExecutionMergePending || completed.CoreResult == nil {
		t.Fatalf("completed = %#v, %v", completed, err)
	}
}

func TestSessionAgentStopAdmissionRaceIsLinearizable(t *testing.T) {
	store := session.NewMemoryRunStore()
	host := newHost(t, store, &hostRunner{}, nil)
	start := make(chan struct{})
	results := make(chan struct {
		receipt session.RunReceipt
		err     error
	}, 32)
	for i := 0; i < cap(results); i++ {
		go func(i int) {
			<-start
			receipt, err := host.Run(context.Background(), hostRequest(time.Unix(0, int64(i)).String(), session.MergeNone))
			results <- struct {
				receipt session.RunReceipt
				err     error
			}{receipt, err}
		}(i)
	}
	close(start)
	host.StopAdmission()
	for i := 0; i < cap(results); i++ {
		outcome := <-results
		if errors.Is(outcome.err, session.ErrAdmissionClosed) || errors.Is(outcome.err, session.ErrQueueFull) {
			continue
		}
		if outcome.err != nil {
			t.Fatalf("Run() race error = %v", outcome.err)
		}
		if _, err := store.Get(context.Background(), outcome.receipt.RunKey); err != nil {
			t.Fatalf("accepted receipt was not persisted: %v", err)
		}
	}
	if _, err := host.Run(context.Background(), hostRequest("after-stop", session.MergeNone)); !errors.Is(err, session.ErrAdmissionClosed) {
		t.Fatalf("Run() after stop error = %v", err)
	}
}

func TestSessionAgentDrainDeadlineSuspendsAndShutdownIsRepeatable(t *testing.T) {
	store := session.NewMemoryRunStore()
	runner := &hostRunner{started: make(chan struct{}), release: make(chan struct{})}
	host := newHost(t, store, runner, nil)
	receipt, err := host.Run(context.Background(), hostRequest("drain", session.MergeFastForward))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := host.Drain(ctx)
	if !errors.Is(err, session.ErrDrainDeadline) || result.Suspended != 1 || result.Remaining != 0 {
		t.Fatalf("Drain() = %#v, %v", result, err)
	}
	stored, getErr := store.Get(context.Background(), receipt.RunKey)
	if getErr != nil || stored.Receipt.ExecutionState != session.ExecutionSuspended {
		t.Fatalf("deadline did not leave resumable work: %#v, %v", stored, getErr)
	}
	shutdown, err := host.Shutdown(context.Background())
	if err != nil || shutdown.Suspended != 1 {
		t.Fatalf("Shutdown() = %#v, %v", shutdown, err)
	}
	again, err := host.Shutdown(context.Background())
	if err != nil || again != shutdown {
		t.Fatalf("repeated Shutdown() = %#v, %v", again, err)
	}
}
