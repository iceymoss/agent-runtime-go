package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type referenceService struct {
	policy Policy
	store  Store
	clock  Clock
	fences FenceValidator
}

type ServiceOptions struct {
	Policy Policy
	Store  Store
	Clock  Clock
	Fences FenceValidator
}

func NewService(options ServiceOptions) (Service, error) {
	if options.Policy == nil || options.Store == nil {
		return nil, permissionError(ErrInvalidRequest, "new service", "", "policy and store are required")
	}
	if options.Clock == nil {
		options.Clock = realClock{}
	}
	return &referenceService{policy: options.Policy, store: options.Store, clock: options.Clock, fences: options.Fences}, nil
}

func (s *referenceService) Check(ctx context.Context, request CheckRequest) (CheckResult, error) {
	if !validCheck(request) {
		return CheckResult{}, permissionError(ErrInvalidRequest, "check", request.RequestKey, "missing immutable binding")
	}
	if request.PolicyVersion != s.policy.Version() {
		return CheckResult{}, permissionError(ErrPolicyUnavailable, "check", request.RequestKey, "exact policy version unavailable")
	}
	if err := s.validateFence(ctx, request); err != nil {
		return CheckResult{}, err
	}
	grants, err := s.store.FindGrants(ctx, GrantQuery{TenantKey: request.Subject.TenantKey, Check: request})
	if err != nil {
		return CheckResult{}, err
	}
	result, err := s.policy.Evaluate(ctx, request, grants)
	if err != nil {
		return CheckResult{}, err
	}
	if result.PolicyVersion == "" {
		result.PolicyVersion = s.policy.Version()
	}
	if result.InputDigest == "" {
		result.InputDigest = request.InputDigest
	}
	if result.PolicyVersion != request.PolicyVersion || result.InputDigest != request.InputDigest {
		return CheckResult{}, permissionError(ErrPolicyInvalid, "check", request.RequestKey, "evaluation changed immutable binding")
	}
	switch result.Decision {
	case DecisionAllow, DecisionDeny:
		return result, nil
	case DecisionAsk:
		token, tokenErr := newResumeToken()
		if tokenErr != nil {
			return CheckResult{}, tokenErr
		}
		expires := request.ApprovalExpiresAt
		approval := ApprovalRequest{RequestKey: request.RequestKey, Check: request, ExpiresAt: expires}
		snapshot, _, createErr := s.store.CreateAndSuspend(ctx, CreateApprovalCommand{Request: approval, ResumeToken: token})
		if createErr != nil {
			return CheckResult{}, createErr
		}
		result.Approval = &snapshot
		result.Blocker = &SuspensionBlocker{RequestRef: snapshot.Request.RequestKey, ResumeToken: snapshot.ResumeToken, Revision: snapshot.Request.Revision}
		return result, nil
	default:
		return CheckResult{}, permissionError(ErrPolicyInvalid, "check", request.RequestKey, "unknown decision")
	}
}

func (s *referenceService) Resolve(ctx context.Context, command ResolveCommand) (Snapshot, bool, error) {
	return s.store.Resolve(ctx, command)
}
func (s *referenceService) Cancel(ctx context.Context, command CancelCommand) (Snapshot, bool, error) {
	snapshot, err := s.store.GetRequest(ctx, GetRequestQuery{TenantKey: command.TenantKey, RequestKey: command.RequestKey})
	if err != nil {
		return Snapshot{}, false, err
	}
	check := snapshot.Request.Check
	check.AttemptRef, check.FenceToken = command.AttemptRef, command.FenceToken
	if err := s.validateFence(ctx, check); err != nil {
		return Snapshot{}, false, err
	}
	return s.store.Cancel(ctx, command)
}
func (s *referenceService) GetRequest(ctx context.Context, q GetRequestQuery) (Snapshot, error) {
	return s.store.GetRequest(ctx, q)
}
func (s *referenceService) ListPending(ctx context.Context, q ListPendingQuery) ([]Snapshot, error) {
	return s.store.ListPending(ctx, q)
}
func (s *referenceService) ConsumeGrant(ctx context.Context, c ConsumeGrantCommand) (Grant, error) {
	if err := s.validateFence(ctx, c.Check); err != nil {
		return Grant{}, err
	}
	return s.store.ConsumeGrant(ctx, c)
}
func (s *referenceService) RevokeGrant(ctx context.Context, c RevokeGrantCommand) (Grant, error) {
	return s.store.RevokeGrant(ctx, c)
}
func (s *referenceService) ExpireDue(ctx context.Context, c ExpireCommand) (ExpireResult, error) {
	return s.store.ExpireDue(ctx, c)
}

