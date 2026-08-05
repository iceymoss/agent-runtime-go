package prompt

import (
	"errors"
	"testing"
)

func TestPromptCompileAndRender(t *testing.T) {
	p, err := New("greeting", `你好，{{.Name}}`)
	if err != nil {
		t.Fatalf("New() 报错: %v", err)
	}

	got, err := p.Render(map[string]any{"Name": "小知"})
	if err != nil {
		t.Fatalf("Render() 报错: %v", err)
	}
	if got != "你好，小知" {
		t.Fatalf("Render() = %q, 期望 %q", got, "你好，小知")
	}
	if p.Version() != "sha256:ee614f0c5db0ca8109a0253196945c02caa51f4e5e71c73d12e58f1de6f1b4ce" {
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
	p, err := New("greeting", `你好，{{.Name}}`)
	if err != nil {
		t.Fatalf("New() 报错: %v", err)
	}

	_, err = p.Render(map[string]any{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}
