package session

import (
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

const StepPolicySchemaV1 = "session.step-policy/v1"

// StepPolicyArtifact is the immutable, portable execution policy for one run.
// It contains values only so stores can reconstruct the exact model request sequence.
type StepPolicyArtifact struct {
	Schema string           `json:"schema"`
	Steps  []StepPolicyStep `json:"steps"`
}

type StepPolicyStep struct {
	ActiveTools []string         `json:"active_tools"`
	ToolChoice  agent.ToolChoice `json:"tool_choice"`
}

func NewStepPolicyArtifact(steps ...StepPolicyStep) (StepPolicyArtifact, error) {
	artifact := StepPolicyArtifact{Schema: StepPolicySchemaV1, Steps: cloneStepPolicySteps(steps)}
	if err := artifact.Validate(); err != nil {
		return StepPolicyArtifact{}, err
	}
	return artifact, nil
}

func (p StepPolicyArtifact) Validate() error {
	if p.Schema != StepPolicySchemaV1 || len(p.Steps) == 0 {
		return fmt.Errorf("%w: incomplete step policy artifact", ErrInvalidCommand)
	}
	for index, step := range p.Steps {
		choice := step.ToolChoice
		switch choice.Mode {
		case agent.ToolChoiceNamed:
			if choice.Name == "" || len(step.ActiveTools) != 1 || step.ActiveTools[0] != choice.Name {
				return fmt.Errorf("%w: invalid named choice at policy step %d", ErrInvalidCommand, index)
			}
		case agent.ToolChoiceNone:
			if choice.Name != "" || len(step.ActiveTools) != 0 {
				return fmt.Errorf("%w: invalid none choice at policy step %d", ErrInvalidCommand, index)
			}
		default:
			return fmt.Errorf("%w: non-deterministic choice at policy step %d", ErrInvalidCommand, index)
		}
	}
	return nil
}

func (p StepPolicyArtifact) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	return agent.CanonicalDigest(p)
}

func (p StepPolicyArtifact) Compile() (agent.StepPolicy, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	steps := cloneStepPolicySteps(p.Steps)
	return func(input agent.StepPolicyInput) (agent.StepPolicyDecision, error) {
		index := len(input.CompletedSteps)
		if index >= len(steps) {
			return agent.StepPolicyDecision{}, fmt.Errorf("policy exhausted after %d steps", len(steps))
		}
		step := steps[index]
		choice := step.ToolChoice
		return agent.StepPolicyDecision{ActiveTools: append([]string{}, step.ActiveTools...), ToolChoice: &choice}, nil
	}, nil
}

func cloneStepPolicy(value StepPolicyArtifact) StepPolicyArtifact {
	value.Steps = cloneStepPolicySteps(value.Steps)
	return value
}

func cloneStepPolicySteps(steps []StepPolicyStep) []StepPolicyStep {
	result := make([]StepPolicyStep, len(steps))
	for index := range steps {
		result[index] = steps[index]
		result[index].ActiveTools = append([]string{}, steps[index].ActiveTools...)
	}
	return result
}
