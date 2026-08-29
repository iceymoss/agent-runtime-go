package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

// childAgentSpec declares one delegate: what it is for, what it may read, and
// what it is allowed to spend.
//
// Every child is read-only. A delegate exists to produce an opinion or a map of
// the code, not to change the workspace, and giving it write tools would make
// the parent's approval record meaningless.
type childAgentSpec struct {
	key          string
	toolName     string
	description  string
	systemPrompt string
	maxSteps     int
	maxTokens    int
	// inputTokens and outputTokens bound the child's share of the tree budget.
	inputTokens  int64
	outputTokens int64
	maxToolCalls int64
	runtime      time.Duration
}

// childAgentSpecs are the delegates iCoder ships with. Reviewer judges a change;
// explorer answers "where is this and how does it fit together" without spending
// the parent's context on the search itself.
func childAgentSpecs() []childAgentSpec {
	return []childAgentSpec{
		{
			key: "icoder.reviewer", toolName: "delegate_review",
			description: "Delegate a focused code review to an independent read-only child run.",
			systemPrompt: "You are an independent read-only code reviewer. Inspect relevant files and git changes before reporting findings. " +
				"Prioritize correctness, security, regressions, and missing tests. Return findings with file and line references; " +
				"state explicitly when no findings are found.",
			maxSteps: 10, maxTokens: 2048,
			inputTokens: 8192, outputTokens: 2048, maxToolCalls: 16, runtime: 2 * time.Minute,
		},
		{
			key: "icoder.explorer", toolName: "delegate_explore",
			description: "Delegate codebase exploration to an independent read-only child run. Use it when locating code would cost many search and read steps in this run.",
			systemPrompt: "You are an independent read-only codebase explorer. Locate the files, symbols, and call paths that answer the question. " +
				"Use glob_files and search_code before reading, and read only the ranges that matter. " +
				"Answer with concrete file paths and line references plus a short explanation of how the pieces fit together. " +
				"Never speculate about code you did not read.",
			maxSteps: 12, maxTokens: 1536,
			inputTokens: 12288, outputTokens: 1536, maxToolCalls: 24, runtime: 2 * time.Minute,
		},
	}
}

// delegationRunner executes one claimed child. It is the subagent.Runner port:
// the service owns the relationship, budget, and cancellation state machine, and
// this type owns only "run this agent on this input".
type delegationRunner struct {
	agents map[string]childRuntime
	store  *Store
}

// childRuntime pairs a child agent with the system prompt that defines it. The
// prompt belongs to the child, not to the task the parent delegates, so it is
// applied here and never taken from the parent's input.
type childRuntime struct {
	runner *agent.Agent
	prompt string
}

func (r *delegationRunner) Run(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	child, ok := r.agents[request.AgentKey]
	if !ok {
		return subagent.RunResult{}, fmt.Errorf("no child agent is registered for %q", request.AgentKey)
	}
	result, err := child.runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage(child.prompt),
		agent.NewUserMessage(string(request.Input)),
	}})
	if err != nil {
		return subagent.RunResult{}, err
	}
	// The result is stored under a content-addressed reference so the parent
	// receives a small structured handle instead of the child's whole history.
	ref := subagent.ResultRef(request.AgentKey + ":" + digest(request.Input)[7:23])
	if err := r.store.SaveDelegationResult(ctx, string(ref), string(request.Child.RelationshipKey), request.AgentKey, result.Text); err != nil {
		return subagent.RunResult{}, err
	}
	return subagent.RunResult{
		State: subagent.ChildCompleted, ResultRef: ref, UsageFactKey: subagent.UsageFactKey(ref),
		Usage: subagent.Usage{InputTokens: int64(result.Usage.PromptTokens), OutputTokens: int64(result.Usage.CompletionTokens)},
	}, nil
}

// eventWaker turns a finished child into a reliable parent-visible fact.
//
// The parent run is not suspended while a child works, so the wake is not a
// control signal here; it is the durable record that the delegation finished,
// which is what an audit and the session transcript need.
type eventWaker struct{ store *Store }

func (w eventWaker) Wake(ctx context.Context, request subagent.WakeRequest) error {
	return w.store.AppendRunEvent(ctx, string(request.Parent.SessionKey), "subagent:"+string(request.WakeKey), "agent.subagent.completed", map[string]any{
		"relationship_key": request.Child.RelationshipKey,
		"child_run_key":    request.Child.RunKey,
		"agent_key":        request.Parent.RunKey,
		"state":            request.State,
	})
}

