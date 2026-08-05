// Package context prepares immutable, revision-bound provider context plans.
// It owns context artifact and pivot semantics, but deliberately owns no session
// or message storage.
package context

import (
	stdcontext "context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

const CurrentSchemaVersion uint16 = 1

type DiagnosticCode string
type RepairPolicy string

const (
	RepairReject       RepairPolicy = "reject"
	RepairFromTerminal RepairPolicy = "from_terminal"

	DiagnosticEmptyAssistant    DiagnosticCode = "empty_assistant_removed"
	DiagnosticMalformedInput    DiagnosticCode = "malformed_tool_input"
	DiagnosticResultNameFilled  DiagnosticCode = "tool_result_name_filled"
	DiagnosticTerminalRepaired  DiagnosticCode = "terminal_tool_repaired"
	DiagnosticConservativeCount DiagnosticCode = "conservative_token_count"
)

type TerminalFact struct {
	CallID         string `json:"call_id"`
	Reason         string `json:"reason"`
	SourceRevision uint64 `json:"source_revision"`
	EffectUnknown  bool   `json:"effect_unknown"`
}

type Diagnostic struct {
	Code         DiagnosticCode `json:"code"`
	MessageIndex int            `json:"message_index"`
	PartIndex    int            `json:"part_index"`
	CallID       string         `json:"call_id,omitempty"`
}

type ToolExchange struct {
	Start       int      `json:"start"`
	End         int      `json:"end"`
	CallIDs     []string `json:"call_ids"`
	SourceStart int      `json:"source_start"`
	SourceEnd   int      `json:"source_end"`
}

type NormalizeRequest struct {
	Messages      []agent.Message `json:"messages"`
	Policy        RepairPolicy    `json:"policy"`
	TerminalFacts []TerminalFact  `json:"terminal_facts,omitempty"`
}

type NormalizeResult struct {
	Messages    []agent.Message `json:"messages"`
	Exchanges   []ToolExchange  `json:"exchanges,omitempty"`
	Diagnostics []Diagnostic    `json:"diagnostics,omitempty"`
	Digest      string          `json:"digest"`
}

type SourceRef struct {
	TenantKey       agent.TenantKey `json:"tenant_key"`
	SessionKey      string          `json:"session_key"`
	SessionRevision uint64          `json:"session_revision"`
	BranchKey       string          `json:"branch_key,omitempty"`
	BranchVersion   uint64          `json:"branch_version,omitempty"`
}

type ArtifactRef struct {
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Schema     string `json:"schema"`
	Digest     string `json:"digest"`
	Generation string `json:"generation"`
}

// PivotRef is a revision-bound wire value. Persisting or advancing it belongs
// to the session owner; this package never tracks a mutable current pivot.
type PivotRef struct {
	Artifact       ArtifactRef `json:"artifact"`
	CoveredThrough uint64      `json:"covered_through"`
	SourceDigest   string      `json:"source_digest"`
	FactSetDigest  string      `json:"protected_fact_set_digest"`
}

type ProtectedFact struct {
	Key            string `json:"key"`
	Kind           string `json:"kind"`
	Value          string `json:"value"`
	SourceKey      string `json:"source_key"`
	SourceRevision uint64 `json:"source_revision"`
	PolicyVersion  string `json:"policy_version"`
}

type FactSet struct {
	Facts  []ProtectedFact `json:"facts"`
	Digest string          `json:"digest"`
}

type Budget struct {
	ContextTokens        int `json:"context_tokens"`
	ReservedOutputTokens int `json:"reserved_output_tokens"`
	SafetyMarginTokens   int `json:"safety_margin_tokens"`
	ToolSchemaTokens     int `json:"tool_schema_tokens"`
	MediaTokens          int `json:"media_tokens"`
}

func (b Budget) InputLimit() int {
	return b.ContextTokens - b.ReservedOutputTokens - b.SafetyMarginTokens
}

type Estimate struct {
	MessageTokens    int    `json:"message_tokens"`
	ToolSchemaTokens int    `json:"tool_schema_tokens"`
	MediaTokens      int    `json:"media_tokens"`
	TotalInputTokens int    `json:"total_input_tokens"`
	CounterID        string `json:"counter_id"`
}

type RuntimeArtifacts struct {
	DefinitionDigest      string          `json:"definition_digest"`
	ProjectionVersion     string          `json:"projection_version"`
	TokenizerID           string          `json:"tokenizer_id"`
	SystemMessages        []agent.Message `json:"system_messages,omitempty"`
	CapabilityMessages    []agent.Message `json:"capability_messages,omitempty"`
	Artifacts             []ArtifactRef   `json:"artifacts,omitempty"`
	ExecutionPolicy       string          `json:"execution_policy,omitempty"`
	ExecutionPolicyDigest string          `json:"execution_policy_digest,omitempty"`
}

type PrepareRequest struct {
	Source             SourceRef        `json:"source"`
	Runtime            RuntimeArtifacts `json:"runtime"`
	Pivot              *PivotRef        `json:"pivot,omitempty"`
	SummaryMessages    []agent.Message  `json:"summary_messages,omitempty"`
	ProtectedFacts     []ProtectedFact  `json:"protected_facts,omitempty"`
	MainlineMessages   []agent.Message  `json:"mainline_messages,omitempty"`
	BranchMessages     []agent.Message  `json:"branch_messages,omitempty"`
	InvocationMessages []agent.Message  `json:"invocation_messages,omitempty"`
	Artifacts          []ArtifactRef    `json:"artifacts,omitempty"`
	Budget             Budget           `json:"budget"`
	Expected           *PlanRef         `json:"expected,omitempty"`
}

// TokenCounter estimates the complete canonical message projection. ID must
// identify the provider/model/tokenizer implementation and its version.
type TokenCounter interface {
	ID() string
	CountTokens(stdcontext.Context, []agent.Message) (int, error)
}

type Planner interface {
	Prepare(stdcontext.Context, PrepareRequest) (Plan, error)
}

type PlanStore interface {
	CreateOrVerify(stdcontext.Context, Plan) (Plan, error)
	Get(stdcontext.Context, PlanRef) (Plan, error)
}

type SummaryRequest struct {
	Source         SourceRef       `json:"source"`
	Messages       []agent.Message `json:"messages"`
	ProtectedFacts []ProtectedFact `json:"protected_facts"`
	Predecessor    *ArtifactRef    `json:"predecessor,omitempty"`
	RuntimeDigest  string          `json:"runtime_digest"`
	SourceDigest   string          `json:"source_digest"`
}

type SummaryResult struct {
	Messages       []agent.Message `json:"messages"`
	UnresolvedWork []string        `json:"unresolved_work,omitempty"`
	Generation     string          `json:"generation"`
}

type Summarizer interface {
	Summarize(stdcontext.Context, SummaryRequest) (SummaryResult, error)
}

type ArtifactStore interface {
	CreateOrVerify(stdcontext.Context, SummaryArtifact) (SummaryArtifact, error)
	Get(stdcontext.Context, agent.TenantKey, ArtifactRef) (SummaryArtifact, error)
}

type Clock func() time.Time

type CompactionOptions struct {
	Summarizer Summarizer
	Artifacts  ArtifactStore
	Counter    TokenCounter
	Clock      Clock
	MinSavings int
}

type CompactRequest struct {
	Source         SourceRef
	Messages       []agent.Message
	ProtectedFacts []ProtectedFact
	Predecessor    *ArtifactRef
	RuntimeDigest  string
	TargetTokens   int
	CoveredThrough uint64
}

type CompactResult struct {
	Artifact SummaryArtifact
	Pivot    PivotRef
	Kept     []agent.Message
}
