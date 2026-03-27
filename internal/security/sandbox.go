package security

// SandboxParams holds the paths to allowlist for the Landlock sandbox.
type SandboxParams struct {
	DataDir      string
	ConfigPath   string
	LogDir       string   // log file parent directory (rw, for lumberjack rotation)
	ExtraPaths   []string // additional read-only paths
	ExtraPathsRW []string // additional read-write paths
}
