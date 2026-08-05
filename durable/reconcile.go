package durable

import (
	"context"
	"errors"
	"time"
)

type RunRef struct{ RunKey RunKey }
type CheckpointReason string
type RevokeReason string

type CheckpointReport struct {
	Inspected int
	Remaining []RunRef
	Err       error
}

type RevokeReport struct {
	Inspected int
	Revoked   int
	Remaining []RunRef
	Err       error
}

type BlockerKind string

const (
	BlockerApproval BlockerKind = "approval"
	BlockerChild    BlockerKind = "child"
	BlockerArtifact BlockerKind = "artifact"
	BlockerOperator BlockerKind = "operator"
)

type Blocker struct {
	Kind   BlockerKind
	Key    string
	Digest string
	Active bool
}

type BlockerReader interface {
	GetBlocker(context.Context, RunRef) (Blocker, bool, error)
}

type WorkItem struct {
	RunKey RunKey
	Phase  Phase
}

type WorkSink interface {
	Enqueue(context.Context, WorkItem) error
}

type Classification string

const (
	ClassificationTerminal         Classification = "terminal"
	ClassificationActive           Classification = "active"
	ClassificationEligible         Classification = "eligible"
	ClassificationBlocked          Classification = "blocked"
	ClassificationOperatorRequired Classification = "operator_required"
	ClassificationCorrupt          Classification = "corrupt"
)

type ReconcileRequest struct {
	Now    time.Time
	Cursor RunKey
	Limit  int
}

type ReconcileReport struct {
	Scanned         int
	Revoked         int
	UnknownEffects  int
	Enqueued        int
	Next            RunKey
	Classifications map[Classification]int
	Errors          map[string]int
}

type Reconciler struct {
	store    Store
	effects  ExecutionLedger
	blockers BlockerReader
	sink     WorkSink
	maxBatch int
}

type ReconcilerOptions struct {
	Store    Store
	Effects  ExecutionLedger
	Blockers BlockerReader
	Work     WorkSink
	MaxBatch int
}

func NewReconciler(options ReconcilerOptions) (*Reconciler, error) {
	if options.Store == nil || options.Effects == nil || options.MaxBatch <= 0 {
		return nil, durableError(ErrInvalidTransition, "new reconciler", "", "store, effects, and positive max batch are required")
	}
	return &Reconciler{store: options.Store, effects: options.Effects, blockers: options.Blockers, sink: options.Work, maxBatch: options.MaxBatch}, nil
}

