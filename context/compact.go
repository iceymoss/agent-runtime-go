package context

import (
	stdcontext "context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	projectjson "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

type summaryArtifactWire struct {
	SchemaVersion  uint16          `json:"schema_version"`
	Source         SourceRef       `json:"source"`
	CoveredThrough uint64          `json:"covered_through"`
	SourceDigest   string          `json:"source_digest"`
	RuntimeDigest  string          `json:"runtime_digest"`
	FactSet        FactSet         `json:"protected_fact_set"`
	Messages       []agent.Message `json:"messages"`
	UnresolvedWork []string        `json:"unresolved_work,omitempty"`
	Predecessor    *ArtifactRef    `json:"predecessor,omitempty"`
	Generation     string          `json:"generation"`
}

// SummaryArtifact is immutable. Accessors return deep copies.
type SummaryArtifact struct {
	ref            ArtifactRef
	source         SourceRef
	coveredThrough uint64
	sourceDigest   string
	runtimeDigest  string
	factSet        FactSet
	messages       []agent.Message
	unresolvedWork []string
	predecessor    *ArtifactRef
	createdAt      time.Time
}

func (a SummaryArtifact) Ref() ArtifactRef          { return a.ref }
func (a SummaryArtifact) Source() SourceRef         { return a.source }
func (a SummaryArtifact) CoveredThrough() uint64    { return a.coveredThrough }
func (a SummaryArtifact) SourceDigest() string      { return a.sourceDigest }
func (a SummaryArtifact) RuntimeDigest() string     { return a.runtimeDigest }
func (a SummaryArtifact) ProtectedFactSet() FactSet { return cloneFactSet(a.factSet) }
func (a SummaryArtifact) Messages() []agent.Message { return cloneMessages(a.messages) }
func (a SummaryArtifact) UnresolvedWork() []string  { return append([]string(nil), a.unresolvedWork...) }
func (a SummaryArtifact) CreatedAt() time.Time      { return a.createdAt }

func (a SummaryArtifact) Predecessor() *ArtifactRef {
	if a.predecessor == nil {
		return nil
	}
	value := *a.predecessor
	return &value
}

func (a SummaryArtifact) MarshalWire() ([]byte, error) {
	if err := validateSummaryArtifact(a); err != nil {
		return nil, err
	}
	return projectjson.Marshal(struct {
		Ref       ArtifactRef         `json:"ref"`
		Wire      summaryArtifactWire `json:"wire"`
		CreatedAt time.Time           `json:"created_at"`
	}{Ref: a.ref, Wire: a.wire(), CreatedAt: a.createdAt})
}

func UnmarshalSummaryArtifact(data []byte) (SummaryArtifact, error) {
	var envelope struct {
		Ref       ArtifactRef         `json:"ref"`
		Wire      summaryArtifactWire `json:"wire"`
		CreatedAt time.Time           `json:"created_at"`
	}
	if err := projectjson.UnmarshalStrict(data, &envelope); err != nil {
		return SummaryArtifact{}, contextError(CodeInvalidRequest, "unmarshal_artifact", "", ErrInvalidRequest, err)
	}
	artifact := SummaryArtifact{
		ref: envelope.Ref, source: envelope.Wire.Source, coveredThrough: envelope.Wire.CoveredThrough,
		sourceDigest: envelope.Wire.SourceDigest, runtimeDigest: envelope.Wire.RuntimeDigest,
		factSet: envelope.Wire.FactSet, messages: envelope.Wire.Messages,
		unresolvedWork: envelope.Wire.UnresolvedWork, predecessor: envelope.Wire.Predecessor,
		createdAt: envelope.CreatedAt,
	}
	if envelope.Wire.SchemaVersion != CurrentSchemaVersion || envelope.Wire.Generation != envelope.Ref.Generation {
		return SummaryArtifact{}, contextError(CodeArtifactConflict, "unmarshal_artifact", envelope.Ref.Key, ErrArtifactConflict, nil)
	}
	if err := validateSummaryArtifact(artifact); err != nil {
		return SummaryArtifact{}, err
	}
	return cloneSummaryArtifact(artifact), nil
}

func NewCompactor(options CompactionOptions) (*Compactor, error) {
	if options.Summarizer == nil || options.Artifacts == nil || options.Counter == nil || options.Clock == nil || options.Counter.ID() == "" || options.MinSavings < 0 {
		return nil, contextError(CodeInvalidRequest, "new_compactor", "", ErrInvalidRequest, fmt.Errorf("summarizer, artifact store, token counter, and clock are required"))
	}
	return &Compactor{options: options}, nil
}

type Compactor struct {
	options CompactionOptions
}

func (c *Compactor) Compact(ctx stdcontext.Context, request CompactRequest) (CompactResult, error) {
	if ctx == nil || c == nil {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, fmt.Errorf("invalid compactor"))
	}
	if err := ctx.Err(); err != nil {
		return CompactResult{}, err
	}
	if !request.Source.TenantKey.Valid() || request.Source.SessionKey == "" || request.Source.SessionRevision == 0 || request.RuntimeDigest == "" || request.TargetTokens <= 0 || request.CoveredThrough == 0 {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, fmt.Errorf("exact source, runtime, target, and coverage are required"))
	}
	normalized, err := NormalizeHistory(NormalizeRequest{Messages: request.Messages, Policy: RepairReject})
	if err != nil {
		return CompactResult{}, err
	}
	factSet, err := NewFactSet(request.ProtectedFacts)
	if err != nil {
		return CompactResult{}, err
	}
	cut := selectSafePrefix(normalized.Messages)
	if cut == 0 {
		return CompactResult{}, contextError(CodeCompactionNoProgress, "compact", request.Source.SessionKey, ErrCompactionNoProgress, nil)
	}
	candidate := cloneMessages(normalized.Messages[:cut])
	kept := cloneMessages(normalized.Messages[cut:])
	candidateNormalized, err := NormalizeHistory(NormalizeRequest{Messages: candidate, Policy: RepairReject})
	if err != nil {
		return CompactResult{}, err
	}
	before, err := c.options.Counter.CountTokens(ctx, normalized.Messages)
	if err != nil {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, err)
	}
	summary, err := c.options.Summarizer.Summarize(ctx, SummaryRequest{
		Source: request.Source, Messages: candidate, ProtectedFacts: cloneFacts(factSet.Facts),
		Predecessor: cloneArtifactRef(request.Predecessor), RuntimeDigest: request.RuntimeDigest,
		SourceDigest: candidateNormalized.Digest,
	})
	if err != nil {
		return CompactResult{}, err
	}
	if summary.Generation == "" {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, fmt.Errorf("summary generation is required"))
	}
	normalizedSummary, err := NormalizeHistory(NormalizeRequest{Messages: summary.Messages, Policy: RepairReject})
	if err != nil {
		return CompactResult{}, err
	}
	if containsNonSummaryContent(normalizedSummary.Messages) {
		return CompactResult{}, contextError(CodeHistoryInvalid, "compact", "", ErrHistoryInvalid, fmt.Errorf("summary cannot manufacture tool or image content"))
	}
	afterMessages := append(cloneMessages(normalizedSummary.Messages), kept...)
	after, err := c.options.Counter.CountTokens(ctx, afterMessages)
	if err != nil {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, err)
	}
	if before-after < c.options.MinSavings || after > request.TargetTokens {
		return CompactResult{}, contextError(CodeCompactionNoProgress, "compact", request.Source.SessionKey, ErrCompactionNoProgress, nil)
	}

	artifact := SummaryArtifact{
		ref:    ArtifactRef{Kind: "summary", Schema: "context-summary/v1", Generation: summary.Generation},
		source: request.Source, coveredThrough: request.CoveredThrough,
		sourceDigest: candidateNormalized.Digest, runtimeDigest: request.RuntimeDigest,
		factSet: factSet, messages: cloneMessages(normalizedSummary.Messages),
		unresolvedWork: append([]string(nil), summary.UnresolvedWork...),
		predecessor:    cloneArtifactRef(request.Predecessor), createdAt: c.options.Clock().UTC(),
	}
	if artifact.createdAt.IsZero() {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, fmt.Errorf("clock returned zero time"))
	}
	wire := artifact.wire()
	digest, err := agent.CanonicalDigest(wire)
	if err != nil {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, err)
	}
	keyDigest, err := agent.CanonicalDigest(struct {
		Source        SourceRef `json:"source"`
		Covered       uint64    `json:"covered_through"`
		SourceDigest  string    `json:"source_digest"`
		RuntimeDigest string    `json:"runtime_digest"`
	}{request.Source, request.CoveredThrough, candidateNormalized.Digest, request.RuntimeDigest})
	if err != nil {
		return CompactResult{}, contextError(CodeInvalidRequest, "compact", "", ErrInvalidRequest, err)
	}
	artifact.ref.Key = "sum_" + keyDigest[:32]
	artifact.ref.Digest = digest
	stored, err := c.options.Artifacts.CreateOrVerify(ctx, artifact)
	if err != nil {
		return CompactResult{}, err
	}
	pivot := PivotRef{Artifact: stored.Ref(), CoveredThrough: stored.CoveredThrough(), SourceDigest: stored.SourceDigest(), FactSetDigest: stored.factSet.Digest}
	return CompactResult{Artifact: stored, Pivot: pivot, Kept: kept}, nil
}

