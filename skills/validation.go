package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

var (
	namePattern      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	semverPattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
	hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func digestBytes(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

func validateAndDigest(skill SourceSkill, source SourceRef, limits Limits) (SourceSkill, error) {
	d := cloneDescriptor(skill.Descriptor)
	if d.Key == "" {
		d.Key = SkillKey(d.Name)
	}
	if len(d.Name) < 1 || len(d.Name) > 64 || !namePattern.MatchString(d.Name) || d.Key != SkillKey(d.Name) {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate", ErrDescriptorInvalid)
	}
	if !semverPattern.MatchString(string(d.Version)) {
		return SourceSkill{}, skillError(CodeVersionInvalid, "validate", ErrVersionInvalid)
	}
	if d.SchemaVersion != CurrentSchemaVersion {
		return SourceSkill{}, skillError(CodeSchemaUnsupported, "validate", ErrSchemaUnsupported)
	}
	if d.Description == "" || len(d.Description) > limits.MaxDescriptionBytes || !utf8.ValidString(d.Description) || len(d.Compatibility) > limits.MaxCompatibilityBytes || !utf8.ValidString(d.Compatibility) || len(d.License) > 191 || !utf8.ValidString(d.License) || len(d.Publisher) > 128 || !utf8.ValidString(d.Publisher) || len(d.ReviewVersion) > 64 || !utf8.ValidString(d.ReviewVersion) {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate", ErrDescriptorInvalid)
	}
	if len(skill.Instructions) == 0 || len(skill.Instructions) > limits.MaxInstructionsBytes || !utf8.Valid(skill.Instructions) || bytesLines(skill.Instructions) > limits.MaxInstructionLines {
		cause := ErrDescriptorInvalid
		code := CodeDescriptorInvalid
		if len(skill.Instructions) > limits.MaxInstructionsBytes {
			cause = ErrResourceTooLarge
			code = CodeResourceTooLarge
		}
		return SourceSkill{}, skillError(code, "validate_instructions", cause)
	}
	if len(d.Metadata) > limits.MaxMetadataEntries {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_metadata", ErrDescriptorInvalid)
	}
	metadataBytes := 0
	for key, value := range d.Metadata {
		metadataBytes += len(key) + len(value)
		if key == "" || len(key) > 64 || len(value) > 512 || !utf8.ValidString(key) || !utf8.ValidString(value) {
			return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_metadata", ErrDescriptorInvalid)
		}
	}
	if metadataBytes > limits.MaxMetadataBytes {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_metadata", ErrDescriptorInvalid)
	}
	if d.Trust == "" {
		switch source.Kind {
		case SourcePlatformFilesystem:
			d.Trust = TrustPlatform
		case SourceTenantFilesystem:
			d.Trust = TrustTenantReviewed
		default:
			d.Trust = TrustTenantUnreviewed
		}
	}
	if d.Trust != TrustPlatform && d.Trust != TrustTenantReviewed && d.Trust != TrustTenantUnreviewed {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_trust", ErrDescriptorInvalid)
	}
	if source.Kind != SourcePlatformFilesystem && d.Trust == TrustPlatform {
		return SourceSkill{}, skillError(CodeTrustDenied, "validate_trust", ErrTrustDenied)
	}
	if d.Replaces != nil && (d.Replaces.Key == "" || d.Replaces.Key != d.Key || !validVersionRange(d.Replaces.VersionRange)) {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_replacement", ErrDescriptorInvalid)
	}
	seenTools := make(map[string]struct{}, len(d.ToolRequirements))
	for _, requirement := range d.ToolRequirements {
		if !validToolRequirement(requirement) {
			return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_tool_requirement", ErrDescriptorInvalid)
		}
		if _, duplicate := seenTools[requirement]; duplicate {
			return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_tool_requirement", ErrDescriptorInvalid)
		}
		seenTools[requirement] = struct{}{}
	}
	sort.Strings(d.ToolRequirements)
	artifacts := mergedArtifacts(d)
	if len(artifacts) > limits.MaxArtifacts {
		return SourceSkill{}, skillError(CodeResourceTooLarge, "validate_artifacts", ErrResourceTooLarge)
	}
	seen := make(map[string]struct{}, len(artifacts))
	for i := range artifacts {
		a := &artifacts[i]
		if !validArtifactKey(a.Key) || a.Key == InstructionsKey {
			return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_artifact", ErrDescriptorInvalid)
		}
		if _, ok := seen[a.Key]; ok {
			return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_artifact", ErrDescriptorInvalid)
		}
		seen[a.Key] = struct{}{}
		body, ok := skill.Artifacts[a.Key]
		if !ok {
			return SourceSkill{}, skillError(CodeResourceNotFound, "validate_artifact", ErrResourceNotFound)
		}
		if int64(len(body)) > limits.MaxArtifactBytes || int64(len(body)) != a.SizeBytes {
			return SourceSkill{}, skillError(CodeResourceTooLarge, "validate_artifact", ErrResourceTooLarge)
		}
		if a.MIMEType == "" || !hexDigestPattern.MatchString(a.Digest) || digestBytes(body) != a.Digest {
			return SourceSkill{}, skillError(CodeResourceChanged, "validate_artifact", ErrResourceChanged)
		}
	}
	if len(skill.Artifacts) != len(seen) {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_artifact", ErrDescriptorInvalid)
	}
	for key := range skill.Artifacts {
		if _, declared := seen[key]; !declared {
			return SourceSkill{}, skillError(CodeDescriptorInvalid, "validate_artifact", ErrDescriptorInvalid)
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Key < artifacts[j].Key })
	d.Artifacts = artifacts
	d.Resources = nil
	d.Source = source
	d.InstructionsDigest = digestBytes(skill.Instructions)
	canonical := d
	canonical.Source = SourceRef{Kind: source.Kind, Name: source.Name}
	canonical.DescriptorDigest = ""
	canonical.InstructionsDigest = ""
	canonical.ContentDigest = ""
	descriptorBytes, err := jsoncodec.Marshal(canonical)
	if err != nil {
		return SourceSkill{}, fmt.Errorf("digest descriptor: %w", err)
	}
	d.DescriptorDigest = digestBytes(descriptorBytes)
	manifestBytes, err := jsoncodec.Marshal(artifacts)
	if err != nil {
		return SourceSkill{}, fmt.Errorf("digest content: %w", err)
	}
	content := make([]byte, 0, len(descriptorBytes)+len(skill.Instructions)+len(manifestBytes)+2)
	content = append(content, descriptorBytes...)
	content = append(content, 0)
	content = append(content, skill.Instructions...)
	content = append(content, 0)
	content = append(content, manifestBytes...)
	d.ContentDigest = digestBytes(content)
	return SourceSkill{Descriptor: d, Instructions: append([]byte(nil), skill.Instructions...), Artifacts: cloneBodies(skill.Artifacts), readInstructions: skill.readInstructions, readArtifact: skill.readArtifact}, nil
}

func validVersionRange(value string) bool {
	if value == "" || value == "*" || semverPattern.MatchString(value) {
		return true
	}
	for _, constraint := range strings.Fields(value) {
		version := strings.TrimLeft(constraint, "<>=")
		operator := strings.TrimSuffix(constraint, version)
		if (operator != "=" && operator != ">" && operator != ">=" && operator != "<" && operator != "<=") || !semverPattern.MatchString(version) {
			return false
		}
	}
	return len(strings.Fields(value)) > 0
}
func replacementMatches(r *Replacement, lower Descriptor) bool {
	if r == nil || r.Key != lower.Key || !validVersionRange(r.VersionRange) {
		return false
	}
	if r.VersionRange == "" || r.VersionRange == "*" {
		return true
	}
	if semverPattern.MatchString(r.VersionRange) {
		return r.VersionRange == string(lower.Version)
	}
	for _, constraint := range strings.Fields(r.VersionRange) {
		version := strings.TrimLeft(constraint, "<>=")
		operator := strings.TrimSuffix(constraint, version)
		comparison := compareSemver(string(lower.Version), version)
		if operator == "=" && comparison != 0 || operator == ">" && comparison <= 0 || operator == ">=" && comparison < 0 || operator == "<" && comparison >= 0 || operator == "<=" && comparison > 0 {
			return false
		}
	}
	return true
}

type semanticVersion struct {
	major, minor, patch int
	prerelease          []string
}

func compareSemver(left, right string) int {
	a := parseSemver(left)
	b := parseSemver(right)
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) > 0 {
		return 1
	}
	if len(b.prerelease) == 0 && len(a.prerelease) > 0 {
		return -1
	}
	for index := 0; index < len(a.prerelease) && index < len(b.prerelease); index++ {
		leftPart, rightPart := a.prerelease[index], b.prerelease[index]
		leftNumber, leftErr := strconv.Atoi(leftPart)
		rightNumber, rightErr := strconv.Atoi(rightPart)
		if leftErr == nil && rightErr == nil {
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
			continue
		}
		if leftErr == nil {
			return -1
		}
		if rightErr == nil {
			return 1
		}
		if leftPart < rightPart {
			return -1
		}
		if leftPart > rightPart {
			return 1
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1
	}
	return 0
}

func parseSemver(value string) semanticVersion {
	core := strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(core, "-", 2)
	numbers := strings.Split(parts[0], ".")
	major, _ := strconv.Atoi(numbers[0])
	minor, _ := strconv.Atoi(numbers[1])
	patch, _ := strconv.Atoi(numbers[2])
	result := semanticVersion{major: major, minor: minor, patch: patch}
	if len(parts) == 2 {
		result.prerelease = strings.Split(parts[1], ".")
	}
	return result
}
func validArtifactKey(v string) bool {
	return len(v) > 0 && len(v) <= 128 && namePattern.MatchString(v)
}
func validToolRequirement(value string) bool {
	if len(value) < 1 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r == '/' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}
func bytesLines(v []byte) int {
	if len(v) == 0 {
		return 0
	}
	return strings.Count(string(v), "\n") + 1
}
func mergedArtifacts(d Descriptor) []ArtifactDescriptor {
	if len(d.Artifacts) > 0 && len(d.Resources) > 0 {
		return append(append([]ArtifactDescriptor(nil), d.Artifacts...), d.Resources...)
	}
	if len(d.Artifacts) > 0 {
		return append([]ArtifactDescriptor(nil), d.Artifacts...)
	}
	return append([]ArtifactDescriptor(nil), d.Resources...)
}
func descriptorByArtifact(d Descriptor, key string) (ArtifactDescriptor, bool) {
	for _, a := range d.Artifacts {
		if a.Key == key {
			return a, true
		}
	}
	return ArtifactDescriptor{}, false
}
