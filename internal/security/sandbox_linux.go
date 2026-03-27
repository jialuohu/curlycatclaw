//go:build linux

package security

import (
	"log/slog"
	"path/filepath"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// ApplySandbox restricts filesystem access using Landlock. Paths not in
// the allowlist become inaccessible. Uses BestEffort so the call degrades
// gracefully on kernels older than 5.13.
func ApplySandbox(p SandboxParams) error {
	roPaths := []landlock.Rule{
		landlock.ROFiles(p.ConfigPath),
		landlock.RODirs("/etc"),
		landlock.RODirs("/usr/share/ca-certificates"),
		landlock.RODirs("/etc/ssl"),
		landlock.RODirs("/usr/share/zoneinfo"),
	}
	rwPaths := []landlock.Rule{
		landlock.RWDirs(p.DataDir),
		landlock.RWDirs("/tmp"),
	}

	// Allow lumberjack to rotate log files if a log file path is configured.
	if p.LogDir != "" {
		rwPaths = append(rwPaths, landlock.RWDirs(p.LogDir))
	}

	// Add the directory containing the config file as read-only.
	if dir := filepath.Dir(p.ConfigPath); dir != "" && dir != "." {
		roPaths = append(roPaths, landlock.RODirs(dir))
	}

	for _, path := range p.ExtraPaths {
		roPaths = append(roPaths, landlock.RODirs(path))
	}
	for _, path := range p.ExtraPathsRW {
		rwPaths = append(rwPaths, landlock.RWDirs(path))
	}

	all := make([]landlock.Rule, 0, len(roPaths)+len(rwPaths))
	all = append(all, roPaths...)
	all = append(all, rwPaths...)

	if err := landlock.V5.BestEffort().RestrictPaths(all...); err != nil {
		return err
	}

	slog.Info("sandbox: landlock applied",
		"data_dir", p.DataDir,
		"config", p.ConfigPath,
		"extra_ro", len(p.ExtraPaths),
		"extra_rw", len(p.ExtraPathsRW),
	)
	return nil
}