// selectSafePrefix preserves every system boundary, user-intent message, and
// complete tool exchange. Selection is deterministic and contiguous.
func selectSafePrefix(messages []agent.Message) int {
	latestUser := -1
	for i, message := range messages {
		if message.Role == agent.RoleUser {
			latestUser = i
		}
	}
	limit := latestUser
	if limit < 0 {
		limit = len(messages)
	}
	cut := 0
	for cut < limit {
		message := messages[cut]
		if message.Role == agent.RoleSystem || message.Role == agent.RoleUser || message.Role == agent.RoleTool || len(message.ToolCalls()) != 0 {
			break
		}
		cut++
	}
	return cut
}

func containsToolContent(messages []agent.Message) bool {
	for _, message := range messages {
		if message.Role == agent.RoleTool || len(message.ToolCalls()) != 0 || len(message.ToolResults()) != 0 {
			return true
		}
	}
	return false
}

func containsNonSummaryContent(messages []agent.Message) bool {
	if containsToolContent(messages) {
		return true
	}
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == agent.PartImage {
				return true
			}
		}
	}
	return false
}

func (a SummaryArtifact) wire() summaryArtifactWire {
	return summaryArtifactWire{
		SchemaVersion: CurrentSchemaVersion, Source: a.source, CoveredThrough: a.coveredThrough,
		SourceDigest: a.sourceDigest, RuntimeDigest: a.runtimeDigest, FactSet: cloneFactSet(a.factSet),
		Messages: cloneMessages(a.messages), UnresolvedWork: append([]string(nil), a.unresolvedWork...),
		Predecessor: cloneArtifactRef(a.predecessor), Generation: a.ref.Generation,
	}
}

