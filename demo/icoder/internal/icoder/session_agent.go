package icoder

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
	"github.com/iceymoss/agent-runtime-go/session"
)

// queueLease bounds how long a worker may hold a queued run before another may
// take it over. It matches the durable run lease, because the two protect the
// same attempt from two different layers and a shorter queue lease would let a
// second worker claim work the durable store still considers owned.
const queueLease = runLeaseDuration

// queueLimits are iCoder's admission limits. They are deliberately small: a
// single workspace has one set of files, so letting several runs edit it
// concurrently would produce interleaved changes nobody asked for. The queue is
// here to serialize and survive restarts, not to parallelize.
var queueLimits = session.Limits{
	MaxActiveGlobal: 1, MaxActivePerTenant: 1, MaxActivePerSession: 1,
	MaxQueuedPerTenant: 64, MaxQueuedPerSession: 64,
}

// runQueue is iCoder's session.SessionAgent: the durable queue that admits a
// run, leases it to a worker, executes exactly one attempt for it, and merges
// the result into the session.
//
// The interactive path does not use it - a person typing at a prompt wants their
// own turn, not a place in a queue - but the daemon does, and it is what makes
// background work survive a restart with its position, its context plan, and its
// cancellation intact.
type runQueue struct {
	host  *session.SessionAgent
	store *SQLiteSessionRunStore
}

// newRunQueue assembles the queue over the application's own runtime generation,
// attempt execution, and event stream.
func newRunQueue(application *App, workerID string) (*runQueue, error) {
	store := NewSQLiteSessionRunStore(application.store.db)
	host, err := session.New(session.Options{
		Store:       store,
		Definitions: &queueDefinitions{app: application},
		Attempts:    &queueAttempts{app: application, store: store},
		Events:      &queueEvents{app: application},
		Limits:      queueLimits,
		WorkerID:    workerID,
		// Cleanup runs on a context that outlives cancellation, so a worker that
		// is being shut down still records what happened to its run.
		LeaseDuration:  queueLease,
		CleanupTimeout: 5 * time.Second,
		Clock:          func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, err
	}
	return &runQueue{host: host, store: store}, nil
}

// queueDefinitions resolves the runtime generation a queued run executes under.
//
// A queued run records the definition digest it was admitted with, and the
// worker refuses to execute it under a different one. That is the point of
// resolving by digest rather than by name: a binary that has since changed its
// prompt or tool set must not silently answer a request admitted against the old
// composition.
type queueDefinitions struct{ app *App }

func (d *queueDefinitions) Resolve(_ context.Context, _ session.DefinitionRequest) (session.ResolvedExecution, error) {
	return d.execution(d.app.resolved.DefinitionDigest, d.app.resolved.Definition, d.app.resolved.SchemaVersion), nil
}

func (d *queueDefinitions) ResolveGeneration(ctx context.Context, _ agent.TenantKey, definitionDigest string) (session.ResolvedExecution, error) {
	if definitionDigest == d.app.resolved.DefinitionDigest {
		return d.execution(definitionDigest, d.app.resolved.Definition, d.app.resolved.SchemaVersion), nil
	}
	resolved, err := d.app.resolver.ResolveGeneration(ctx, provider.Scope{TenantKey: tenantKey}, definitionDigest)
	if err != nil {
		return session.ResolvedExecution{}, fmt.Errorf("%w: %v", session.ErrGenerationUnavailable, err)
	}
	return d.execution(resolved.DefinitionDigest, resolved.Definition, resolved.SchemaVersion), nil
}

func (d *queueDefinitions) execution(digestValue string, definition *agent.RuntimeDefinition, schemaVersion uint16) session.ResolvedExecution {
	artifacts := make([]session.ArtifactRef, 0, len(d.app.resolved.Artifacts))
	for _, ref := range d.app.resolved.Artifacts {
		artifacts = append(artifacts, session.ArtifactRef{
			Kind: ref.Kind, Key: ref.Key, Generation: ref.Generation,
			Digest: ref.Digest, SchemaVersion: ref.SchemaVersion,
		})
	}
	return session.ResolvedExecution{
		Definition: definition, DefinitionDigest: digestValue,
		SchemaVersion: schemaVersion, Artifacts: artifacts,
	}
}

// queueAttempts executes one attempt for a claimed run.
//
// It is context-aware in the sense the queue needs: the messages it runs are the
// ones the run was admitted with, so a run that waited in the queue answers the
// question it was asked against the history that existed when it was asked, not
// against whatever the session looks like when a worker finally gets to it.
type queueAttempts struct {
	app   *App
	store *SQLiteSessionRunStore
}

func (r *queueAttempts) Resume(ctx context.Context, request session.AttemptRequest) (session.AttemptResult, error) {
	stored, err := r.store.Get(ctx, request.RunKey)
	if err != nil {
		return session.AttemptResult{}, err
	}
	instruction, ok := lastUserInstruction(request.Input)
	if !ok {
		return session.AttemptResult{}, fmt.Errorf("queued run %q has no user instruction", request.RunKey)
	}
	sessionID := string(stored.Receipt.SessionKey)
	snapshot, _, err := r.app.store.Load(ctx, sessionID)
	if err != nil {
		return session.AttemptResult{}, err
	}
	fence := new(atomic.Uint64)
	r.app.runMu.Lock()
	result, runErr := r.app.executeAttempt(ctx, runInvocation{
		sessionID: sessionID, snapshot: snapshot,
		requestID: string(request.RunKey), runKey: string(request.RunKey),
		instruction: instruction, messages: request.Input,
		observeFence: func(token uint64) { fence.Store(token) },
	})
	r.app.runMu.Unlock()
	attempt := session.AttemptResult{Fence: fence.Load(), Result: result}
	if result != nil {
		attempt.Outcome = result.Outcome
	}
	if runErr != nil {
		attempt.Failure = &session.Failure{
			Code: "attempt_failed", Message: runErr.Error(),
			// A cancelled attempt is retryable; a run that genuinely failed is not,
			// because retrying it would repeat whatever made it fail.
			Retryable: errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded),
			Cause:     runErr,
		}
		return attempt, runErr
	}
	if suspension := toolSuspension(result); suspension != nil {
		attempt.Failure = &session.Failure{
			Code: "tool_suspended", Message: string(suspension.Kind), Retryable: true,
		}
	}
	return attempt, nil
}