// delegateTool spawns one child and parks until the child is done.
//
// Parking rather than blocking is what makes delegation survive a crash: the
// parent run is checkpointed with a handle to the child, so a process that dies
// while a review is running leaves a resumable run instead of losing the work
// and the tokens already spent on it. From the terminal the turn still looks
// synchronous, because the application drives the child and resumes immediately.
type delegateTool struct {
	spec      childAgentSpec
	service   *subagent.Service
	store     *Store
	sessionID func() string
}

func (t *delegateTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: t.spec.toolName, Description: t.spec.description, Strict: true,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"task": map[string]any{"type": "string", "description": "The question or review scope for the child run."}},
			"required":   []any{"task"},
		},
	}
}

func (*delegateTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }

func (t *delegateTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	limits := subagent.Limits{
		MaxDepth: 2, MaxFanout: 4, MaxInputTokens: t.spec.inputTokens, MaxOutputTokens: t.spec.outputTokens,
		MaxCostMicros: 1_000_000, MaxToolCalls: t.spec.maxToolCalls, MaxRuntime: t.spec.runtime,
	}
	reserve := subagent.Reservation{
		InputTokens: t.spec.inputTokens, OutputTokens: t.spec.outputTokens,
		CostMicros: 1_000_000, ToolCalls: t.spec.maxToolCalls, Runtime: t.spec.runtime,
	}
	sessionID := t.sessionID()
	if run := currentRunContext(ctx); run.session != "" {
		sessionID = run.session
	}
	receipt, err := t.service.Spawn(ctx, subagent.SpawnRequest{
		// The tool call id makes the spawn idempotent: a replayed invocation
		// adopts the existing child instead of starting a second one.
		RequestKey: subagent.RequestKey(invocation.CallID),
		Parent:     subagent.ParentRef{TenantKey: tenantKey, SessionKey: subagent.SessionKey(sessionID), RunKey: subagent.RunKey(sessionID)},
		AgentKey:   t.spec.key, Input: []byte(input.Task), Limits: limits, Reserve: reserve,
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	// A resume carries the handle this tool issued, so it can look up the child it
	// started instead of spawning a second one. The spawn above is what makes that
	// safe: it is idempotent, so the resumed call adopts the same child.
	return t.collect(ctx, receipt)
}

// collect reports the child's outcome, or parks again if it is still working.
//
// The spec digest travels as the resume token: it is derived from the exact
// spawn request, so a resume can only continue the delegation it belongs to.
func (t *delegateTool) collect(ctx context.Context, receipt subagent.SpawnReceipt) (agent.ToolResult, error) {
	snapshot, err := t.service.Get(ctx, tenantKey, receipt.Child.RelationshipKey)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !snapshot.State.Terminal() {
		// A wake that arrives before the child finished parks again rather than
		// failing: the tool has not gone wrong, the external event it is waiting
		// on simply has not happened yet. The caller bounds how many times it is
		// willing to re-enter, so an unfinishable child ends the attempt there.
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind:        agent.ToolSuspensionExternal,
			RequestRef:  string(receipt.Child.RelationshipKey),
			ResumeToken: receipt.SpecDigest,
			Revision:    snapshot.Version,
		}}
	}
	if snapshot.State != subagent.ChildCompleted {
		payload, marshalErr := marshalString(map[string]any{
			"child": receipt.Child.RunKey, "state": snapshot.State, "failure": snapshot.Failure,
		})
		return agent.ToolResult{Content: payload, IsError: true}, marshalErr
	}
	content, ok, err := t.store.LoadDelegationResult(ctx, string(snapshot.ResultRef))
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !ok {
		return agent.ToolResult{Content: "child completed without a readable result", IsError: true}, nil
	}
	data, err := marshalString(map[string]any{
		"child": receipt.Child.RunKey, "agent": t.spec.key, "state": snapshot.State,
		"result_ref": snapshot.ResultRef, "result": content,
		"usage": map[string]int64{"input_tokens": snapshot.Usage.InputTokens, "output_tokens": snapshot.Usage.OutputTokens},
	})
	return agent.ToolResult{Content: data}, err
}