func validateSummaryArtifact(artifact SummaryArtifact) error {
	if !artifact.source.TenantKey.Valid() || artifact.source.SessionKey == "" || artifact.coveredThrough == 0 || artifact.sourceDigest == "" || artifact.runtimeDigest == "" || artifact.createdAt.IsZero() || artifact.ref.Key == "" || artifact.ref.Kind != "summary" || artifact.ref.Schema != "context-summary/v1" || artifact.ref.Digest == "" || artifact.ref.Generation == "" {
		return contextError(CodeInvalidRequest, "validate_artifact", artifact.ref.Key, ErrInvalidRequest, fmt.Errorf("incomplete summary artifact"))
	}
	digest, err := agent.CanonicalDigest(artifact.wire())
	if err != nil || digest != artifact.ref.Digest {
		return contextError(CodeArtifactConflict, "validate_artifact", artifact.ref.Key, ErrArtifactConflict, err)
	}
	keyDigest, keyErr := agent.CanonicalDigest(struct {
		Source        SourceRef `json:"source"`
		Covered       uint64    `json:"covered_through"`
		SourceDigest  string    `json:"source_digest"`
		RuntimeDigest string    `json:"runtime_digest"`
	}{artifact.source, artifact.coveredThrough, artifact.sourceDigest, artifact.runtimeDigest})
	if keyErr != nil || artifact.ref.Key != "sum_"+keyDigest[:32] {
		return contextError(CodeArtifactConflict, "validate_artifact", artifact.ref.Key, ErrArtifactConflict, keyErr)
	}
	facts, err := NewFactSet(artifact.factSet.Facts)
	if err != nil || facts.Digest != artifact.factSet.Digest {
		return contextError(CodeArtifactConflict, "validate_artifact", artifact.ref.Key, ErrArtifactConflict, err)
	}
	normalized, err := NormalizeHistory(NormalizeRequest{Messages: artifact.messages, Policy: RepairReject})
	if err != nil || normalized.Digest == "" || containsNonSummaryContent(normalized.Messages) {
		return contextError(CodeArtifactConflict, "validate_artifact", artifact.ref.Key, ErrArtifactConflict, err)
	}
	return nil
}

func cloneSummaryArtifact(artifact SummaryArtifact) SummaryArtifact {
	artifact.factSet = cloneFactSet(artifact.factSet)
	artifact.messages = cloneMessages(artifact.messages)
	artifact.unresolvedWork = append([]string(nil), artifact.unresolvedWork...)
	artifact.predecessor = cloneArtifactRef(artifact.predecessor)
	return artifact
}

func cloneFactSet(set FactSet) FactSet {
	set.Facts = cloneFacts(set.Facts)
	return set
}

func cloneArtifactRef(value *ArtifactRef) *ArtifactRef {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}
