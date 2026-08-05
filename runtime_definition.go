package agent

import "fmt"

// ModelMetadata is the resolved, immutable model identity used by a definition.
type ModelMetadata struct {
	Name          string       `json:"name"`
	Version       string       `json:"version"`
	ContextWindow int          `json:"context_window,omitempty"`
	Capabilities  Capabilities `json:"capabilities"`
}

// ExecutionSettings contains definition-level runtime behavior.
type ExecutionSettings struct {
	MaxSteps            int             `json:"max_steps"`
	LoopDetectWindow    int             `json:"loop_detect_window,omitempty"`
	LoopDetectThreshold int             `json:"loop_detect_threshold,omitempty"`
	ToolRepairLimit     int             `json:"tool_repair_limit,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	ToolChoice          *ToolChoice     `json:"tool_choice,omitempty"`
	StopConditions      []StopCondition `json:"-"`
}

// ArtifactVersions identifies executable or external artifacts captured by a definition.
type ArtifactVersions struct {
	Definition string `json:"definition"`
	Model      string `json:"model"`
	Tools      string `json:"tools"`
	Prompt     string `json:"prompt"`
	Policy     string `json:"policy"`
}

// RuntimeDefinitionSpec is the validated value input to an immutable definition.
type RuntimeDefinitionSpec struct {
	Key           string            `json:"key"`
	Model         ModelMetadata     `json:"model"`
	Execution     ExecutionSettings `json:"execution"`
	PromptVersion string            `json:"prompt_version"`
	PolicyVersion string            `json:"policy_version"`
}

// RuntimeDefinition is a complete immutable runtime composition artifact.
type RuntimeDefinition struct {
	spec     RuntimeDefinitionSpec
	model    Model
	tools    *ToolSet
	versions ArtifactVersions
}

// NewRuntimeDefinition validates and snapshots a complete runtime composition.
func NewRuntimeDefinition(spec RuntimeDefinitionSpec, model Model, tools *ToolSet) (*RuntimeDefinition, error) {
	if spec.Key == "" || spec.Model.Name == "" || spec.Model.Version == "" || spec.PromptVersion == "" || spec.PolicyVersion == "" || model == nil || tools == nil {
		return nil, fmt.Errorf("%w: incomplete runtime definition", ErrAgentConfigInvalid)
	}
	if spec.Model.ContextWindow < 0 {
		return nil, fmt.Errorf("%w: negative context window", ErrAgentConfigInvalid)
	}
	if err := spec.Model.Capabilities.Validate(); err != nil {
		return nil, fmt.Errorf("%w: model capabilities: %w", ErrAgentConfigInvalid, err)
	}
	if model.Capabilities() != spec.Model.Capabilities {
		return nil, fmt.Errorf("%w: model capabilities differ from metadata", ErrAgentConfigInvalid)
	}
	spec.Execution = cloneExecutionSettings(spec.Execution)
	cfg := configFromDefinition(spec)
	if _, err := newAgent(cfg, model, tools, spec.Model.Capabilities); err != nil {
		return nil, err
	}
	versions := ArtifactVersions{Model: spec.Model.Version, Tools: tools.Version(), Prompt: spec.PromptVersion, Policy: spec.PolicyVersion}
	digest, err := CanonicalDigest(struct {
		Spec     RuntimeDefinitionSpec `json:"spec"`
		Versions ArtifactVersions      `json:"versions"`
	}{Spec: spec, Versions: versions})
	if err != nil {
		return nil, fmt.Errorf("%w: digest runtime definition: %w", ErrAgentConfigInvalid, err)
	}
	versions.Definition = digest
	return &RuntimeDefinition{spec: spec, model: model, tools: tools, versions: versions}, nil
}

func (d *RuntimeDefinition) Key() string { return d.spec.Key }

func (d *RuntimeDefinition) ModelMetadata() ModelMetadata { return d.spec.Model }

func (d *RuntimeDefinition) ExecutionSettings() ExecutionSettings {
	return cloneExecutionSettings(d.spec.Execution)
}

func (d *RuntimeDefinition) ArtifactVersions() ArtifactVersions { return d.versions }

func (d *RuntimeDefinition) ToolDefinitions() []ToolDefinition { return d.tools.Definitions() }

// NewAgent creates a stateless runner that retains this definition's snapshots.
func (d *RuntimeDefinition) NewAgent() (*Agent, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: nil runtime definition", ErrAgentConfigInvalid)
	}
	return newAgent(configFromDefinition(d.spec), d.model, d.tools, d.spec.Model.Capabilities)
}

func configFromDefinition(spec RuntimeDefinitionSpec) Config {
	settings := spec.Execution
	return Config{
		Key: spec.Key, ModelName: spec.Model.Name, MaxSteps: settings.MaxSteps,
		LoopDetectWindow: settings.LoopDetectWindow, LoopDetectThreshold: settings.LoopDetectThreshold,
		ContextWindow: spec.Model.ContextWindow, Temperature: settings.Temperature, MaxTokens: settings.MaxTokens,
		TopP:           settings.TopP,
		StopConditions: settings.StopConditions, ToolChoice: settings.ToolChoice, ToolRepairLimit: settings.ToolRepairLimit,
	}
}

func cloneExecutionSettings(settings ExecutionSettings) ExecutionSettings {
	cloned := settings
	cloned.StopConditions = append([]StopCondition(nil), settings.StopConditions...)
	if settings.Temperature != nil {
		value := *settings.Temperature
		cloned.Temperature = &value
	}
	if settings.MaxTokens != nil {
		value := *settings.MaxTokens
		cloned.MaxTokens = &value
	}
	if settings.TopP != nil {
		value := *settings.TopP
		cloned.TopP = &value
	}
	if settings.ToolChoice != nil {
		choice := *settings.ToolChoice
		cloned.ToolChoice = &choice
	}
	return cloned
}