// Interrupt cancels the in-flight attempt for a run. The queue owns what happens
// next; this only stops the work.
func (r *queueAttempts) Interrupt(_ context.Context, runKey session.RunKey, _ session.CancelMode) error {
	r.app.runs.interrupt(string(runKey))
	return nil
}

// queueEvents publishes the fact that a queued run reached a new state, after
// the transaction that made it true has committed.
type queueEvents struct{ app *App }

func (e *queueEvents) PublishAfterCommit(ctx context.Context, runKey session.RunKey) error {
	result, err := e.app.queue.store.Get(ctx, runKey)
	if err != nil {
		return err
	}
	return e.app.store.AppendRunEvent(ctx, string(result.Receipt.SessionKey),
		string(runKey)+":queue:"+string(result.Receipt.ExecutionState), "agent.run.queued", map[string]any{
			"run_key": string(runKey), "admission": result.Receipt.AdmissionState,
			"execution": result.Receipt.ExecutionState, "session_revision": result.SessionRevision,
		})
}

// QueueRun admits an instruction as durable background work and returns the run
// key a caller can await, cancel, or resume later.
//
// The context plan is bound here rather than in the worker, so a run that waits
// answers against the history it was submitted with.
func (a *App) QueueRun(ctx context.Context, instruction string) (session.RunKey, error) {
	if a.queue == nil {
		return "", fmt.Errorf("icoder is not running a durable run queue")
	}
	a.runMu.Lock()
	invocation, err := a.prepareInvocation(ctx, instruction)
	a.runMu.Unlock()
	if err != nil {
		return "", err
	}
	base := invocation.snapshot.Revision
	receipt, err := a.queue.host.Run(ctx, session.RunRequest{
		TenantKey: tenantKey, SessionKey: session.SessionKey(invocation.sessionID),
		RequestID: invocation.requestID, AgentKey: recipeKey,
		Messages: invocation.messages, BaseRevision: &base,
		Merge: session.MergeFastForward,
	})
	if err != nil {
		return "", err
	}
	return receipt.RunKey, nil
}

// AwaitQueuedRun blocks until a queued run reaches a state it will not leave on
// its own, and reports what happened to it.
func (a *App) AwaitQueuedRun(ctx context.Context, runKey session.RunKey) (session.RunResult, error) {
	if a.queue == nil {
		return session.RunResult{}, fmt.Errorf("icoder is not running a durable run queue")
	}
	return a.queue.host.Await(ctx, runKey)
}

// RunQueuedWork executes at most one queued run on the calling goroutine and
// reports whether there was any. It is what a one-shot command uses instead of
// starting a worker it would immediately have to stop.
func (a *App) RunQueuedWork(ctx context.Context) (bool, error) {
	if a.queue == nil {
		return false, nil
	}
	return a.queue.host.RunNext(ctx)
}

// QueuedRun reports a queued run's current state without waiting for it.
func (a *App) QueuedRun(ctx context.Context, runKey session.RunKey) (session.RunResult, error) {
	if a.queue == nil {
		return session.RunResult{}, fmt.Errorf("icoder is not running a durable run queue")
	}
	return a.queue.host.Get(ctx, runKey)
}

// CancelQueuedRun asks the queue to stop a run. Suspending leaves it resumable;
// abandoning does not.
func (a *App) CancelQueuedRun(ctx context.Context, runKey session.RunKey, mode session.CancelMode, reason string) (session.CancelResult, error) {
	if a.queue == nil {
		return session.CancelResult{}, fmt.Errorf("icoder is not running a durable run queue")
	}
	return a.queue.host.Cancel(ctx, session.CancelRequest{
		TenantKey: tenantKey, RunKey: runKey, Mode: mode, Reason: reason,
	})
}

// ResumeQueuedRun returns a retryably suspended queued run to the queue.
func (a *App) ResumeQueuedRun(ctx context.Context, runKey session.RunKey) (session.ResumeResult, error) {
	if a.queue == nil {
		return session.ResumeResult{}, fmt.Errorf("icoder is not running a durable run queue")
	}
	return a.queue.host.Resume(ctx, session.ResumeRequest{TenantKey: tenantKey, RunKey: runKey})
}
