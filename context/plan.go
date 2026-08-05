package context

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/iceymoss/agent-runtime-go"
)

type PlanRef struct {
	TenantKey              agent.TenantKey `json:"tenant_key"`
	PlanKey                string          `json:"plan_key"`
	PlanDigest             string          `json:"plan_digest"`
	SchemaVersion          uint16          `json:"schema_version"`
	SessionKey             string          `json:"session_key"`
	SessionRevision        uint64          `json:"session_revision"`
	BranchKey              string          `json:"branch_key,omitempty"`
	BranchVersion          uint64          `json:"branch_version,omitempty"`
	RuntimeDigest          string          `json:"runtime_digest"`
	InputDigest            string          `json:"input_digest"`
	ProtectedFactSetDigest string          `json:"protected_fact_set_digest"`
	Budget                 Budget          `json:"budget"`
	ExecutionPolicyDigest  string          `json:"execution_policy_digest,omitempty"`
}

type planWire struct {
	SchemaVersion          uint16          `json:"schema_version"`
	Source                 SourceRef       `json:"source"`
	RuntimeDigest          string          `json:"runtime_digest"`
	InputDigest            string          `json:"input_digest"`
	ProtectedFactSetDigest string          `json:"protected_fact_set_digest"`
	Budget                 Budget          `json:"budget"`
	Pivot                  *PivotRef       `json:"pivot,omitempty"`
	Artifacts              []ArtifactRef   `json:"artifacts,omitempty"`
	Messages               []agent.Message `json:"messages"`
	Estimate               Estimate        `json:"estimate"`
	ProjectionVersion      string          `json:"projection_version"`
	TokenizerID            string          `json:"tokenizer_id"`
	Diagnostics            []Diagnostic    `json:"diagnostics,omitempty"`
	ExecutionPolicy        string          `json:"execution_policy,omitempty"`
	ExecutionPolicyDigest  string          `json:"execution_policy_digest,omitempty"`
}

// Plan is immutable to callers. Every accessor returns a value or deep copy.
type Plan struct {
	ref  PlanRef
	wire planWire
}

func (p Plan) Ref() PlanRef                   { return p.ref }
func (p Plan) Source() SourceRef              { return p.wire.Source }
func (p Plan) Messages() []agent.Message      { return cloneMessages(p.wire.Messages) }
func (p Plan) Artifacts() []ArtifactRef       { return cloneArtifacts(p.wire.Artifacts) }
func (p Plan) Estimate() Estimate             { return p.wire.Estimate }
func (p Plan) Diagnostics() []Diagnostic      { return cloneDiagnostics(p.wire.Diagnostics) }
func (p Plan) ProjectionVersion() string      { return p.wire.ProjectionVersion }
func (p Plan) TokenizerID() string            { return p.wire.TokenizerID }
func (p Plan) ProtectedFactSetDigest() string { return p.wire.ProtectedFactSetDigest }
func (p Plan) ExecutionPolicy() (string, string) {
	return p.wire.ExecutionPolicy, p.wire.ExecutionPolicyDigest
}

func (p Plan) Pivot() *PivotRef {
	if p.wire.Pivot == nil {
		return nil
	}
	value := *p.wire.Pivot
	return &value
}

// MarshalWire returns the canonical durable payload. It contains values only.
func (p Plan) MarshalWire() ([]byte, error) {
	if err := validatePlan(p); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Ref  PlanRef  `json:"ref"`
		Wire planWire `json:"wire"`
	}{Ref: p.ref, Wire: clonePlan(p).wire})
}

func UnmarshalPlan(data []byte) (Plan, error) {
	var envelope struct {
		Ref  PlanRef  `json:"ref"`
		Wire planWire `json:"wire"`
	}
	if err := unmarshalStrict(data, &envelope); err != nil {
		return Plan{}, contextError(CodeInvalidRequest, "unmarshal_plan", "", ErrInvalidRequest, err)
	}
	plan := Plan{ref: envelope.Ref, wire: envelope.Wire}
	if err := validatePlan(plan); err != nil {
		return Plan{}, err
	}
	return clonePlan(plan), nil
}

type DefaultPlanner struct {
	counter TokenCounter
	store   PlanStore
}

