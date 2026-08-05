package skills

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeInvalidSource         Code = "invalid_source"
	CodeCatalogNotReady       Code = "catalog_not_ready"
	CodeGenerationUnavailable Code = "generation_unavailable"
	CodeDescriptorInvalid     Code = "descriptor_invalid"
	CodeVersionInvalid        Code = "version_invalid"
	CodeDuplicateSkill        Code = "duplicate_skill"
	CodePrecedenceConflict    Code = "precedence_conflict"
	CodeTrustDenied           Code = "trust_denied"
	CodeSkillNotFound         Code = "skill_not_found"
	CodeResourceNotFound      Code = "resource_not_found"
	CodeResourceEscape        Code = "resource_escape"
	CodeResourceChanged       Code = "resource_changed"
	CodeResourceTooLarge      Code = "resource_too_large"
	CodeSchemaUnsupported     Code = "schema_unsupported"
	CodeRevoked               Code = "revoked"
	CodeClosed                Code = "closed"
)

var (
	ErrInvalidSource         = errors.New("agent skills: invalid source")
	ErrCatalogNotReady       = errors.New("agent skills: catalog not ready")
	ErrGenerationUnavailable = errors.New("agent skills: generation unavailable")
	ErrDescriptorInvalid     = errors.New("agent skills: descriptor invalid")
	ErrVersionInvalid        = errors.New("agent skills: version invalid")
	ErrDuplicateSkill        = errors.New("agent skills: duplicate skill")
	ErrPrecedenceConflict    = errors.New("agent skills: precedence conflict")
	ErrTrustDenied           = errors.New("agent skills: trust denied")
	ErrSkillNotFound         = errors.New("agent skills: skill not found")
	ErrResourceNotFound      = errors.New("agent skills: resource not found")
	ErrResourceEscape        = errors.New("agent skills: resource escape")
	ErrResourceChanged       = errors.New("agent skills: resource changed")
	ErrResourceTooLarge      = errors.New("agent skills: resource too large")
	ErrSchemaUnsupported     = errors.New("agent skills: schema unsupported")
	ErrRevoked               = errors.New("agent skills: generation revoked")
	ErrClosed                = errors.New("agent skills: closed")
)

type Error struct {
	Code       Code
	Operation  string
	Skill      SkillKey
	Generation Generation
	Source     string
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	message := "agent skills"
	if e.Operation != "" {
		message += ": " + e.Operation
	}
	if e.Skill != "" {
		message += fmt.Sprintf(" skill=%q", e.Skill)
	}
	if e.Generation != "" {
		message += fmt.Sprintf(" generation=%q", e.Generation)
	}
	if e.Source != "" {
		message += fmt.Sprintf(" source=%q", e.Source)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func skillError(code Code, operation string, cause error) error {
	return &Error{Code: code, Operation: operation, Cause: cause}
}
