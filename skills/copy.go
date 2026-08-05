package skills

func cloneDescriptor(value Descriptor) Descriptor {
	value.Metadata = cloneMap(value.Metadata)
	value.ToolRequirements = append([]string(nil), value.ToolRequirements...)
	value.Artifacts = append([]ArtifactDescriptor(nil), value.Artifacts...)
	value.Resources = append([]ResourceDescriptor(nil), value.Resources...)
	if value.Replaces != nil {
		replacement := *value.Replaces
		value.Replaces = &replacement
	}
	return value
}

func cloneMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for k, v := range value {
		result[k] = v
	}
	return result
}
func cloneBodies(value map[string][]byte) map[string][]byte {
	if value == nil {
		return nil
	}
	result := make(map[string][]byte, len(value))
	for k, v := range value {
		result[k] = append([]byte(nil), v...)
	}
	return result
}
func cloneSkill(value SourceSkill) SourceSkill {
	return SourceSkill{Descriptor: cloneDescriptor(value.Descriptor), Instructions: append([]byte(nil), value.Instructions...), Artifacts: cloneBodies(value.Artifacts), Disabled: value.Disabled, readInstructions: value.readInstructions, readArtifact: value.readArtifact}
}
func cloneSnapshot(value Snapshot) Snapshot {
	value.SourceGenerations = cloneMap(value.SourceGenerations)
	value.Descriptors = cloneDescriptors(value.Descriptors)
	value.Diagnostics = append([]Diagnostic(nil), value.Diagnostics...)
	return value
}
func cloneDescriptors(value []Descriptor) []Descriptor {
	if value == nil {
		return nil
	}
	out := make([]Descriptor, len(value))
	for i := range value {
		out[i] = cloneDescriptor(value[i])
	}
	return out
}
