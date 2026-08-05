package skills

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

type catalogGeneration struct {
	snapshot Snapshot
	skills   map[SkillKey]SourceSkill
	refs     int
	retired  bool
	revoked  bool
}

type scopeState struct {
	started     bool
	starting    chan struct{}
	epoch       uint64
	degraded    bool
	lastError   error
	current     Generation
	generations map[Generation]*catalogGeneration
	refreshMu   sync.Mutex
}

type Manager struct {
	mu      sync.RWMutex
	sources []SourceRegistration
	allowed map[TrustLevel]struct{}
	limits  Limits
	clock   func() time.Time
	scopes  map[agent.TenantKey]*scopeState
	closed  bool
}

func NewCatalog(options Options) (*Manager, error) {
	ordered := make([]SourceRegistration, len(options.Sources))
	copy(ordered, options.Sources)
	if len(ordered) == 0 {
		return nil, skillError(CodeInvalidSource, "new_catalog", ErrInvalidSource)
	}
	seenKind := make(map[SourceKind]struct{}, len(ordered))
	seenName := make(map[string]struct{}, len(ordered))
	for _, registration := range ordered {
		if registration.Source == nil || registration.Source.Name() == "" || precedence(registration.Source.Kind()) < 0 {
			return nil, skillError(CodeInvalidSource, "new_catalog", ErrInvalidSource)
		}
		if _, ok := seenKind[registration.Source.Kind()]; ok {
			return nil, skillError(CodeInvalidSource, "new_catalog", ErrInvalidSource)
		}
		seenKind[registration.Source.Kind()] = struct{}{}
		if _, ok := seenName[registration.Source.Name()]; ok {
			return nil, skillError(CodeInvalidSource, "new_catalog", ErrInvalidSource)
		}
		seenName[registration.Source.Name()] = struct{}{}
	}
	sort.Slice(ordered, func(i, j int) bool {
		return precedence(ordered[i].Source.Kind()) < precedence(ordered[j].Source.Kind())
	})
	allowed := make(map[TrustLevel]struct{})
	if len(options.AllowedTrust) == 0 {
		allowed[TrustPlatform] = struct{}{}
		allowed[TrustTenantReviewed] = struct{}{}
	} else {
		for _, trust := range options.AllowedTrust {
			if trust != TrustPlatform && trust != TrustTenantReviewed && trust != TrustTenantUnreviewed {
				return nil, skillError(CodeTrustDenied, "new_catalog", ErrTrustDenied)
			}
			allowed[trust] = struct{}{}
		}
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Manager{sources: ordered, allowed: allowed, limits: normalizeLimits(options.Limits), clock: clock, scopes: make(map[agent.TenantKey]*scopeState)}, nil
}

func (m *Manager) StartScope(ctx context.Context, scope Scope) error {
	if !scope.TenantKey.Valid() {
		return skillError(CodeInvalidSource, "start_scope", ErrInvalidSource)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return skillError(CodeClosed, "start_scope", ErrClosed)
	}
	if state := m.scopes[scope.TenantKey]; state != nil && state.started {
		waiting := state.starting
		m.mu.Unlock()
		if waiting == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waiting:
		}
		m.mu.RLock()
		current := m.scopes[scope.TenantKey]
		ready := current == state && state.started && state.current != ""
		lastError := state.lastError
		m.mu.RUnlock()
		if ready {
			return nil
		}
		if lastError != nil {
			return lastError
		}
		return skillError(CodeCatalogNotReady, "start_scope", ErrCatalogNotReady)
	}
	state := m.scopes[scope.TenantKey]
	if state == nil {
		state = &scopeState{generations: make(map[Generation]*catalogGeneration)}
		m.scopes[scope.TenantKey] = state
	}
	state.started = true
	state.epoch++
	state.starting = make(chan struct{})
	starting := state.starting
	startEpoch := state.epoch
	m.mu.Unlock()
	if _, err := m.Refresh(ctx, scope); err != nil {
		m.mu.Lock()
		if current := m.scopes[scope.TenantKey]; current == state && current.epoch == startEpoch && current.starting == starting {
			if current.current == "" {
				current.started = false
			}
			if current.starting != nil {
				close(current.starting)
				current.starting = nil
			}
		}
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	if current := m.scopes[scope.TenantKey]; current == state && current.epoch == startEpoch && current.starting == starting {
		close(current.starting)
		current.starting = nil
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) CloseScope(_ context.Context, scope Scope) error {
	if !scope.TenantKey.Valid() {
		return skillError(CodeInvalidSource, "close_scope", ErrInvalidSource)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return skillError(CodeClosed, "close_scope", ErrClosed)
	}
	state := m.scopes[scope.TenantKey]
	if state == nil || !state.started {
		return nil
	}
	state.started = false
	state.epoch++
	if state.starting != nil {
		close(state.starting)
		state.starting = nil
	}
	state.current = ""
	for _, generation := range state.generations {
		generation.retired = true
	}
	m.pruneLocked(scope.TenantKey, state)
	return nil
}

func (m *Manager) Current(scope Scope) (Snapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return Snapshot{}, false
	}
	state := m.scopes[scope.TenantKey]
	if state == nil || !state.started {
		return Snapshot{}, false
	}
	generation := state.generations[state.current]
	if generation == nil || generation.revoked {
		return Snapshot{}, false
	}
	return cloneSnapshot(generation.snapshot), true
}

func (m *Manager) Refresh(ctx context.Context, scope Scope) (Snapshot, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return Snapshot{}, skillError(CodeClosed, "refresh", ErrClosed)
	}
	state := m.scopes[scope.TenantKey]
	started := state != nil && state.started
	hadCurrent := state != nil && state.current != ""
	epoch := uint64(0)
	if state != nil {
		epoch = state.epoch
	}
	sources := append([]SourceRegistration(nil), m.sources...)
	m.mu.RUnlock()
	if !scope.TenantKey.Valid() || !started {
		return Snapshot{}, skillError(CodeCatalogNotReady, "refresh", ErrCatalogNotReady)
	}
	state.refreshMu.Lock()
	defer state.refreshMu.Unlock()
	refreshState := state
	m.mu.RLock()
	stillStarted := !m.closed && m.scopes[scope.TenantKey] == state && state.started && state.epoch == epoch
	m.mu.RUnlock()
	if !stillStarted {
		return Snapshot{}, skillError(CodeCatalogNotReady, "refresh", ErrCatalogNotReady)
	}
	loaded := make([]loadedSource, 0, len(sources))
	diagnostics := make([]Diagnostic, 0)
	var degradedErr error
	for _, registration := range sources {
		value, err := registration.Source.Load(ctx, scope)
		if err != nil {
			if registration.Required || hadCurrent {
				loadErr := &Error{Code: CodeInvalidSource, Operation: "refresh", Source: registration.Source.Name(), Cause: errors.Join(ErrInvalidSource, err)}
				m.markDegraded(scope.TenantKey, loadErr)
				return m.retainedSnapshot(scope, refreshState, loadErr)
			}
			degradedErr = errors.Join(degradedErr, err)
			diagnostics = append(diagnostics, diagnostic(registration.Source.Name(), "", CodeInvalidSource, "optional source unavailable"))
			continue
		}
		if value.Generation == "" {
			err = &Error{Code: CodeInvalidSource, Operation: "refresh", Source: registration.Source.Name(), Cause: ErrInvalidSource}
			if registration.Required || hadCurrent {
				m.markDegraded(scope.TenantKey, err)
				return m.retainedSnapshot(scope, refreshState, err)
			}
			degradedErr = errors.Join(degradedErr, err)
			diagnostics = append(diagnostics, diagnostic(registration.Source.Name(), "", CodeInvalidSource, "optional source generation missing"))
			continue
		}
		loaded = append(loaded, loadedSource{registration.Source, value})
		for _, item := range value.Diagnostics {
			code := item.Code
			if !validDiagnosticCode(code) {
				code = CodeDescriptorInvalid
			}
			diagnostics = append(diagnostics, diagnostic(registration.Source.Name(), item.Skill, code, codeMessage(code)))
		}
	}
	candidate, err := m.build(scope, loaded, diagnostics)
	if err != nil {
		m.markDegraded(scope.TenantKey, err)
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Snapshot{}, skillError(CodeClosed, "refresh", ErrClosed)
	}
	state = m.scopes[scope.TenantKey]
	if state == nil || state != refreshState || !state.started || state.epoch != epoch {
		return Snapshot{}, skillError(CodeCatalogNotReady, "refresh", ErrCatalogNotReady)
	}
	if existing := state.generations[candidate.snapshot.Generation]; existing != nil {
		if previous := state.generations[state.current]; previous != nil && previous != existing {
			previous.retired = true
		}
		state.current = candidate.snapshot.Generation
		state.degraded = degradedErr != nil
		state.lastError = degradedErr
		existing.retired = false
		m.pruneLocked(scope.TenantKey, state)
		return cloneSnapshot(existing.snapshot), nil
	}
	if previous := state.generations[state.current]; previous != nil {
		previous.retired = true
	}
	state.generations[candidate.snapshot.Generation] = candidate
	state.current = candidate.snapshot.Generation
	state.degraded = degradedErr != nil
	state.lastError = degradedErr
	m.pruneLocked(scope.TenantKey, state)
	return cloneSnapshot(candidate.snapshot), nil
}

