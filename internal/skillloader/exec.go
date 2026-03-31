package skillloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/jialuohu/curlycatclaw/skills"
)

// ExecAdapter runs an external skill as a subprocess, communicating via
// stdin/stdout JSON. The subprocess environment is minimal (PATH, HOME,
// TMPDIR only) plus any skill-specific env from skill.toml, preventing
// leakage of daemon secrets like CLAUDE_CODE_OAUTH_TOKEN.
type ExecAdapter struct {
	command string
	args    []string
	dir     string
	env     []string
	timeout time.Duration
}

// execInput is the JSON payload written to the subprocess stdin.
type execInput struct {
	Input   json.RawMessage `json:"input"`
	Context execContext      `json:"context"`
}

type execContext struct {
	UserID int64 `json:"user_id"`
	ChatID int64 `json:"chat_id"`
}

// execOutput is the JSON payload expected from the subprocess stdout.
type execOutput struct {
	Result string `json:"result"`
	Error  string `json:"error"`
}

// NewExecAdapter creates an adapter that runs the given command in the
// specified directory. The environment is built from scratch: only PATH,
// HOME, and TMPDIR from the host plus any skill-specific env vars.
func NewExecAdapter(command string, args []string, dir string, skillEnv map[string]string, timeout time.Duration) *ExecAdapter {
	env := buildMinimalEnv(skillEnv)
	return &ExecAdapter{
		command: command,
		args:    args,
		dir:     dir,
		env:     env,
		timeout: timeout,
	}
}

// buildMinimalEnv constructs a minimal environment from PATH, HOME, and
// TMPDIR (from the current process), plus any skill-specific overrides.
// This prevents the daemon's secrets from leaking to external skills.
func buildMinimalEnv(skillEnv map[string]string) []string {
	env := make([]string, 0, 3+len(skillEnv))

	if v := os.Getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	if v := os.Getenv("HOME"); v != "" {
		env = append(env, "HOME="+v)
	}
	env = append(env, "TMPDIR=/tmp")

	for k, v := range skillEnv {
		env = append(env, k+"="+v)
	}
	return env
}

func (a *ExecAdapter) Start(_ context.Context) error { return nil }
func (a *ExecAdapter) Stop() error                    { return nil }

// Execute runs the subprocess with the given input, enforcing the
// configured timeout via context.
func (a *ExecAdapter) Execute(ctx context.Context, input json.RawMessage, user skills.UserInfo) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	payload, err := json.Marshal(execInput{
		Input: input,
		Context: execContext{
			UserID: user.UserID,
			ChatID: user.ChatID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("exec: marshal input: %w", err)
	}

	cmd := exec.CommandContext(ctx, a.command, a.args...)
	cmd.Dir = a.dir
	cmd.Env = a.env
	cmd.Stdin = bytes.NewReader(payload)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("exec: timeout after %s", a.timeout)
		}
		detail := stderr.String()
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("exec: %s", detail)
	}

	var out execOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return "", fmt.Errorf("exec: invalid output JSON: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("exec: skill error: %s", out.Error)
	}
	return out.Result, nil
}
