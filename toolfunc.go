package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// FuncTool is a Tool built from a typed Go function by NewTool. The input
// schema is generated from T and the raw JSON input is decoded into T before
// the function runs.
type FuncTool[T any] struct {
	definition        ToolDefinition
	replayPolicy      ReplayPolicy
	executableVersion string
	fn                func(ctx context.Context, input T) (ToolResult, error)
}

// ToolOption customizes a FuncTool created by NewTool.
type ToolOption func(*toolOptions)

type toolOptions struct {
	replayPolicy      ReplayPolicy
	executableVersion string
	strict            bool
}

// WithToolReplayPolicy sets the replay policy. The default is
// ReplayPolicyNever, the safest choice for tools with side effects.
func WithToolReplayPolicy(policy ReplayPolicy) ToolOption {
	return func(o *toolOptions) { o.replayPolicy = policy }
}

// WithToolExecutableVersion declares the implementation version used by
// durable artifact digests. Tools that never participate in durable exact
// restoration can omit it.
func WithToolExecutableVersion(version string) ToolOption {
	return func(o *toolOptions) { o.executableVersion = version }
}

// WithoutStrictSchema disables the strict schema default, allowing the model
// to send properties that are not declared on T.
func WithoutStrictSchema() ToolOption {
	return func(o *toolOptions) { o.strict = false }
}

// NewTool builds a Tool from a typed Go function. T must be a struct (or
// pointer to struct); its exported fields become the JSON Schema sent to the
// model:
//
//   - property names come from `json` tags (falling back to the field name)
//   - fields are required unless they are pointers or tagged omitempty/omitzero
//   - a `description` tag becomes the property description
//   - nested structs, slices, and string-keyed maps are supported
//
// The runtime validates raw input against the generated schema before Execute
// runs, so fn receives well-formed input. The schema is strict by default:
// undeclared properties are rejected and fed back to the model for repair.
//
//	type WeatherInput struct {
//		City string `json:"city" description:"City name"`
//		Days int    `json:"days,omitempty" description:"Forecast days"`
//	}
//
//	tool, err := agent.NewTool("get_weather", "Get the weather for a city.",
//		func(ctx context.Context, input WeatherInput) (agent.ToolResult, error) {
//			return agent.ToolResult{Content: lookup(input.City)}, nil
//		})
func NewTool[T any](name, description string, fn func(ctx context.Context, input T) (ToolResult, error), opts ...ToolOption) (*FuncTool[T], error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: tool name is empty", ErrAgentConfigInvalid)
	}
	if fn == nil {
		return nil, fmt.Errorf("%w: tool %s function is nil", ErrAgentConfigInvalid, name)
	}
	options := toolOptions{replayPolicy: ReplayPolicyNever, strict: true}
	for _, opt := range opts {
		opt(&options)
	}
	inputType := reflect.TypeOf((*T)(nil)).Elem()
	for inputType.Kind() == reflect.Pointer {
		inputType = inputType.Elem()
	}
	if inputType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: tool %s input type %s is not a struct", ErrAgentConfigInvalid, name, inputType)
	}
	parameters, err := structSchema(inputType, map[reflect.Type]bool{})
	if err != nil {
		return nil, fmt.Errorf("%w: tool %s: %w", ErrAgentConfigInvalid, name, err)
	}
	return &FuncTool[T]{
		definition: ToolDefinition{
			Name:        name,
			Description: description,
			Parameters:  parameters,
			Strict:      options.strict,
		},
		replayPolicy:      options.replayPolicy,
		executableVersion: options.executableVersion,
		fn:                fn,
	}, nil
}

// MustNewTool is NewTool that panics on error, for assembly-time registration
// where a schema problem is a programming error.
func MustNewTool[T any](name, description string, fn func(ctx context.Context, input T) (ToolResult, error), opts ...ToolOption) *FuncTool[T] {
	tool, err := NewTool(name, description, fn, opts...)
	if err != nil {
		panic(err)
	}
	return tool
}

// Definition returns the generated model-visible declaration.
func (t *FuncTool[T]) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }

// ReplayPolicy returns the configured replay policy.
func (t *FuncTool[T]) ReplayPolicy() ReplayPolicy { return t.replayPolicy }

// ExecutableVersion returns the declared implementation version. It is empty
// unless WithToolExecutableVersion was used.
func (t *FuncTool[T]) ExecutableVersion() string { return t.executableVersion }

// Execute decodes the raw input into T and invokes the function. Decode
// failures are returned as model-correctable IsError results.
func (t *FuncTool[T]) Execute(ctx context.Context, invocation ToolInvocation) (ToolResult, error) {
	raw := invocation.RawInput
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var input T
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return ToolResult{
			ToolCallID: invocation.CallID,
			Name:       invocation.Name,
			Content:    fmt.Sprintf("input does not match the expected structure: %s. Reply with a valid JSON object that conforms to the tool's JSON Schema.", err),
			IsError:    true,
		}, nil
	}
	return t.fn(ctx, input)
}

var timeType = reflect.TypeOf(time.Time{})

func typeSchema(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}, nil
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Interface:
		// Any JSON value is accepted.
		return map[string]any{}, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// encoding/json represents []byte as a base64 string.
			return map[string]any{"type": "string"}, nil
		}
		items, err := typeSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key type %s is not supported; only string keys map to JSON objects", t.Key())
		}
		values, err := typeSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		return structSchema(t, seen)
	default:
		return nil, fmt.Errorf("field type %s is not supported", t)
	}
}

func structSchema(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	if seen[t] {
		return nil, fmt.Errorf("recursive type %s is not supported", t)
	}
	seen[t] = true
	defer delete(seen, t)

	properties := map[string]any{}
	required := []string{}
	if err := collectStructFields(t, seen, properties, &required); err != nil {
		return nil, err
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = toAnySlice(required)
	}
	return schema, nil
}

func collectStructFields(t reflect.Type, seen map[reflect.Type]bool, properties map[string]any, required *[]string) error {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		tagName, tagOptions, _ := strings.Cut(tag, ",")
		if field.Anonymous && tagName == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				if err := collectStructFields(embedded, seen, properties, required); err != nil {
					return err
				}
				continue
			}
		}
		name := tagName
		if name == "" {
			name = field.Name
		}
		fieldSchema, err := typeSchema(field.Type, seen)
		if err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}
		if description := field.Tag.Get("description"); description != "" {
			fieldSchema["description"] = description
		}
		properties[name] = fieldSchema
		optional := field.Type.Kind() == reflect.Pointer ||
			hasTagOption(tagOptions, "omitempty") ||
			hasTagOption(tagOptions, "omitzero")
		if !optional {
			*required = append(*required, name)
		}
	}
	return nil
}

func hasTagOption(options, option string) bool {
	for options != "" {
		var current string
		current, options, _ = strings.Cut(options, ",")
		if current == option {
			return true
		}
	}
	return false
}

func toAnySlice(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}
