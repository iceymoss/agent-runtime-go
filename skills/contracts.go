// Package skills provides a tenant-scoped, immutable catalog of untrusted
// instruction content. It never executes skill content or grants tool access.
package skills

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

const CurrentSchemaVersion uint16 = 1

type SkillKey string
type Version string
type Generation string
type TrustLevel string
type SourceKind string
type ContentClass string

const (
	TrustPlatform         TrustLevel = "platform"
	TrustTenantReviewed   TrustLevel = "tenant_reviewed"
	TrustTenantUnreviewed TrustLevel = "tenant_unreviewed"

	SourcePlatformFilesystem SourceKind = "platform_filesystem"
	SourceTenantFilesystem   SourceKind = "tenant_filesystem"
	SourceTenantDB           SourceKind = "tenant_db"

	ContentUntrustedInstructions ContentClass = "untrusted_instructions"
	ContentUntrustedArtifact     ContentClass = "untrusted_artifact"
	InstructionsKey                           = "instructions"
)

type Scope struct {
	TenantKey agent.TenantKey `json:"tenant_key"`
}

type SourceRef struct {
	Name       string     `json:"name"`
	Kind       SourceKind `json:"kind"`
	Generation string     `json:"generation"`
}

type Replacement struct {
	Key          SkillKey `json:"key" yaml:"key"`
	VersionRange string   `json:"version_range,omitempty" yaml:"version_range,omitempty"`
}

type ArtifactDescriptor struct {
	Key       string `json:"key" yaml:"key"`
	Path      string `json:"path,omitempty" yaml:"path,omitempty"`
	MIMEType  string `json:"mime_type" yaml:"mime_type"`
	SizeBytes int64  `json:"size_bytes" yaml:"size_bytes"`
	Digest    string `json:"digest" yaml:"digest"`
}

// ResourceDescriptor is retained as the domain name used by S28.
type ResourceDescriptor = ArtifactDescriptor

type Descriptor struct {
	Key                SkillKey             `json:"key" yaml:"-"`
	Name               string               `json:"name" yaml:"name"`
	Description        string               `json:"description" yaml:"description"`
	Version            Version              `json:"version" yaml:"version"`
	SchemaVersion      uint16               `json:"schema_version" yaml:"schema_version"`
	Compatibility      string               `json:"compatibility,omitempty" yaml:"compatibility,omitempty"`
	License            string               `json:"license,omitempty" yaml:"license,omitempty"`
	Trust              TrustLevel           `json:"trust" yaml:"trust,omitempty"`
	Publisher          string               `json:"publisher,omitempty" yaml:"publisher,omitempty"`
	ReviewVersion      string               `json:"review_version,omitempty" yaml:"review_version,omitempty"`
	Metadata           map[string]string    `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	ToolRequirements   []string             `json:"tool_requirements,omitempty" yaml:"tool_requirements,omitempty"`
	Source             SourceRef            `json:"source" yaml:"-"`
	Replaces           *Replacement         `json:"replaces,omitempty" yaml:"replaces,omitempty"`
	Artifacts          []ArtifactDescriptor `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Resources          []ResourceDescriptor `json:"resources,omitempty" yaml:"resources,omitempty"`
	DescriptorDigest   string               `json:"descriptor_digest" yaml:"-"`
	InstructionsDigest string               `json:"instructions_digest" yaml:"-"`
	ContentDigest      string               `json:"content_digest" yaml:"-"`
}

type Diagnostic struct {
	Source  string   `json:"source"`
	Skill   SkillKey `json:"skill,omitempty"`
	Code    Code     `json:"code"`
	Message string   `json:"message"`
}

type SourceSkill struct {
	Descriptor       Descriptor
	Instructions     []byte
	Artifacts        map[string][]byte
	Disabled         bool
	readInstructions func() ([]byte, error)
	readArtifact     func(string) ([]byte, error)
}

type Tombstone struct {
	Key    SkillKey
	Reason string
}

type SourceSnapshot struct {
	Generation  string
	Skills      []SourceSkill
	Tombstones  []Tombstone
	Diagnostics []Diagnostic
}

type Source interface {
	Name() string
	Kind() SourceKind
	Load(context.Context, Scope) (SourceSnapshot, error)
}

type SourceRegistration struct {
	Source   Source
	Required bool
}

type Options struct {
	Sources      []SourceRegistration
	AllowedTrust []TrustLevel
	Limits       Limits
	Clock        func() time.Time
}

type Snapshot struct {
	Generation        Generation        `json:"generation"`
	CreatedAt         time.Time         `json:"created_at"`
	SourceGenerations map[string]string `json:"source_generations"`
	Digest            string            `json:"digest"`
	Descriptors       []Descriptor      `json:"descriptors"`
	Diagnostics       []Diagnostic      `json:"diagnostics,omitempty"`
}

type Selector struct {
	Key     SkillKey `json:"key"`
	Version Version  `json:"version,omitempty"`
}

type ResolveRequest struct {
	Scope      Scope
	Generation Generation
	Selectors  []Selector
}

type PromptMetadata struct {
	Key           SkillKey     `json:"key"`
	Name          string       `json:"name"`
	Description   string       `json:"description"`
	Version       Version      `json:"version"`
	Trust         TrustLevel   `json:"trust"`
	Compatibility string       `json:"compatibility,omitempty"`
	Location      string       `json:"location"`
	ContentClass  ContentClass `json:"content_class"`
	Provenance    SourceRef    `json:"provenance"`
}

type Selection struct {
	Generation Generation       `json:"generation"`
	Skills     []Descriptor     `json:"skills"`
	Prompt     []PromptMetadata `json:"prompt"`
}

type ReadRequest struct {
	Scope      Scope
	Generation Generation
	Skill      SkillKey
	Version    Version
	Artifact   string
}

type Resource struct {
	Skill        SkillKey     `json:"skill"`
	Version      Version      `json:"version"`
	Key          string       `json:"key"`
	MIMEType     string       `json:"mime_type"`
	Content      []byte       `json:"content"`
	Digest       string       `json:"digest"`
	ContentClass ContentClass `json:"content_class"`
	Trusted      bool         `json:"trusted"`
	Provenance   SourceRef    `json:"provenance"`
}

type ScopeStatus struct {
	Started    bool
	Degraded   bool
	LastError  error
	Generation Generation
}

type RevokeRequest struct {
	Scope      Scope
	Generation Generation
}

type Catalog interface {
	StartScope(context.Context, Scope) error
	CloseScope(context.Context, Scope) error
	Current(Scope) (Snapshot, bool)
	Refresh(context.Context, Scope) (Snapshot, error)
	Acquire(Scope, Generation) (*Lease, error)
	Resolve(ResolveRequest) (Selection, error)
	Read(context.Context, ReadRequest) (Resource, error)
	Revoke(context.Context, RevokeRequest) error
	Status(Scope) ScopeStatus
	Close(context.Context) error
}
