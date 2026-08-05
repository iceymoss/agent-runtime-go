package subagent

import (
	"context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type Service struct {
	store         Store
	runner        Runner
	parentWaker   ParentWaker
	workerID      string
	leaseDuration time.Duration
	clock         func() time.Time
}

func New(options Options) (*Service, error) {
	if options.Store == nil || options.Runner == nil || options.ParentWaker == nil {
		return nil, fmt.Errorf("%w: store, runner, and parent waker are required", ErrInvalidRequest)
	}
	if options.WorkerID == "" || options.LeaseDuration <= 0 {
		return nil, fmt.Errorf("%w: worker ID and positive lease duration are required", ErrInvalidRequest)
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Service{
		store:         options.Store,
		runner:        options.Runner,
		parentWaker:   options.ParentWaker,
		workerID:      options.WorkerID,
		leaseDuration: options.LeaseDuration,
		clock:         options.Clock,
	}, nil
}

func (s *Service) Spawn(ctx context.Context, request SpawnRequest) (SpawnReceipt, error) {
	returnReceipt, _, err := s.store.Spawn(ctx, request, s.clock())
	return returnReceipt, err
}

func (s *Service) Get(ctx context.Context, tenantKey agent.TenantKey, relationshipKey RelationshipKey) (Snapshot, error) {
	return s.store.Get(ctx, tenantKey, relationshipKey)
}

// RunNext claims and executes at most one child. It starts no background work.
func (s *Service) RunNext(ctx context.Context, tenantKey agent.TenantKey) (Snapshot, bool, error) {
	now := s.clock()
	snapshot, ok, err := s.store.ClaimNext(ctx, ClaimRequest{
		TenantKey: tenantKey,
		Owner:     s.workerID,
		Now:       now,
		Lease:     s.leaseDuration,
	})
	if err != nil || !ok {
		return Snapshot{}, ok, err
	}
	result, runErr := s.runner.Run(ctx, RunRequest{
		Child:       snapshot.Receipt.Child,
		AgentKey:    snapshot.AgentKey,
		Input:       cloneBytes(snapshot.Input),
		ContextRefs: cloneContextRefs(snapshot.ContextRefs),
		Limits:      snapshot.Receipt.EffectiveLimit,
		Deadline:    snapshot.Receipt.EffectiveLimit.Deadline,
	})
	if runErr != nil {
		result = RunResult{State: ChildSuspended, Failure: &Failure{Code: "runner_error", Message: runErr.Error(), Retryable: true}}
	}
	if result.State == ChildSuspended {
		committed, suspendErr := s.store.CommitSuspended(ctx, SuspendCommand{
			TenantKey:       snapshot.Receipt.Child.TenantKey,
			RelationshipKey: snapshot.Receipt.Child.RelationshipKey,
			ExpectedVersion: snapshot.Version,
			Owner:           s.workerID,
			Failure:         cloneFailure(result.Failure),
			SuspendedAt:     s.clock(),
		})
		if suspendErr != nil {
			return Snapshot{}, true, suspendErr
		}
		return committed, true, nil
	}
	committed, err := s.store.CommitTerminal(ctx, TerminalCommand{
		TenantKey:       snapshot.Receipt.Child.TenantKey,
		RelationshipKey: snapshot.Receipt.Child.RelationshipKey,
		ExpectedVersion: snapshot.Version,
		Owner:           s.workerID,
		Result:          result,
		CompletedAt:     s.clock(),
	})
	if err != nil {
		return Snapshot{}, true, err
	}
	return committed, true, nil
}

func (s *Service) Cancel(ctx context.Context, request CancelRequest) (CancelResult, error) {
	return s.store.RequestCancel(ctx, request, s.clock())
}

func (s *Service) SettleUsage(ctx context.Context, command SettleCommand) (BudgetSnapshot, bool, error) {
	return s.store.SettleUsage(ctx, command)
}

// Reconcile recovers expired/suspended work and retries committed wake intents.
func (s *Service) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileReport, error) {
	if request.Now.IsZero() {
		request.Now = s.clock()
	}
	recovered, err := s.store.Recover(ctx, request)
	if err != nil {
		return ReconcileReport{}, err
	}
	report := ReconcileReport{LeasesRecovered: recovered}
	for report.WakesDelivered+report.WakeFailures < request.Limit || request.Limit <= 0 {
		claim, ok, claimErr := s.store.ClaimWake(ctx, request.TenantKey)
		if claimErr != nil {
			return report, claimErr
		}
		if !ok {
			break
		}
		if wakeErr := s.parentWaker.Wake(ctx, claim.Request); wakeErr != nil {
			report.WakeFailures++
			break
		}
		if completeErr := s.store.CompleteWake(ctx, request.TenantKey, claim.Request.WakeKey, claim.Version, s.clock()); completeErr != nil {
			return report, completeErr
		}
		report.WakesDelivered++
	}
	return report, nil
}
