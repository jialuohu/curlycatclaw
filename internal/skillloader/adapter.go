package skillloader

import (
	"context"
	"encoding/json"

	"github.com/jialuohu/curlycatclaw/skills"
)

// SkillAdapter defines how an external skill is executed. The exec adapter
// is the only implementation for v0.13.0; wasm and mcp adapters may follow.
type SkillAdapter interface {
	// Start performs any one-time initialization (e.g. starting a subprocess).
	Start(ctx context.Context) error

	// Execute runs the skill with the given input and user context.
	Execute(ctx context.Context, input json.RawMessage, user skills.UserInfo) (string, error)

	// Stop releases resources held by the adapter.
	Stop() error
}