func NewPlanner(counter TokenCounter, store PlanStore) (*DefaultPlanner, error) {
	if counter == nil || strings.TrimSpace(counter.ID()) == "" || store == nil {
		return nil, contextError(CodeInvalidRequest, "new_planner", "", ErrInvalidRequest, fmt.Errorf("token counter and plan store are required"))
	}
	return &DefaultPlanner{counter: counter, store: store}, nil
}

func (p *DefaultPlanner) Prepare(ctx stdcontext.Context, request PrepareRequest) (Plan, error) {
	if ctx == nil || p == nil || p.counter == nil || p.store == nil {
		return Plan{}, contextError(CodeInvalidRequest, "prepare", "", ErrInvalidRequest, fmt.Errorf("planner is not initialized"))
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	if err := validatePrepareRequest(request); err != nil {
		return Plan{}, err
	}
	if request.Runtime.TokenizerID != p.counter.ID() {
		return Plan{}, contextError(CodeInvalidRequest, "prepare", "", ErrInvalidRequest, fmt.Errorf("runtime tokenizer and token counter identities differ"))
	}
	factSet, err := NewFactSet(request.ProtectedFacts)
	if err != nil {
		return Plan{}, err
	}
	if request.Pivot != nil && request.Pivot.FactSetDigest != factSet.Digest {
		return Plan{}, contextError(CodePivotInvalid, "prepare", request.Pivot.Artifact.Key, ErrPivotInvalid, fmt.Errorf("pivot protected fact set does not match exact source revision"))
	}
	factMessages, err := projectFacts(factSet)
	if err != nil {
		return Plan{}, err
	}
	factTokens, err := p.counter.CountTokens(ctx, cloneMessages(factMessages))
	if err != nil {
		return Plan{}, contextError(CodeInvalidRequest, "count_protected_facts", "", ErrInvalidRequest, err)
	}
	if factTokens < 0 {
		return Plan{}, contextError(CodeInvalidRequest, "count_protected_facts", "", ErrInvalidRequest, fmt.Errorf("negative token count"))
	}
	if factTokens+request.Budget.ToolSchemaTokens+request.Budget.MediaTokens > request.Budget.InputLimit() {
		return Plan{}, contextError(CodeProtectedFactsTooLarge, "prepare", request.Source.SessionKey, ErrProtectedFactsTooLarge, nil)
	}

	messages := make([]agent.Message, 0)
	messages = append(messages, cloneMessages(request.Runtime.SystemMessages)...)
	messages = append(messages, cloneMessages(request.Runtime.CapabilityMessages)...)
	messages = append(messages, cloneMessages(request.SummaryMessages)...)
	messages = append(messages, factMessages...)
	messages = append(messages, cloneMessages(request.MainlineMessages)...)
	messages = append(messages, cloneMessages(request.BranchMessages)...)
	messages = append(messages, cloneMessages(request.InvocationMessages)...)
	normalized, err := NormalizeHistory(NormalizeRequest{Messages: messages, Policy: RepairReject})
	if err != nil {
		return Plan{}, err
	}

	messageTokens, err := p.counter.CountTokens(ctx, cloneMessages(normalized.Messages))
	if err != nil {
		return Plan{}, contextError(CodeInvalidRequest, "count_tokens", "", ErrInvalidRequest, err)
	}
	if messageTokens < 0 {
		return Plan{}, contextError(CodeInvalidRequest, "count_tokens", "", ErrInvalidRequest, fmt.Errorf("negative token count"))
	}
	estimate := Estimate{
		MessageTokens: messageTokens, ToolSchemaTokens: request.Budget.ToolSchemaTokens,
		MediaTokens: request.Budget.MediaTokens, CounterID: p.counter.ID(),
	}
	estimate.TotalInputTokens = estimate.MessageTokens + estimate.ToolSchemaTokens + estimate.MediaTokens
	if estimate.TotalInputTokens > request.Budget.InputLimit() {
		compactable := append(cloneMessages(request.MainlineMessages), request.BranchMessages...)
		if normalizedHistory, normalizeErr := NormalizeHistory(NormalizeRequest{Messages: compactable}); normalizeErr == nil && selectSafePrefix(normalizedHistory.Messages) > 0 {
			return Plan{}, contextError(CodeCompactionRequired, "prepare", request.Source.SessionKey, ErrCompactionRequired, nil)
		}
		return Plan{}, contextError(CodeContextTooLarge, "prepare", request.Source.SessionKey, ErrContextTooLarge, nil)
	}

	artifacts := append(cloneArtifacts(request.Runtime.Artifacts), request.Artifacts...)
	wire := planWire{
		SchemaVersion: CurrentSchemaVersion, Source: request.Source,
		RuntimeDigest: request.Runtime.DefinitionDigest, InputDigest: normalized.Digest,
		ProtectedFactSetDigest: factSet.Digest, Budget: request.Budget,
		Pivot: clonePivot(request.Pivot), Artifacts: cloneArtifacts(artifacts), Messages: cloneMessages(normalized.Messages),
		Estimate: estimate, ProjectionVersion: request.Runtime.ProjectionVersion,
		TokenizerID: request.Runtime.TokenizerID, Diagnostics: cloneDiagnostics(normalized.Diagnostics),
		ExecutionPolicy: request.Runtime.ExecutionPolicy, ExecutionPolicyDigest: request.Runtime.ExecutionPolicyDigest,
	}
	digest, err := agent.CanonicalDigest(wire)
	if err != nil {
		return Plan{}, contextError(CodeInvalidRequest, "digest_plan", "", ErrInvalidRequest, err)
	}
	ref := PlanRef{
		TenantKey: request.Source.TenantKey, PlanKey: "ctx_" + digest[:32], PlanDigest: digest,
		SchemaVersion: CurrentSchemaVersion, SessionKey: request.Source.SessionKey,
		SessionRevision: request.Source.SessionRevision, BranchKey: request.Source.BranchKey,
		BranchVersion: request.Source.BranchVersion, RuntimeDigest: request.Runtime.DefinitionDigest,
		InputDigest: normalized.Digest, ProtectedFactSetDigest: factSet.Digest, Budget: request.Budget,
		ExecutionPolicyDigest: request.Runtime.ExecutionPolicyDigest,
	}
	plan := Plan{ref: ref, wire: wire}
	if request.Expected != nil && *request.Expected != ref {
		return Plan{}, contextError(CodePlanDrift, "prepare", ref.PlanKey, ErrPlanDrift, nil)
	}
	return p.store.CreateOrVerify(ctx, plan)
}

func NewFactSet(facts []ProtectedFact) (FactSet, error) {
	canonical := cloneFacts(facts)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Key != canonical[j].Key {
			return canonical[i].Key < canonical[j].Key
		}
		return canonical[i].SourceRevision < canonical[j].SourceRevision
	})
	for i, fact := range canonical {
		if fact.Key == "" || fact.Kind == "" || fact.Value == "" || fact.SourceKey == "" || fact.SourceRevision == 0 || fact.PolicyVersion == "" {
			return FactSet{}, contextError(CodeInvalidRequest, "fact_set", fact.Key, ErrInvalidRequest, fmt.Errorf("protected fact is incomplete"))
		}
		if i > 0 && canonical[i-1].Key == fact.Key {
			return FactSet{}, contextError(CodeProtectedFactConflict, "fact_set", fact.Key, ErrProtectedFactConflict, nil)
		}
	}
	digest, err := agent.CanonicalDigest(canonical)
	if err != nil {
		return FactSet{}, contextError(CodeInvalidRequest, "fact_set", "", ErrInvalidRequest, err)
	}
	return FactSet{Facts: canonical, Digest: digest}, nil
}

