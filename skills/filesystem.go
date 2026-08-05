package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/iceymoss/agent-runtime-go"

	"go.yaml.in/yaml/v3"
)

type Limits struct {
	MaxManifestBytes      int64
	MaxInstructionsBytes  int
	MaxInstructionLines   int
	MaxDescriptionBytes   int
	MaxCompatibilityBytes int
	MaxMetadataEntries    int
	MaxMetadataBytes      int
	MaxArtifacts          int
	MaxArtifactBytes      int64
}

func DefaultLimits() Limits {
	return Limits{MaxManifestBytes: 256 << 10, MaxInstructionsBytes: 256 << 10, MaxInstructionLines: 5000, MaxDescriptionBytes: 1024, MaxCompatibilityBytes: 500, MaxMetadataEntries: 32, MaxMetadataBytes: 8 << 10, MaxArtifacts: 64, MaxArtifactBytes: 4 << 20}
}

type FilesystemOptions struct {
	Name       string
	Kind       SourceKind
	Root       string
	TenantKey  agent.TenantKey
	Generation string
	Limits     Limits
}

type FilesystemSource struct {
	name       string
	kind       SourceKind
	root       string
	rootInfo   fs.FileInfo
	tenantKey  agent.TenantKey
	generation string
	limits     Limits
	initErr    error
}

func NewFilesystemSource(options FilesystemOptions) *FilesystemSource {
	s := &FilesystemSource{name: options.Name, kind: options.Kind, tenantKey: options.TenantKey, generation: options.Generation, limits: normalizeLimits(options.Limits)}
	if s.name == "" {
		s.name = string(s.kind)
	}
	if s.kind != SourcePlatformFilesystem && s.kind != SourceTenantFilesystem {
		s.initErr = ErrInvalidSource
		return s
	}
	if s.kind == SourceTenantFilesystem && !s.tenantKey.Valid() {
		s.initErr = ErrInvalidSource
		return s
	}
	abs, err := filepath.Abs(options.Root)
	if err != nil {
		s.initErr = ErrInvalidSource
		return s
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		s.initErr = ErrResourceEscape
		return s
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		s.initErr = ErrResourceEscape
		return s
	}
	realInfo, err := os.Lstat(real)
	if err != nil || !realInfo.IsDir() || realInfo.Mode()&os.ModeSymlink != 0 {
		s.initErr = ErrResourceEscape
		return s
	}
	s.root = filepath.Clean(real)
	s.rootInfo = realInfo
	return s
}

func (s *FilesystemSource) Name() string {
	if s == nil {
		return ""
	}
	return s.name
}
func (s *FilesystemSource) Kind() SourceKind {
	if s == nil {
		return ""
	}
	return s.kind
}

