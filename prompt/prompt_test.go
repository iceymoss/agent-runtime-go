package prompt

import (
	"errors"
	"testing"
)

func TestPromptCompileAndRender(t *testing.T) {
	p, err := New("greeting", `Hello, {{.Name}}`)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	got, err := p.Render(map[string]any{"Name": "Ava"})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}
	if got != "Hello, Ava" {
		t.Fatalf("Render() = %q, want %q", got, "Hello, Ava")
	}
	if p.Version() != "sha256:0d60809d817d973fe6a98d1ea5ec49dee63559d055a3be0645283906d871c926" {
		t.Fatalf("Version() = %q", p.Version())
	}
}

func TestPromptVersionTracksSemanticContent(t *testing.T) {
	first, err := New("first", "same")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("second", "same")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := New("first", "changed")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version() != second.Version() || first.Version() == changed.Version() {
		t.Fatalf("versions = %q, %q, %q", first.Version(), second.Version(), changed.Version())
	}
}

func TestPromptRejectsInvalidTemplate(t *testing.T) {
	_, err := New("broken", `{{if}}`)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}

func TestPromptRejectsMissingVariable(t *testing.T) {
	p, err := New("greeting", `Hello, {{.Name}}`)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = p.Render(map[string]any{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}
