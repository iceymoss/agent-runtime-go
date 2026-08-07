package icoder

import (
	"context"
	"strings"
	"time"
)

type ApprovalDecision string

const (
	ApprovalApproveOnce ApprovalDecision = "approve_once"
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

type runContext struct {
	id      string
	session string
	approve ApprovalFunc
	record  func(context.Context, string, string, any) error
}

func withRunContext(ctx context.Context, id, session string, approve ApprovalFunc, record func(context.Context, string, string, any) error) context.Context {
	return context.WithValue(ctx, runContextKey{}, runContext{id: id, session: session, approve: approve, record: record})
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
