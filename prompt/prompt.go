// Package prompt provides side-effect-free prompt compilation and rendering.
package prompt

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"text/template"
)

var (
	// ErrInvalid means a prompt could not be compiled or rendered.
	ErrInvalid = errors.New("prompt: invalid")
	// ErrNotFound means the requested prompt does not exist.
	ErrNotFound = errors.New("prompt: not found")
)

// Prompt is an immutable, precompiled prompt template.
type Prompt struct {
	name     string
	source   string
	version  string
	template *template.Template
}

// New compiles a prompt and rejects syntax errors before runtime.
func New(name, source string) (*Prompt, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(source)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
	}
	version := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("go-text-template-v1\x00"+source)))
	return &Prompt{name: name, source: source, version: version, template: tmpl}, nil
}

// Name returns the stable prompt name.
func (p *Prompt) Name() string { return p.name }

// Source returns the original template text.
func (p *Prompt) Source() string { return p.source }

// Version returns a deterministic semantic version of the template format and source.
func (p *Prompt) Version() string { return p.version }

// Render executes the precompiled template with caller-provided data only.
func (p *Prompt) Render(data any) (string, error) {
	var output bytes.Buffer
	if err := p.template.Execute(&output, data); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInvalid, p.name, err)
	}
	return output.String(), nil
}