// ReconcileBatch synchronously classifies at most request.Limit runs. Expired
// leases are fenced before work is enqueued. Any running tool effect is marked
// unknown by the store's atomic revoke and is never automatically replayed.
func (r *Reconciler) ReconcileBatch(ctx context.Context, request ReconcileRequest) (ReconcileReport, error) {
	report := ReconcileReport{Classifications: make(map[Classification]int), Errors: make(map[string]int)}
	if request.Now.IsZero() || request.Limit <= 0 || request.Limit > r.maxBatch {
		return report, durableError(ErrLimitExceeded, "reconcile", "", "invalid batch limit")
	}
	page, err := r.store.Scan(ctx, ScanRequest{Cursor: request.Cursor, Limit: request.Limit})
	if err != nil {
		return report, err
	}
	report.Next = page.Next
	for _, snapshot := range page.Snapshots {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(ErrReconciliationIncomplete, err)
		}
		report.Scanned++
		if err := ValidateSnapshot(snapshot); err != nil {
			report.Classifications[ClassificationCorrupt]++
			report.Errors["snapshot_incoherent"]++
			continue
		}
		if snapshot.Terminal() {
			report.Classifications[ClassificationTerminal]++
			continue
		}
		if snapshot.Status == StatusRunning && snapshot.LeaseUntil.After(request.Now) {
			report.Classifications[ClassificationActive]++
			continue
		}
		if snapshot.Status == StatusRunning {
			before, listErr := r.effects.ListEffects(ctx, RunKey(snapshot.Identity.RunKey))
			if listErr != nil {
				report.Classifications[ClassificationCorrupt]++
				report.Errors["effect_load"]++
				continue
			}
			unknown := 0
			for _, effect := range before {
				if effect.Status == EffectRunning {
					unknown++
				}
			}
			revoked, revokeErr := r.store.RevokeLease(ctx, RevokeLeaseRequest{
				RunKey: RunKey(snapshot.Identity.RunKey), ExpectedRevision: snapshot.Revision,
				ExpectedFence: snapshot.FenceToken, Now: request.Now, RequireExpired: true,
			})
			if revokeErr != nil {
				if errors.Is(revokeErr, ErrLeaseLost) || errors.Is(revokeErr, ErrLeaseHeld) {
					report.Classifications[ClassificationActive]++
					continue
				}
				report.Classifications[ClassificationCorrupt]++
				report.Errors["revoke"]++
				continue
			}
			snapshot = revoked
			report.Revoked++
			report.UnknownEffects += unknown
			if unknown > 0 {
				report.Classifications[ClassificationOperatorRequired]++
				continue
			}
		}
		effects, effectsErr := r.effects.ListEffects(ctx, RunKey(snapshot.Identity.RunKey))
		if effectsErr != nil {
			report.Classifications[ClassificationCorrupt]++
			report.Errors["effect_load"]++
			continue
		}
		operatorRequired := false
		for _, effect := range effects {
			if effect.Status == EffectUnknown {
				operatorRequired = true
				break
			}
		}
		if operatorRequired {
			report.Classifications[ClassificationOperatorRequired]++
			continue
		}
		blocked, blockerErr := r.blocked(ctx, snapshot)
		if blockerErr != nil {
			report.Classifications[ClassificationCorrupt]++
			report.Errors["blocker"]++
			continue
		}
		if blocked {
			report.Classifications[ClassificationBlocked]++
			continue
		}
		if r.sink != nil {
			if err := r.sink.Enqueue(ctx, WorkItem{RunKey: RunKey(snapshot.Identity.RunKey), Phase: snapshot.Phase}); err != nil {
				report.Classifications[ClassificationCorrupt]++
				report.Errors["enqueue"]++
				continue
			}
			report.Enqueued++
		}
		report.Classifications[ClassificationEligible]++
	}
	if report.Classifications[ClassificationCorrupt] > 0 {
		return report, ErrReconciliationIncomplete
	}
	return report, nil
}

func (r *Reconciler) blocked(ctx context.Context, snapshot Snapshot) (bool, error) {
	if snapshot.Status != StatusSuspended || r.blockers == nil {
		return false, nil
	}
	blocker, found, err := r.blockers.GetBlocker(ctx, RunRef{RunKey: RunKey(snapshot.Identity.RunKey)})
	if err != nil {
		return false, err
	}
	return found && blocker.Active, nil
}

func (r *Reconciler) Checkpoint(ctx context.Context, refs []RunRef, _ CheckpointReason) CheckpointReport {
	report := CheckpointReport{}
	if len(refs) > r.maxBatch {
		report.Remaining = append([]RunRef(nil), refs...)
		report.Err = ErrLimitExceeded
		return report
	}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			report.Remaining = append(report.Remaining, refs[i:]...)
			report.Err = err
			break
		}
		snapshot, err := r.store.Load(ctx, ref.RunKey)
		report.Inspected++
		if err != nil {
			report.Remaining = append(report.Remaining, ref)
			report.Err = errors.Join(report.Err, err)
			continue
		}
		if !snapshot.Terminal() {
			report.Remaining = append(report.Remaining, ref)
		}
	}
	return report
}

func (r *Reconciler) Revoke(ctx context.Context, refs []RunRef, _ RevokeReason, now time.Time) RevokeReport {
	report := RevokeReport{}
	if len(refs) > r.maxBatch {
		report.Remaining = append([]RunRef(nil), refs...)
		report.Err = ErrLimitExceeded
		return report
	}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			report.Remaining = append(report.Remaining, refs[i:]...)
			report.Err = err
			break
		}
		snapshot, err := r.store.Load(ctx, ref.RunKey)
		report.Inspected++
		if err != nil || snapshot.Terminal() || snapshot.Status != StatusRunning {
			if err != nil {
				report.Err = errors.Join(report.Err, err)
				report.Remaining = append(report.Remaining, ref)
			}
			continue
		}
		_, err = r.store.RevokeLease(ctx, RevokeLeaseRequest{RunKey: ref.RunKey, ExpectedRevision: snapshot.Revision, ExpectedFence: snapshot.FenceToken, Now: now})
		if err != nil {
			report.Remaining = append(report.Remaining, ref)
			report.Err = errors.Join(report.Err, err)
			continue
		}
		report.Revoked++
	}
	return report
}
