package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplySandbox_ReturnsNilOnThisPlatform(t *testing.T) {
	// Create temp files so Landlock can validate paths on Linux.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("# test"), 0644); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}

	err := ApplySandbox(SandboxParams{
		DataDir:    dataDir,
		ConfigPath: configPath,
	})
	if err != nil {
		t.Fatalf("ApplySandbox returned unexpected error: %v", err)
	}
}
