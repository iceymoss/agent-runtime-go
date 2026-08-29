package icoder

import (
	"context"
	"strings"
	"sync/atomic"
	"time"
)

type ApprovalDecision string

const (
	ApprovalApproveOnce ApprovalDecision = "approve_once"
	ApprovalApproveAuto ApprovalDecision = "approve_auto"
	ApprovalDeny        ApprovalDecision = "deny"
)

type ApprovalPrompt struct {
	ToolName  string
	Action    string
	Resource  string
	Input     string
	ExpiresAt time.Time
}

type ApprovalFunc func(context.Context, ApprovalPrompt) (ApprovalDecision, error)

type runContextKey struct{}

// runContext carries the identities a tool invocation needs but the runtime does
// not pass through ToolInvocation: which durable run and attempt it belongs to,
// which session owns it, who can answer an approval, and where facts are written.
//
// id is the durable run key and is stable across attempts, so a resumed run
// keeps its recovery identity. attempt changes on every acquisition, which is
// what makes effect records attributable to one worker.
type runContext struct {
	id      string
	attempt string
	fence   *atomic.Uint64
	session string
	approve ApprovalFunc
	record  func(context.Context, string, string, any) error
}

// fenceToken reports the lease fence of the attempt that owns this context. It
// is zero until the durable store grants the lease, which is before any tool can
// run, so a tool always observes the real fence.
func (r runContext) fenceToken() uint64 {
	if r.fence == nil {
		return 0
	}
	return r.fence.Load()
}

func withRunContext(ctx context.Context, value runContext) context.Context {
	return context.WithValue(ctx, runContextKey{}, value)
}

func currentRunContext(ctx context.Context) runContext {
	value, _ := ctx.Value(runContextKey{}).(runContext)
	return value
}

func approvalInput(raw string) string {
	const limit = 4000
	raw = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, raw)
	if len(raw) > limit {
		return raw[:limit] + "\n... truncated"
	}
	return raw
}
