package coordinator

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

func NewArtifactManifest(tenant agent.TenantKey, wire ManifestWire, createdAt time.Time) (ArtifactManifest, error) {
	wire = canonicalWire(wire)
	if err := validateWire(wire); err != nil {
		return ArtifactManifest{}, err
	}
	if !tenant.Valid() || createdAt.IsZero() {
		return ArtifactManifest{}, manifestError("new_manifest", wire.DefinitionDigest, "tenant and creation time are required", nil)
	}
	digest, err := agent.CanonicalDigest(wire)
	if err != nil {
		return ArtifactManifest{}, manifestError("new_manifest", wire.DefinitionDigest, "digest wire manifest", err)
	}
	return ArtifactManifest{TenantKey: tenant, Wire: wire, ManifestDigest: digest, CreatedAt: createdAt}, nil
}

func ValidateArtifactManifest(manifest ArtifactManifest) error {
	if !manifest.TenantKey.Valid() || manifest.CreatedAt.IsZero() {
		return manifestError("validate_manifest", manifest.Wire.DefinitionDigest, "tenant and creation time are required", nil)
	}
	if err := validateWire(manifest.Wire); err != nil {
		return err
	}
	digest, err := agent.CanonicalDigest(manifest.Wire)
	if err != nil {
		return manifestError("validate_manifest", manifest.Wire.DefinitionDigest, "digest wire manifest", err)
	}
	if manifest.ManifestDigest == "" || digest != manifest.ManifestDigest {
		return manifestError("validate_manifest", manifest.Wire.DefinitionDigest, "manifest digest mismatch", nil)
	}
	return nil
}

func validateWire(wire ManifestWire) error {
	if wire.SchemaVersion != CurrentSchemaVersion {
		return manifestError("validate_wire", wire.DefinitionDigest, "unsupported schema version", nil)
	}
	if strings.TrimSpace(wire.DefinitionDigest) == "" || strings.TrimSpace(wire.RecipeKey) == "" || strings.TrimSpace(wire.RecipeVersion) == "" || wire.Provider == "" || wire.Model == "" || strings.TrimSpace(wire.ModelVersion) == "" || strings.TrimSpace(wire.CatalogGeneration) == "" || strings.TrimSpace(wire.OptionsVersion) == "" {
		return manifestError("validate_wire", wire.DefinitionDigest, "required identity is empty", nil)
	}
	if wire.Role == "" {
		wire.Role = "primary"
	}
	if wire.Role != "primary" && wire.Role != "utility" && wire.Role != "summary" && wire.Role != "title" {
		return manifestError("validate_wire", wire.DefinitionDigest, "invalid model role", nil)
	}
	if len(wire.Execution.StopConditions) != 0 {
		return manifestError("validate_wire", wire.DefinitionDigest, "executable stop conditions cannot enter a wire manifest", nil)
	}
	if err := validateOptions(wire.Options); err != nil {
		return manifestError("validate_wire", wire.DefinitionDigest, "invalid generation options", err)
	}
	if len(wire.RootToolNames) != 0 && !wire.RootToolsDeclared {
		return manifestError("validate_wire", wire.DefinitionDigest, "root tool names require a declaration", nil)
	}
	for index, name := range wire.RootToolNames {
		if strings.TrimSpace(name) == "" || index > 0 && wire.RootToolNames[index-1] >= name {
			return manifestError("validate_wire", wire.DefinitionDigest, "root tool names are not canonical", nil)
		}
	}
	seen := make(map[string]struct{}, len(wire.Artifacts))
	previous := ""
	for _, ref := range wire.Artifacts {
		key := artifactKey(ref)
		if ref.Kind == "" || ref.Key == "" || ref.Generation == "" || ref.Digest == "" || ref.SchemaVersion == 0 {
			return manifestError("validate_wire", wire.DefinitionDigest, "incomplete artifact reference", nil)
		}
		if _, exists := seen[key]; exists {
			return manifestError("validate_wire", wire.DefinitionDigest, "duplicate artifact reference", nil)
		}
		if previous != "" && key < previous {
			return manifestError("validate_wire", wire.DefinitionDigest, "artifact references are not canonical", nil)
		}
		seen[key] = struct{}{}
		previous = key
	}
	return nil
}