func projectFacts(set FactSet) ([]agent.Message, error) {
	if len(set.Facts) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(struct {
		Schema string          `json:"schema"`
		Digest string          `json:"digest"`
		Facts  []ProtectedFact `json:"facts"`
	}{Schema: "protected-facts/v1", Digest: set.Digest, Facts: set.Facts})
	if err != nil {
		return nil, contextError(CodeInvalidRequest, "project_facts", "", ErrInvalidRequest, err)
	}
	return []agent.Message{agent.NewSystemMessage(string(data))}, nil
}

func validatePrepareRequest(request PrepareRequest) error {
	if !request.Source.TenantKey.Valid() || request.Source.SessionKey == "" || request.Source.SessionRevision == 0 || request.Runtime.DefinitionDigest == "" || request.Runtime.ProjectionVersion == "" || request.Runtime.TokenizerID == "" {
		return contextError(CodeInvalidRequest, "prepare", "", ErrInvalidRequest, fmt.Errorf("exact source and runtime identity are required"))
	}
	if request.Source.BranchKey == "" && request.Source.BranchVersion != 0 || request.Source.BranchKey != "" && request.Source.BranchVersion == 0 {
		return contextError(CodeInvalidRequest, "prepare", "", ErrInvalidRequest, fmt.Errorf("branch key and version must be specified together"))
	}
	if request.Budget.ContextTokens <= 0 || request.Budget.ReservedOutputTokens < 0 || request.Budget.SafetyMarginTokens < 0 || request.Budget.ToolSchemaTokens < 0 || request.Budget.MediaTokens < 0 || request.Budget.InputLimit() <= 0 {
		return contextError(CodeInvalidRequest, "prepare", "", ErrInvalidRequest, fmt.Errorf("invalid token budget"))
	}
	if request.Pivot != nil {
		pivot := request.Pivot
		if pivot.Artifact.Key == "" || pivot.Artifact.Digest == "" || pivot.CoveredThrough == 0 || pivot.SourceDigest == "" || pivot.FactSetDigest == "" {
			return contextError(CodePivotInvalid, "prepare", pivot.Artifact.Key, ErrPivotInvalid, nil)
		}
	}
	for _, ref := range append(cloneArtifacts(request.Runtime.Artifacts), request.Artifacts...) {
		if ref.Key == "" || ref.Kind == "" || ref.Schema == "" || ref.Digest == "" || ref.Generation == "" {
			return contextError(CodeInvalidRequest, "prepare", ref.Key, ErrInvalidRequest, fmt.Errorf("artifact reference is incomplete"))
		}
	}
	return nil
}

