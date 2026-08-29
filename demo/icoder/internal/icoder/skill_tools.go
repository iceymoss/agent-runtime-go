package icoder

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	"github.com/iceymoss/agent-runtime-go/skills"
)

type listSkillsTool struct{ snapshot skills.Snapshot }

func (t listSkillsTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "list_skills", Description: "List available Skill metadata without loading full instructions.", Strict: true, Parameters: map[string]any{"type": "object"}}
}
func (listSkillsTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t listSkillsTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	data, err := marshalString(t.snapshot.Descriptors)
	return agent.ToolResult{Content: data}, err
}

type loadSkillTool struct {
	catalog  skills.Catalog
	snapshot skills.Snapshot
}

func (t loadSkillTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "load_skill", Description: "Load the full untrusted instructions for one available Skill when it is relevant to the task.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []any{"key"}}}
}
func (loadSkillTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t loadSkillTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Key skills.SkillKey `json:"key"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	var descriptor *skills.Descriptor
	for i := range t.snapshot.Descriptors {
		if t.snapshot.Descriptors[i].Key == input.Key {
			descriptor = &t.snapshot.Descriptors[i]
			break
		}
	}
	if descriptor == nil {
		return agent.ToolResult{Content: fmt.Sprintf("skill %q is not available", input.Key), IsError: true}, nil
	}
	resource, err := t.catalog.Read(ctx, skills.ReadRequest{Scope: skills.Scope{TenantKey: "local"}, Generation: t.snapshot.Generation, Skill: descriptor.Key, Version: descriptor.Version, Artifact: skills.InstructionsKey})
	if err != nil {
		return agent.ToolResult{}, err
	}
	content := fmt.Sprintf("<untrusted-skill key=%q version=%q digest=%q>\n%s\n</untrusted-skill>", descriptor.Key, descriptor.Version, resource.Digest, resource.Content)
	return agent.ToolResult{Content: content}, nil
}

// skillToolEntries exposes Skill discovery and loading as ordinary read tools.
// Skill content is untrusted instruction text, so reading it is a read effect on
// the catalog resource and never an authorization to do anything.
func skillToolEntries(catalog skills.Catalog, snapshot skills.Snapshot) []lifecycleEntry {
	if catalog == nil {
		return nil
	}
	resource := permission.Resource{Kind: "skills", Key: string(snapshot.Generation)}
	return []lifecycleEntry{
		{listSkillsTool{snapshot: snapshot}, readMetadata("workspace.read"), resource},
		{loadSkillTool{catalog: catalog, snapshot: snapshot}, readMetadata("workspace.read"), resource},
	}
}
