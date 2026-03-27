package security

import "testing"

func TestApplySandbox_ReturnsNilOnThisPlatform(t *testing.T) {
	// On non-Linux (macOS, Windows), ApplySandbox is a no-op that returns nil.
	// On Linux, it applies Landlock restrictions.
	err := ApplySandbox(SandboxParams{
		DataDir:    "/tmp/test-data",
		ConfigPath: "/tmp/test-config.toml",
	})
	if err != nil {
		t.Fatalf("ApplySandbox returned unexpected error: %v", err)
	}
}