// subagentToolEntries builds every child agent and exposes each as one tool.
//
// The children share one service, so depth and fanout limits apply across the
// whole delegation tree rather than per tool.
func subagentToolEntries(model agent.Model, modelName string, workspace *Workspace, store *Store, sessionID func() string) ([]lifecycleEntry, *subagent.Service, error) {
	readOnly := []agent.Tool{
		workingDirectoryTool{workspace}, listFilesTool{workspace}, globFilesTool{workspace},
		readFileTool{workspace}, searchCodeTool{workspace}, gitStatusTool{workspace}, gitDiffTool{workspace},
	}
	runner := &delegationRunner{agents: make(map[string]childRuntime), store: store}
	specs := childAgentSpecs()
	for _, spec := range specs {
		registry := agent.NewRegistry()
		for _, tool := range readOnly {
			if err := registry.Register(tool); err != nil {
				return nil, nil, err
			}
		}
		maxTokens := spec.maxTokens
		child, err := agent.New(agent.Config{
			Key: spec.key, ModelName: modelName, MaxSteps: spec.maxSteps, MaxTokens: &maxTokens,
			AllowedTools: registry.Names(),
		}, model, registry)
		if err != nil {
			return nil, nil, err
		}
		runner.agents[spec.key] = childRuntime{runner: child, prompt: spec.systemPrompt}
	}
	service, err := subagent.New(subagent.Options{
		Store: store.SubagentStore(), Runner: runner, ParentWaker: eventWaker{store: store},
		WorkerID: "icoder-worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		return nil, nil, err
	}
	entries := make([]lifecycleEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, lifecycleEntry{
			tool: &delegateTool{spec: spec, service: service, store: store, sessionID: sessionID},
			// Delegating is idempotent by execution key: the child's request key
			// deduplicates a repeated spawn of the same call.
			metadata: externalMetadata("subagent.spawn", "subagent."+spec.key, true),
			resource: permission.Resource{Kind: "subagent", Key: spec.key},
		})
	}
	return entries, service, nil
}

// Delegation is one recorded child run, for inspection from the CLI.
type Delegation struct {
	ResultRef       string `json:"result_ref"`
	RelationshipKey string `json:"relationship_key"`
	AgentKey        string `json:"agent_key"`
	CreatedAt       string `json:"created_at"`
	Result          string `json:"result"`
}

// SubagentStore returns the persistent child-run state machine.
//
// It is durable rather than process-local because delegation no longer completes
// inside one tool call: a parent parks while its child runs, so the
// relationship, the tree budget it reserved, and the wake intent that resumes
// the parent all have to survive a restart. The safety limits themselves still
// come from the subagent package's exported state machine - see
// SQLiteSubagentStore.
func (s *Store) SubagentStore() subagent.Store { return NewSQLiteSubagentStore(s.db) }

var delegationMigrations = []string{
	`CREATE TABLE IF NOT EXISTS subagent_results (
		result_ref TEXT PRIMARY KEY,
		relationship_key TEXT NOT NULL,
		agent_key TEXT NOT NULL,
		content TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS subagent_results_by_agent ON subagent_results(agent_key, created_at)`,
}

// SaveDelegationResult stores a child's answer under its result reference.
//
// The reference is content-addressed, so a replayed child that produces the same
// answer writes the same row rather than a duplicate.
func (s *Store) SaveDelegationResult(ctx context.Context, resultRef, relationshipKey, agentKey, content string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO subagent_results(result_ref, relationship_key, agent_key, content, created_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(result_ref) DO UPDATE SET relationship_key = excluded.relationship_key, content = excluded.content`,
		resultRef, relationshipKey, agentKey, content, time.Now().UTC().UnixNano())
	return err
}

// LoadDelegationResult reads back one child's answer.
func (s *Store) LoadDelegationResult(ctx context.Context, resultRef string) (string, bool, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM subagent_results WHERE result_ref = ?`, resultRef).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// ListDelegations returns recorded child runs, newest first.
func (s *Store) ListDelegations(ctx context.Context, limit int) (delegations []Delegation, resultErr error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT result_ref, relationship_key, agent_key, content, created_at FROM subagent_results ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var item Delegation
		var createdAt int64
		if err := rows.Scan(&item.ResultRef, &item.RelationshipKey, &item.AgentKey, &item.Result, &createdAt); err != nil {
			return nil, err
		}
		item.CreatedAt = fromNanos(createdAt).Format(time.RFC3339)
		delegations = append(delegations, item)
	}
	return delegations, rows.Err()
}

// Delegations returns recorded child runs for inspection.
func (a *App) Delegations(ctx context.Context, limit int) ([]Delegation, error) {
	return a.store.ListDelegations(ctx, limit)
}
