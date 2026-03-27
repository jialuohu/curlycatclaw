//go:build !linux

package security

import "log/slog"

// ApplySandbox is a no-op on non-Linux platforms where Landlock is unavailable.
func ApplySandbox(_ SandboxParams) error {
	slog.Info("sandbox: not available on this platform, skipping")
	return nil
}
