package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const defaultTimeout = 60 * time.Second

// Executor runs gws CLI commands as subprocesses.
type Executor struct {
	GWSPath string
	Timeout time.Duration // zero means defaultTimeout
}

func (e *Executor) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return defaultTimeout
}

// validArg checks that a positional argument is safe to pass to gws.
// Rejects empty strings, flag-like values (starting with -), and special characters.
var validArgRe = regexp.MustCompile(`^[+]?[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func validArg(name, value string) error {
	if value == "" {
		return fmt.Errorf("gws %s must not be empty", name)
	}
	if !validArgRe.MatchString(value) {
		return fmt.Errorf("gws %s contains invalid characters: %q", name, value)
	}
	return nil
}

// ExecuteHelper runs a gws helper command (e.g. "gmail +send") with typed flags.
// The flags map is converted to CLI --flag value pairs.
func (e *Executor) ExecuteHelper(ctx context.Context, service, helper string, flags map[string]any) (string, error) {
	if err := validArg("service", service); err != nil {
		return "", err
	}
	if err := validArg("helper", helper); err != nil {
		return "", err
	}
	args := []string{service, helper, "--format", "json"}
	args = append(args, flagsToArgs(flags)...)
	return e.run(ctx, args)
}

// ExecuteAPI runs a generic gws API command.
func (e *Executor) ExecuteAPI(ctx context.Context, service, resource, method string, params, body map[string]any) (string, error) {
	if err := validArg("service", service); err != nil {
		return "", err
	}
	if resource != "" {
		if err := validArg("resource", resource); err != nil {
			return "", err
		}
	}
	if err := validArg("method", method); err != nil {
		return "", err
	}
	args := []string{service}
	if resource != "" {
		args = append(args, resource)
	}
	if method != "" {
		args = append(args, method)
	}
	args = append(args, "--format", "json")

	if len(params) > 0 {
		p, err := json.Marshal(params)
		if err != nil {
			return "", fmt.Errorf("marshal params: %w", err)
		}
		args = append(args, "--params", string(p))
	}
	if len(body) > 0 {
		b, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal body: %w", err)
		}
		args = append(args, "--json", string(b))
	}
	return e.run(ctx, args)
}

func (e *Executor) run(ctx context.Context, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, e.GWSPath, args...)
	cmd.WaitDelay = 2 * time.Second // don't hang on orphaned child processes

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("gws command timed out after %s", e.timeout())
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("gws error: %s", errMsg)
	}

	return stdout.String(), nil
}

// reservedFlags are flags set internally or gws global flags that must not be
// overridden by LLM input. This prevents argument injection through helper tools.
var reservedFlags = map[string]bool{
	"format":        true,
	"config":        true,
	"profile":       true,
	"sanitize":      true,
	"page-all":      true,
	"page-limit":    true,
	"page-delay":    true,
	"dry-run":       true,
	"help":          true,
	"_user_context": true, // injected by curlycatclaw MCP manager, not a gws flag
}

// flagsToArgs converts a map of flag name → value into CLI arguments.
// Boolean true → --flag (no value). Slices → repeated --flag value.
// Reserved flags (e.g. --format) are silently skipped to prevent LLM override.
func flagsToArgs(flags map[string]any) []string {
	var args []string
	for k, v := range flags {
		if reservedFlags[k] {
			continue
		}
		flag := "--" + k
		switch val := v.(type) {
		case bool:
			if val {
				args = append(args, flag)
			}
		case []any:
			for _, item := range val {
				args = append(args, flag, fmt.Sprint(item))
			}
		case nil:
			// skip
		default:
			args = append(args, flag, fmt.Sprint(val))
		}
	}
	return args
}