func (s *referenceService) Revalidate(ctx context.Context, command RevalidateCommand) (CheckResult, error) {
	snapshot, err := s.store.GetRequest(ctx, GetRequestQuery{TenantKey: command.TenantKey, RequestKey: command.RequestKey})
	if err != nil {
		return CheckResult{}, err
	}
	if snapshot.ResumeToken == "" || snapshot.ResumeToken != command.ResumeToken {
		return CheckResult{}, permissionError(ErrResumeRevalidation, "revalidate", command.RequestKey, "invalid resume token")
	}
	check := snapshot.Request.Check
	if command.InputDigest != check.InputDigest || command.PolicyVersion != check.PolicyVersion || command.ToolGeneration != check.ToolGeneration {
		return CheckResult{}, permissionError(ErrResumeRevalidation, "revalidate", command.RequestKey, "immutable binding changed")
	}
	if snapshot.Request.State == ApprovalDenied {
		return CheckResult{}, ErrPermissionDenied
	}
	if snapshot.Request.State == ApprovalExpired {
		return CheckResult{}, ErrRequestExpired
	}
	if snapshot.Request.State == ApprovalCanceled {
		return CheckResult{}, ErrRequestCanceled
	}
	if snapshot.Request.State != ApprovalApproved {
		return CheckResult{}, ErrApprovalRequired
	}
	check.AttemptRef, check.FenceToken = command.AttemptRef, command.FenceToken
	if err := s.validateFence(ctx, check); err != nil {
		return CheckResult{}, err
	}
	if command.PolicyVersion != s.policy.Version() {
		return CheckResult{}, ErrPolicyUnavailable
	}
	grants, err := s.store.FindGrants(ctx, GrantQuery{TenantKey: command.TenantKey, Check: check})
	if err != nil {
		return CheckResult{}, err
	}
	result, err := s.policy.Evaluate(ctx, check, grants)
	if err != nil {
		return CheckResult{}, err
	}
	if result.PolicyVersion == "" {
		result.PolicyVersion = s.policy.Version()
	}
	if result.InputDigest == "" {
		result.InputDigest = check.InputDigest
	}
	if result.PolicyVersion != check.PolicyVersion || result.InputDigest != check.InputDigest {
		return CheckResult{}, permissionError(ErrResumeRevalidation, "revalidate", command.RequestKey, "current policy does not allow exact effect")
	}
	if result.Decision == DecisionDeny {
		return CheckResult{}, permissionError(ErrPermissionDenied, "revalidate", command.RequestKey, result.ReasonCode)
	}
	if result.Decision != DecisionAllow && result.Decision != DecisionAsk {
		return CheckResult{}, permissionError(ErrPolicyInvalid, "revalidate", command.RequestKey, "unknown decision")
	}
	// The unchanged exact approval satisfies the same policy's ask outcome.
	result.Decision = DecisionAllow
	return result, nil
}

func (s *referenceService) validateFence(ctx context.Context, request CheckRequest) error {
	if s.fences == nil {
		return nil
	}
	if err := s.fences.ValidateFence(ctx, request.Subject.TenantKey, request.RunRef, request.AttemptRef, request.FenceToken); err != nil {
		if errors.Is(err, ErrStaleFence) {
			return err
		}
		return permissionError(ErrStaleFence, "validate fence", request.RequestKey, "attempt is no longer current")
	}
	return nil
}

func newResumeToken() (ResumeToken, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", permissionError(ErrInvalidRequest, "resume token", "", "random source unavailable")
	}
	return ResumeToken(hex.EncodeToString(value[:])), nil
}