func (m *Manager) retainedSnapshot(scope Scope, expected *scopeState, refreshErr error) (Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.scopes[scope.TenantKey]
	if state == nil || state != expected {
		return Snapshot{}, refreshErr
	}
	generation := state.generations[state.current]
	if generation == nil || generation.revoked {
		return Snapshot{}, refreshErr
	}
	return cloneSnapshot(generation.snapshot), refreshErr
}

type loadedSource struct {
	source   Source
	snapshot SourceSnapshot
}

func (m *Manager) build(_ Scope, loaded []loadedSource, diagnostics []Diagnostic) (*catalogGeneration, error) {
	selected := make(map[SkillKey]SourceSkill)
	blocked := make(map[SkillKey]bool)
	sourceGenerations := make(map[string]string, len(loaded))
	limits := m.limits
	for _, item := range loaded {
		sourceGenerations[item.source.Name()] = item.snapshot.Generation
		groups := make(map[SkillKey][]SourceSkill)
		for _, raw := range item.snapshot.Skills {
			if raw.Disabled {
				continue
			}
			validated, err := validateAndDigest(raw, SourceRef{Name: item.source.Name(), Kind: item.source.Kind(), Generation: item.snapshot.Generation}, limits)
			if err != nil {
				diagnostics = append(diagnostics, diagnosticForError(item.source.Name(), raw.Descriptor.Key, err))
				continue
			}
			groups[validated.Descriptor.Key] = append(groups[validated.Descriptor.Key], validated)
		}
		keys := make([]SkillKey, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, key := range keys {
			values := groups[key]
			if len(values) != 1 {
				delete(selected, key)
				blocked[key] = true
				diagnostics = append(diagnostics, diagnostic(item.source.Name(), key, CodeDuplicateSkill, "duplicate skill"))
				continue
			}
			candidate := values[0]
			if _, allowed := m.allowed[candidate.Descriptor.Trust]; !allowed {
				diagnostics = append(diagnostics, diagnostic(item.source.Name(), key, CodeTrustDenied, "trust denied"))
				continue
			}
			lower, exists := selected[key]
			if exists && !replacementMatches(candidate.Descriptor.Replaces, lower.Descriptor) {
				delete(selected, key)
				blocked[key] = true
				diagnostics = append(diagnostics, diagnostic(item.source.Name(), key, CodePrecedenceConflict, "override requires matching replaces"))
				continue
			}
			if blocked[key] && !exists {
				continue
			}
			selected[key] = candidate
		}
		if item.source.Kind() == SourceTenantDB {
			seenTombstones := make(map[SkillKey]struct{}, len(item.snapshot.Tombstones))
			for _, tombstone := range item.snapshot.Tombstones {
				if tombstone.Key == "" || !namePattern.MatchString(string(tombstone.Key)) {
					diagnostics = append(diagnostics, diagnostic(item.source.Name(), tombstone.Key, CodeDescriptorInvalid, "invalid tombstone"))
					continue
				}
				if _, duplicate := seenTombstones[tombstone.Key]; duplicate {
					diagnostics = append(diagnostics, diagnostic(item.source.Name(), tombstone.Key, CodeDuplicateSkill, "duplicate tombstone"))
				}
				seenTombstones[tombstone.Key] = struct{}{}
				delete(selected, tombstone.Key)
				blocked[tombstone.Key] = true
			}
		} else if len(item.snapshot.Tombstones) > 0 {
			return nil, &Error{Code: CodeInvalidSource, Operation: "build", Source: item.source.Name(), Cause: ErrInvalidSource}
		}
	}
	keys := make([]SkillKey, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	descriptors := make([]Descriptor, 0, len(keys))
	immutable := make(map[SkillKey]SourceSkill, len(keys))
	for _, key := range keys {
		immutable[key] = cloneSkill(selected[key])
		descriptors = append(descriptors, cloneDescriptor(selected[key].Descriptor))
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Source != diagnostics[j].Source {
			return diagnostics[i].Source < diagnostics[j].Source
		}
		if diagnostics[i].Skill != diagnostics[j].Skill {
			return diagnostics[i].Skill < diagnostics[j].Skill
		}
		return diagnostics[i].Code < diagnostics[j].Code
	})
	digestInput := struct {
		Schema      uint16            `json:"schema"`
		Sources     map[string]string `json:"sources"`
		Descriptors []Descriptor      `json:"descriptors"`
		Diagnostics []Diagnostic      `json:"diagnostics,omitempty"`
	}{CurrentSchemaVersion, sourceGenerations, descriptors, diagnostics}
	body, err := jsoncodec.Marshal(digestInput)
	if err != nil {
		return nil, fmt.Errorf("digest catalog: %w", err)
	}
	digest := digestBytes(body)
	snapshot := Snapshot{Generation: Generation(digest), CreatedAt: m.clock().UTC(), SourceGenerations: sourceGenerations, Digest: digest, Descriptors: descriptors, Diagnostics: diagnostics}
	return &catalogGeneration{snapshot: snapshot, skills: immutable}, nil
}

func (m *Manager) Resolve(request ResolveRequest) (Selection, error) {
	if request.Generation == "" {
		return Selection{}, &Error{Code: CodeGenerationUnavailable, Operation: "resolve", Cause: ErrGenerationUnavailable}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	generation, err := m.getGenerationLocked(request.Scope, request.Generation, "resolve")
	if err != nil {
		return Selection{}, err
	}
	selectors := append([]Selector(nil), request.Selectors...)
	sort.Slice(selectors, func(i, j int) bool {
		if selectors[i].Key == selectors[j].Key {
			return selectors[i].Version < selectors[j].Version
		}
		return selectors[i].Key < selectors[j].Key
	})
	result := Selection{Generation: generation.snapshot.Generation}
	seen := make(map[SkillKey]struct{}, len(selectors))
	for _, selector := range selectors {
		if _, duplicate := seen[selector.Key]; duplicate {
			return Selection{}, &Error{Code: CodeDuplicateSkill, Operation: "resolve", Skill: selector.Key, Cause: ErrDuplicateSkill}
		}
		seen[selector.Key] = struct{}{}
		skill, ok := generation.skills[selector.Key]
		if !ok || (selector.Version != "" && skill.Descriptor.Version != selector.Version) {
			return Selection{}, &Error{Code: CodeSkillNotFound, Operation: "resolve", Skill: selector.Key, Generation: generation.snapshot.Generation, Cause: ErrSkillNotFound}
		}
		d := cloneDescriptor(skill.Descriptor)
		result.Skills = append(result.Skills, d)
		result.Prompt = append(result.Prompt, promptMetadata(d))
	}
	return result, nil
}

func (m *Manager) Read(ctx context.Context, request ReadRequest) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	if request.Generation == "" {
		return Resource{}, &Error{Code: CodeGenerationUnavailable, Operation: "read", Cause: ErrGenerationUnavailable}
	}
	if request.Version == "" {
		return Resource{}, &Error{Code: CodeVersionInvalid, Operation: "read", Skill: request.Skill, Cause: ErrVersionInvalid}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	generation, err := m.getGenerationLocked(request.Scope, request.Generation, "read")
	if err != nil {
		return Resource{}, err
	}
	skill, ok := generation.skills[request.Skill]
	if !ok || (request.Version != "" && request.Version != skill.Descriptor.Version) {
		return Resource{}, &Error{Code: CodeSkillNotFound, Operation: "read", Skill: request.Skill, Generation: generation.snapshot.Generation, Cause: ErrSkillNotFound}
	}
	key := request.Artifact
	if key == "" {
		key = InstructionsKey
	}
	if key == InstructionsKey {
		content := append([]byte(nil), skill.Instructions...)
		if skill.readInstructions != nil {
			content, err = skill.readInstructions()
			if err != nil {
				return Resource{}, &Error{Code: CodeResourceChanged, Operation: "read", Skill: request.Skill, Generation: generation.snapshot.Generation, Cause: errors.Join(ErrResourceChanged, err)}
			}
		}
		if digestBytes(content) != skill.Descriptor.InstructionsDigest {
			return Resource{}, &Error{Code: CodeResourceChanged, Operation: "read", Skill: request.Skill, Cause: ErrResourceChanged}
		}
		return Resource{Skill: request.Skill, Version: skill.Descriptor.Version, Key: key, MIMEType: "text/markdown; charset=utf-8", Content: content, Digest: skill.Descriptor.InstructionsDigest, ContentClass: ContentUntrustedInstructions, Trusted: false, Provenance: skill.Descriptor.Source}, nil
	}
	descriptor, declared := descriptorByArtifact(skill.Descriptor, key)
	content, present := skill.Artifacts[key]
	if !declared || !present {
		return Resource{}, &Error{Code: CodeResourceNotFound, Operation: "read", Skill: request.Skill, Generation: generation.snapshot.Generation, Cause: ErrResourceNotFound}
	}
	if skill.readArtifact != nil {
		content, err = skill.readArtifact(key)
		if err != nil {
			return Resource{}, &Error{Code: CodeResourceChanged, Operation: "read", Skill: request.Skill, Generation: generation.snapshot.Generation, Cause: errors.Join(ErrResourceChanged, err)}
		}
	}
	body := append([]byte(nil), content...)
	if int64(len(body)) != descriptor.SizeBytes || digestBytes(body) != descriptor.Digest {
		return Resource{}, &Error{Code: CodeResourceChanged, Operation: "read", Skill: request.Skill, Cause: ErrResourceChanged}
	}
	return Resource{Skill: request.Skill, Version: skill.Descriptor.Version, Key: key, MIMEType: descriptor.MIMEType, Content: body, Digest: descriptor.Digest, ContentClass: ContentUntrustedArtifact, Trusted: false, Provenance: skill.Descriptor.Source}, nil
}

func (m *Manager) getGenerationLocked(scope Scope, requested Generation, operation string) (*catalogGeneration, error) {
	if m.closed {
		return nil, skillError(CodeClosed, operation, ErrClosed)
	}
	state := m.scopes[scope.TenantKey]
	if state == nil {
		return nil, skillError(CodeCatalogNotReady, operation, ErrCatalogNotReady)
	}
	generation := state.generations[requested]
	if generation == nil {
		return nil, &Error{Code: CodeGenerationUnavailable, Operation: operation, Generation: requested, Cause: ErrGenerationUnavailable}
	}
	if generation.revoked {
		return nil, &Error{Code: CodeRevoked, Operation: operation, Generation: requested, Cause: ErrRevoked}
	}
	return generation, nil
}

type Lease struct {
	manager    *Manager
	tenant     agent.TenantKey
	generation Generation
	once       sync.Once
}

func (m *Manager) Acquire(scope Scope, generation Generation) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, skillError(CodeClosed, "acquire", ErrClosed)
	}
	state := m.scopes[scope.TenantKey]
	if state == nil || !state.started {
		return nil, skillError(CodeCatalogNotReady, "acquire", ErrCatalogNotReady)
	}
	if generation == "" {
		generation = state.current
	}
	value := state.generations[generation]
	if value == nil || value.retired {
		return nil, &Error{Code: CodeGenerationUnavailable, Operation: "acquire", Generation: generation, Cause: ErrGenerationUnavailable}
	}
	if value.revoked {
		return nil, &Error{Code: CodeRevoked, Operation: "acquire", Generation: generation, Cause: ErrRevoked}
	}
	value.refs++
	return &Lease{manager: m, tenant: scope.TenantKey, generation: generation}, nil
}
func (l *Lease) Generation() Generation {
	if l == nil {
		return ""
	}
	return l.generation
}
func (l *Lease) Snapshot() (Snapshot, error) {
	if l == nil || l.manager == nil {
		return Snapshot{}, ErrGenerationUnavailable
	}
	l.manager.mu.RLock()
	defer l.manager.mu.RUnlock()
	generation, err := l.manager.getGenerationLocked(Scope{TenantKey: l.tenant}, l.generation, "lease_snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(generation.snapshot), nil
}
func (l *Lease) Release() {
	if l == nil || l.manager == nil {
		return
	}
	l.once.Do(func() { l.manager.release(l.tenant, l.generation) })
}
func (l *Lease) Close() error { l.Release(); return nil }
func (m *Manager) release(tenant agent.TenantKey, generation Generation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.scopes[tenant]
	if state == nil {
		return
	}
	value := state.generations[generation]
	if value == nil {
		return
	}
	if value.refs > 0 {
		value.refs--
	}
	m.pruneLocked(tenant, state)
}
func (m *Manager) pruneLocked(tenant agent.TenantKey, state *scopeState) {
	for key, generation := range state.generations {
		if generation.retired && generation.refs == 0 {
			delete(state.generations, key)
		}
	}
	if !state.started && len(state.generations) == 0 {
		delete(m.scopes, tenant)
	}
}

func (m *Manager) Revoke(_ context.Context, request RevokeRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return skillError(CodeClosed, "revoke", ErrClosed)
	}
	state := m.scopes[request.Scope.TenantKey]
	if state == nil {
		return skillError(CodeCatalogNotReady, "revoke", ErrCatalogNotReady)
	}
	generation := state.generations[request.Generation]
	if generation == nil {
		return &Error{Code: CodeGenerationUnavailable, Operation: "revoke", Generation: request.Generation, Cause: ErrGenerationUnavailable}
	}
	generation.revoked = true
	if state.current == request.Generation {
		state.degraded = true
		state.lastError = ErrRevoked
	}
	return nil
}
func (m *Manager) Status(scope Scope) ScopeStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.scopes[scope.TenantKey]
	if state == nil {
		return ScopeStatus{}
	}
	return ScopeStatus{Started: state.started, Degraded: state.degraded, LastError: state.lastError, Generation: state.current}
}
func (m *Manager) markDegraded(tenant agent.TenantKey, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.scopes[tenant]; state != nil {
		state.degraded = true
		state.lastError = err
	}
}
func (m *Manager) Close(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	for tenant, state := range m.scopes {
		state.started = false
		state.epoch++
		if state.starting != nil {
			close(state.starting)
			state.starting = nil
		}
		state.current = ""
		for _, generation := range state.generations {
			generation.retired = true
		}
		m.pruneLocked(tenant, state)
	}
	return nil
}