func canonicalWire(wire ManifestWire) ManifestWire {
	wire = cloneWire(wire)
	if wire.Role == "" {
		wire.Role = "primary"
	}
	sort.Slice(wire.Artifacts, func(i, j int) bool { return artifactKey(wire.Artifacts[i]) < artifactKey(wire.Artifacts[j]) })
	return wire
}

func artifactKey(ref ArtifactRef) string {
	return ref.Kind + "\x00" + ref.Key + "\x00" + ref.Generation + "\x00" + ref.Digest + fmt.Sprintf("\x00%05d", ref.SchemaVersion)
}

func validateOptions(options agent.GenerationOptions) error {
	if options.Temperature != nil && (*options.Temperature < 0 || *options.Temperature > 2) {
		return fmt.Errorf("temperature is out of range")
	}
	if options.MaxTokens != nil && *options.MaxTokens <= 0 {
		return fmt.Errorf("max tokens must be positive")
	}
	if options.TopP != nil && (*options.TopP < 0 || *options.TopP > 1) {
		return fmt.Errorf("top_p is out of range")
	}
	return nil
}

func cloneManifest(value ArtifactManifest) ArtifactManifest {
	value.Wire = cloneWire(value.Wire)
	return value
}

func cloneWire(value ManifestWire) ManifestWire {
	value.PromptMessages = cloneMessages(value.PromptMessages)
	value.Execution = cloneExecution(value.Execution)
	value.Options = cloneOptions(value.Options)
	value.RootToolNames = append([]string(nil), value.RootToolNames...)
	value.Artifacts = append([]ArtifactRef(nil), value.Artifacts...)
	return value
}

func cloneMessages(value []agent.Message) []agent.Message {
	if value == nil {
		return nil
	}
	result := make([]agent.Message, len(value))
	for i, message := range value {
		result[i] = message
		result[i].Parts = make([]agent.ContentPart, len(message.Parts))
		for j, part := range message.Parts {
			result[i].Parts[j] = part
			if part.ToolCall != nil {
				v := *part.ToolCall
				result[i].Parts[j].ToolCall = &v
			}
			if part.ToolResult != nil {
				v := *part.ToolResult
				result[i].Parts[j].ToolResult = &v
			}
			if part.Image != nil {
				v := *part.Image
				v.Data = append([]byte(nil), part.Image.Data...)
				result[i].Parts[j].Image = &v
			}
		}
	}
	return result
}

func cloneExecution(value agent.ExecutionSettings) agent.ExecutionSettings {
	value.StopConditions = append([]agent.StopCondition(nil), value.StopConditions...)
	if value.Temperature != nil {
		v := *value.Temperature
		value.Temperature = &v
	}
	if value.MaxTokens != nil {
		v := *value.MaxTokens
		value.MaxTokens = &v
	}
	if value.TopP != nil {
		v := *value.TopP
		value.TopP = &v
	}
	if value.ToolChoice != nil {
		v := *value.ToolChoice
		value.ToolChoice = &v
	}
	return value
}

func cloneOptions(value agent.GenerationOptions) agent.GenerationOptions {
	if value.Temperature != nil {
		v := *value.Temperature
		value.Temperature = &v
	}
	if value.MaxTokens != nil {
		v := *value.MaxTokens
		value.MaxTokens = &v
	}
	if value.TopP != nil {
		v := *value.TopP
		value.TopP = &v
	}
	return value
}

func sameArtifacts(left, right []ArtifactRef) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]ArtifactRef(nil), left...)
	right = append([]ArtifactRef(nil), right...)
	sort.Slice(left, func(i, j int) bool { return artifactKey(left[i]) < artifactKey(left[j]) })
	sort.Slice(right, func(i, j int) bool { return artifactKey(right[i]) < artifactKey(right[j]) })
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func manifestError(operation, generation, detail string, cause error) error {
	base := ErrManifestInvalid
	if cause != nil {
		base = errors.Join(base, cause)
	}
	if detail != "" {
		base = fmt.Errorf("%w: %s", base, detail)
	}
	return &Error{Code: CodeManifestInvalid, Operation: operation, Generation: generation, Cause: base}
}
