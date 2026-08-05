package icoder

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	APIKey        string
	BaseURL       string
	Model         string
	Workspace     string
	Database      string
	SkillsRoot    string
	MCPURL        string
	SessionID     string
	AllowWrites   bool
	ContextWindow int
	MaxTokens     int
}

func (c *Config) Normalize() error {
	if c.APIKey == "" {
		c.APIKey = os.Getenv("ICODER_API_KEY")
	}
	if c.BaseURL == "" {
		c.BaseURL = os.Getenv("ICODER_BASE_URL")
	}
	if c.Model == "" {
		c.Model = os.Getenv("ICODER_MODEL")
	}
	if c.APIKey == "" || c.BaseURL == "" || c.Model == "" {
		return fmt.Errorf("ICODER_API_KEY, ICODER_BASE_URL, and ICODER_MODEL are required")
	}
	if c.Workspace == "" {
		c.Workspace = "."
	}
	workspace, err := filepath.Abs(c.Workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	c.Workspace = filepath.Clean(workspace)
	if c.Database == "" {
		c.Database = filepath.Join(c.Workspace, ".icoder.db")
	}
	if c.SessionID == "" {
		sessionID, err := NewSessionID()
		if err != nil {
			return err
		}
		c.SessionID = sessionID
	}
	if c.ContextWindow <= 0 {
		c.ContextWindow = 128000
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 4096
	}
	return nil
}

func NewSessionID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