func (s *FilesystemSource) Load(ctx context.Context, scope Scope) (SourceSnapshot, error) {
	if s == nil || s.initErr != nil {
		cause := ErrInvalidSource
		if s != nil && s.initErr != nil {
			cause = s.initErr
		}
		return SourceSnapshot{}, &Error{Code: CodeInvalidSource, Operation: "filesystem_load", Source: s.Name(), Cause: errors.Join(ErrInvalidSource, cause)}
	}
	if s.kind == SourceTenantFilesystem && scope.TenantKey != s.tenantKey {
		return SourceSnapshot{}, &Error{Code: CodeInvalidSource, Operation: "filesystem_load", Source: s.name, Cause: ErrInvalidSource}
	}
	currentRoot, err := os.Lstat(s.root)
	if err != nil || currentRoot.Mode()&os.ModeSymlink != 0 || !currentRoot.IsDir() || !os.SameFile(s.rootInfo, currentRoot) {
		return SourceSnapshot{}, &Error{Code: CodeResourceEscape, Operation: "filesystem_load", Source: s.name, Cause: ErrResourceEscape}
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return SourceSnapshot{}, &Error{Code: CodeInvalidSource, Operation: "filesystem_load", Source: s.name, Cause: errors.Join(ErrInvalidSource, err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := SourceSnapshot{}
	collisions := make(map[string]bool)
	caseNames := make(map[string]string, len(entries))
	for _, entry := range entries {
		folded := strings.ToLower(entry.Name())
		if previous, exists := caseNames[folded]; exists && previous != entry.Name() {
			collisions[folded] = true
		}
		caseNames[folded] = entry.Name()
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return SourceSnapshot{}, err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(s.name, SkillKey(entry.Name()), CodeResourceEscape, "symlink skill directory rejected"))
			continue
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if collisions[strings.ToLower(name)] {
			result.Diagnostics = append(result.Diagnostics, diagnostic(s.name, SkillKey(name), CodeDuplicateSkill, "case-folding skill name collision"))
			continue
		}
		if !namePattern.MatchString(name) {
			result.Diagnostics = append(result.Diagnostics, diagnostic(s.name, SkillKey(name), CodeDescriptorInvalid, "invalid skill directory name"))
			continue
		}
		skill, loadErr := s.loadSkill(name)
		if loadErr != nil {
			result.Diagnostics = append(result.Diagnostics, diagnosticForError(s.name, SkillKey(name), loadErr))
			continue
		}
		result.Skills = append(result.Skills, skill)
	}
	var generation strings.Builder
	generation.WriteString(s.generation)
	for _, skill := range result.Skills {
		generation.WriteByte(0)
		generation.WriteString(string(skill.Descriptor.Key))
		generation.WriteByte(0)
		generation.WriteString(string(skill.Descriptor.Version))
		generation.WriteByte(0)
		generation.WriteString(skill.Descriptor.ContentDigest)
	}
	for _, item := range result.Diagnostics {
		generation.WriteByte(0)
		generation.WriteString(string(item.Skill))
		generation.WriteByte(0)
		generation.WriteString(string(item.Code))
	}
	result.Generation = digestBytes([]byte(generation.String()))
	for index := range result.Skills {
		result.Skills[index].Descriptor.Source.Generation = result.Generation
	}
	return result, nil
}

func (s *FilesystemSource) loadSkill(directory string) (SourceSkill, error) {
	skillDir := filepath.Join(s.root, directory)
	if err := verifyPath(s.root, skillDir, true); err != nil {
		return SourceSkill{}, err
	}
	manifestPath := filepath.Join(skillDir, "SKILL.md")
	body, err := readConfinedFile(s.root, manifestPath, s.limits.MaxManifestBytes)
	if err != nil {
		return SourceSkill{}, err
	}
	descriptor, instructions, err := parseManifest(body)
	if err != nil {
		return SourceSkill{}, err
	}
	if descriptor.Name != directory {
		return SourceSkill{}, skillError(CodeDescriptorInvalid, "filesystem_manifest", ErrDescriptorInvalid)
	}
	artifacts := mergedArtifacts(descriptor)
	bodies := make(map[string][]byte, len(artifacts))
	paths := make(map[string]string, len(artifacts))
	caseNames := make(map[string]string, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Path == "SKILL.md" || !validRelativePath(artifact.Path) {
			return SourceSkill{}, skillError(CodeResourceEscape, "filesystem_artifact", ErrResourceEscape)
		}
		folded := strings.ToLower(artifact.Path)
		if previous, ok := caseNames[folded]; ok && previous != artifact.Path {
			return SourceSkill{}, skillError(CodeResourceEscape, "filesystem_artifact", ErrResourceEscape)
		}
		caseNames[folded] = artifact.Path
		content, readErr := readConfinedFile(skillDir, filepath.Join(skillDir, filepath.FromSlash(artifact.Path)), s.limits.MaxArtifactBytes)
		if readErr != nil {
			return SourceSkill{}, readErr
		}
		bodies[artifact.Key] = content
		paths[artifact.Key] = filepath.Join(skillDir, filepath.FromSlash(artifact.Path))
	}
	generation := s.generation
	readInstructions := func() ([]byte, error) {
		current, readErr := readConfinedFile(s.root, manifestPath, s.limits.MaxManifestBytes)
		if readErr != nil {
			return nil, readErr
		}
		if digestBytes(current) != digestBytes(body) {
			return nil, skillError(CodeResourceChanged, "read_instructions", ErrResourceChanged)
		}
		_, currentInstructions, parseErr := parseManifest(current)
		if parseErr != nil {
			return nil, skillError(CodeResourceChanged, "read_instructions", ErrResourceChanged)
		}
		return currentInstructions, nil
	}
	readArtifact := func(key string) ([]byte, error) {
		path, exists := paths[key]
		if !exists {
			return nil, skillError(CodeResourceNotFound, "read_artifact", ErrResourceNotFound)
		}
		return readConfinedFile(skillDir, path, s.limits.MaxArtifactBytes)
	}
	return validateAndDigest(SourceSkill{Descriptor: descriptor, Instructions: instructions, Artifacts: bodies, readInstructions: readInstructions, readArtifact: readArtifact}, SourceRef{Name: s.name, Kind: s.kind, Generation: generation}, s.limits)
}

func parseManifest(body []byte) (Descriptor, []byte, error) {
	if !utf8.Valid(body) {
		return Descriptor{}, nil, skillError(CodeDescriptorInvalid, "parse_manifest", ErrDescriptorInvalid)
	}
	prefix := []byte("---\n")
	boundary := []byte("\n---\n")
	if bytes.HasPrefix(body, []byte("---\r\n")) {
		prefix = []byte("---\r\n")
		boundary = []byte("\r\n---\r\n")
	}
	if !bytes.HasPrefix(body, prefix) {
		return Descriptor{}, nil, skillError(CodeDescriptorInvalid, "parse_manifest", ErrDescriptorInvalid)
	}
	end := bytes.Index(body[len(prefix):], boundary)
	if end < 0 {
		return Descriptor{}, nil, skillError(CodeDescriptorInvalid, "parse_manifest", ErrDescriptorInvalid)
	}
	front := body[len(prefix) : len(prefix)+end]
	instructions := body[len(prefix)+end+len(boundary):]
	decoder := yaml.NewDecoder(bytes.NewReader(front))
	decoder.KnownFields(true)
	var descriptor Descriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, nil, skillError(CodeDescriptorInvalid, "parse_manifest", fmt.Errorf("%w: %v", ErrDescriptorInvalid, err))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Descriptor{}, nil, skillError(CodeDescriptorInvalid, "parse_manifest", ErrDescriptorInvalid)
	}
	return descriptor, instructions, nil
}

func readConfinedFile(root, path string, maximum int64) (body []byte, resultErr error) {
	if err := verifyPath(root, path, false); err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || !validRelativePath(filepath.ToSlash(relative)) {
		return nil, skillError(CodeResourceEscape, "read_file", ErrResourceEscape)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, skillError(CodeResourceNotFound, "read_file", ErrResourceNotFound)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || hardLinked(before) {
		return nil, skillError(CodeResourceEscape, "read_file", ErrResourceEscape)
	}
	if before.Size() > maximum {
		return nil, skillError(CodeResourceTooLarge, "read_file", ErrResourceTooLarge)
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return nil, skillError(CodeResourceEscape, "read_file", ErrResourceEscape)
	}
	defer func() {
		if closeErr := confined.Close(); closeErr != nil && resultErr == nil {
			resultErr = skillError(CodeResourceChanged, "close_root", ErrResourceChanged)
		}
	}()
	file, err := confined.Open(filepath.ToSlash(relative))
	if err != nil {
		return nil, skillError(CodeResourceEscape, "read_file", ErrResourceEscape)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && resultErr == nil {
			resultErr = skillError(CodeResourceChanged, "close_file", ErrResourceChanged)
		}
	}()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, skillError(CodeResourceChanged, "read_file", ErrResourceChanged)
	}
	limited := io.LimitReader(file, maximum+1)
	body, err = io.ReadAll(limited)
	if err != nil {
		return nil, skillError(CodeResourceChanged, "read_file", ErrResourceChanged)
	}
	if int64(len(body)) > maximum {
		return nil, skillError(CodeResourceTooLarge, "read_file", ErrResourceTooLarge)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != int64(len(body)) {
		return nil, skillError(CodeResourceChanged, "read_file", ErrResourceChanged)
	}
	return body, nil
}

func verifyPath(root, path string, directory bool) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return skillError(CodeResourceEscape, "verify_path", ErrResourceEscape)
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return skillError(CodeResourceEscape, "verify_path", ErrResourceEscape)
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return skillError(CodeResourceNotFound, "verify_path", ErrResourceNotFound)
			}
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return skillError(CodeResourceEscape, "verify_path", ErrResourceEscape)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return skillError(CodeResourceEscape, "verify_path", ErrResourceEscape)
		}
		if index == len(parts)-1 && directory && !info.IsDir() {
			return skillError(CodeResourceEscape, "verify_path", ErrResourceEscape)
		}
	}
	return nil
}

