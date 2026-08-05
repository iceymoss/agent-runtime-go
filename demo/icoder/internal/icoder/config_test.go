package icoder

import (
	"regexp"
	"testing"
)

func TestNewSessionIDFormatAndUniqueness(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{16}$`)
	first, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.MatchString(first) || !pattern.MatchString(second) || first == second {
		t.Fatalf("session IDs = %q, %q", first, second)
	}
}

func TestConfigCreatesRandomSessionIDByDefault(t *testing.T) {
	config := Config{APIKey: "key", BaseURL: "https://example.com/v1", Model: "model", Workspace: t.TempDir(), Database: t.TempDir() + "/icoder.db"}
	if err := config.Normalize(); err != nil {
		t.Fatal(err)
	}
	if matched, err := regexp.MatchString(`^[0-9a-f]{16}$`, config.SessionID); err != nil || !matched {
		t.Fatalf("SessionID = %q, match error = %v", config.SessionID, err)
	}
}