func validatePlan(plan Plan) error {
	if plan.ref.SchemaVersion != CurrentSchemaVersion || plan.wire.SchemaVersion != CurrentSchemaVersion || !plan.ref.TenantKey.Valid() || plan.ref.PlanKey == "" || plan.ref.PlanDigest == "" {
		return contextError(CodeInvalidRequest, "validate_plan", plan.ref.PlanKey, ErrInvalidRequest, fmt.Errorf("incomplete plan identity"))
	}
	digest, err := agent.CanonicalDigest(plan.wire)
	if err != nil || digest != plan.ref.PlanDigest || plan.ref.PlanKey != "ctx_"+digest[:32] {
		return contextError(CodePlanDrift, "validate_plan", plan.ref.PlanKey, ErrPlanDrift, err)
	}
	if plan.ref.TenantKey != plan.wire.Source.TenantKey || plan.ref.SessionKey != plan.wire.Source.SessionKey || plan.ref.SessionRevision != plan.wire.Source.SessionRevision || plan.ref.BranchKey != plan.wire.Source.BranchKey || plan.ref.BranchVersion != plan.wire.Source.BranchVersion || plan.ref.RuntimeDigest != plan.wire.RuntimeDigest || plan.ref.InputDigest != plan.wire.InputDigest || plan.ref.ProtectedFactSetDigest != plan.wire.ProtectedFactSetDigest || plan.ref.Budget != plan.wire.Budget || plan.ref.ExecutionPolicyDigest != plan.wire.ExecutionPolicyDigest {
		return contextError(CodePlanDrift, "validate_plan", plan.ref.PlanKey, ErrPlanDrift, nil)
	}
	return nil
}

func clonePlan(plan Plan) Plan {
	plan.wire.Messages = cloneMessages(plan.wire.Messages)
	plan.wire.Artifacts = cloneArtifacts(plan.wire.Artifacts)
	plan.wire.Diagnostics = cloneDiagnostics(plan.wire.Diagnostics)
	plan.wire.Pivot = clonePivot(plan.wire.Pivot)
	return plan
}

func clonePivot(value *PivotRef) *PivotRef {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}