func validRelativePath(value string) bool {
	if value == "" || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return filepath.Clean(filepath.FromSlash(value)) == filepath.FromSlash(value)
}
func normalizeLimits(value Limits) Limits {
	defaults := DefaultLimits()
	if value.MaxManifestBytes <= 0 {
		value.MaxManifestBytes = defaults.MaxManifestBytes
	}
	if value.MaxInstructionsBytes <= 0 {
		value.MaxInstructionsBytes = defaults.MaxInstructionsBytes
	}
	if value.MaxInstructionLines <= 0 {
		value.MaxInstructionLines = defaults.MaxInstructionLines
	}
	if value.MaxDescriptionBytes <= 0 {
		value.MaxDescriptionBytes = defaults.MaxDescriptionBytes
	}
	if value.MaxCompatibilityBytes <= 0 {
		value.MaxCompatibilityBytes = defaults.MaxCompatibilityBytes
	}
	if value.MaxMetadataEntries <= 0 {
		value.MaxMetadataEntries = defaults.MaxMetadataEntries
	}
	if value.MaxMetadataBytes <= 0 {
		value.MaxMetadataBytes = defaults.MaxMetadataBytes
	}
	if value.MaxArtifacts <= 0 {
		value.MaxArtifacts = defaults.MaxArtifacts
	}
	if value.MaxArtifactBytes <= 0 {
		value.MaxArtifactBytes = defaults.MaxArtifactBytes
	}
	return value
}
func diagnostic(source string, skill SkillKey, code Code, message string) Diagnostic {
	return Diagnostic{Source: source, Skill: skill, Code: code, Message: message}
}
func diagnosticForError(source string, skill SkillKey, err error) Diagnostic {
	code := CodeDescriptorInvalid
	var typed *Error
	if errors.As(err, &typed) {
		code = typed.Code
	}
	return diagnostic(source, skill, code, codeMessage(code))
}
func codeMessage(code Code) string { return strings.ReplaceAll(string(code), "_", " ") }