func promptMetadata(d Descriptor) PromptMetadata {
	return PromptMetadata{Key: d.Key, Name: d.Name, Description: d.Description, Version: d.Version, Trust: d.Trust, Compatibility: d.Compatibility, Location: fmt.Sprintf("skill://%s@%s/%s", d.Key, d.Version, InstructionsKey), ContentClass: ContentUntrustedInstructions, Provenance: d.Source}
}
func precedence(kind SourceKind) int {
	switch kind {
	case SourcePlatformFilesystem:
		return 0
	case SourceTenantFilesystem:
		return 1
	case SourceTenantDB:
		return 2
	default:
		return -1
	}
}
func validDiagnosticCode(code Code) bool {
	switch code {
	case CodeInvalidSource, CodeDescriptorInvalid, CodeVersionInvalid, CodeDuplicateSkill, CodePrecedenceConflict, CodeTrustDenied, CodeResourceNotFound, CodeResourceEscape, CodeResourceChanged, CodeResourceTooLarge, CodeSchemaUnsupported:
		return true
	default:
		return false
	}
}
func (s Snapshot) Descriptor(key SkillKey) (Descriptor, bool) {
	index := sort.Search(len(s.Descriptors), func(i int) bool { return s.Descriptors[i].Key >= key })
	if index == len(s.Descriptors) || s.Descriptors[index].Key != key {
		return Descriptor{}, false
	}
	return cloneDescriptor(s.Descriptors[index]), true
}
func (s Snapshot) Clone() Snapshot      { return cloneSnapshot(s) }
func (s Snapshot) Skills() []Descriptor { return cloneDescriptors(s.Descriptors) }
func (s Selection) Clone() Selection {
	s.Skills = cloneDescriptors(s.Skills)
	s.Prompt = append([]PromptMetadata(nil), s.Prompt...)
	return s
}
func (r Resource) Clone() Resource { r.Content = append([]byte(nil), r.Content...); return r }
