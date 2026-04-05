package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const defaultTimeout = 60 * time.Second

// Executor runs gws CLI commands as subprocesses.
type Executor struct {
	GWSPath        string
	Timeout        time.Duration     // zero means defaultTimeout
	Accounts       map[string]string   // account name -> credential file path (nil = single-account mode)
	DefaultAccount string              // default account name when Accounts is non-nil
	Services       map[string][]string // account name -> allowed services (nil entry = all services)
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

// ResolveAccount returns the resolved account name and credential file path.
// If name is empty and accounts are configured, it defaults to DefaultAccount.
// If accounts are not configured (single-account mode), it returns ("", "", nil).
func (e *Executor) ResolveAccount(name string) (resolvedName, credPath string, err error) {
	if len(e.Accounts) == 0 {
		return "", "", nil
	}
	if name == "" {
		name = e.DefaultAccount
	}
	path, ok := e.Accounts[name]
	if !ok {
		names := make([]string, 0, len(e.Accounts))
		for k := range e.Accounts {
			names = append(names, k)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("unknown account %q; available: %s (default: %s)",
			name, strings.Join(names, ", "), e.DefaultAccount)
	}
	return name, path, nil
}

// ValidateService checks if the given account is allowed to use the given service.
// Returns nil if services are not configured, account has no restrictions, or service is allowed.
func (e *Executor) ValidateService(accountName, service string) error {
	if len(e.Services) == 0 {
		return nil
	}
	allowed, ok := e.Services[accountName]
	if !ok {
		return nil // no restrictions for this account
	}
	for _, s := range allowed {
		if s == service {
			return nil
		}
	}
	// Build list of accounts that DO support this service.
	var supporting []string
	for name, svcs := range e.Services {
		for _, s := range svcs {
			if s == service {
				supporting = append(supporting, name)
				break
			}
		}
	}
	// Also include accounts with no restrictions (they support everything).
	for name := range e.Accounts {
		if _, restricted := e.Services[name]; !restricted {
			supporting = append(supporting, name)
		}
	}
	sort.Strings(supporting)
	return fmt.Errorf("account %q does not support %s; accounts with %s access: %s",
		accountName, service, service, strings.Join(supporting, ", "))
}

// AccountEnv builds an environment override map for the given credential path.
// Returns nil if credPath is empty (single-account mode, no override needed).
func AccountEnv(credPath string) map[string]string {
	if credPath == "" {
		return nil
	}
	return map[string]string{
		"GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE": credPath,
	}
}

// ExecuteHelper runs a gws helper command (e.g. "gmail +send") with typed flags.
// The flags map is converted to CLI --flag value pairs.
func (e *Executor) ExecuteHelper(ctx context.Context, service, helper string, flags map[string]any, envOverride map[string]string) (string, error) {
	if err := validArg("service", service); err != nil {
		return "", err
	}
	if err := validArg("helper", helper); err != nil {
		return "", err
	}
	args := []string{service, helper, "--format", "json"}
	args = append(args, flagsToArgs(flags)...)
	return e.run(ctx, args, envOverride)
}

// ExecuteAPI runs a generic gws API command.
func (e *Executor) ExecuteAPI(ctx context.Context, service, resource, method string, params, body map[string]any, envOverride map[string]string) (string, error) {
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
	return e.run(ctx, args, envOverride)
}

func (e *Executor) run(ctx context.Context, args []string, envOverride map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, e.GWSPath, args...)
	cmd.WaitDelay = 2 * time.Second // don't hang on orphaned child processes

	// Apply per-call environment overrides (e.g. credential file for multi-account).
	// nil envOverride leaves cmd.Env nil, which inherits the parent process env.
	if envOverride != nil {
		env := make([]string, 0, 64)
		for _, kv := range os.Environ() {
			key, _, _ := strings.Cut(kv, "=")
			if _, overridden := envOverride[key]; !overridden {
				env = append(env, kv)
			}
		}
		for k, v := range envOverride {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

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
	"account":       true, // multi-account selector, handled by curlycatclaw-gws-mcp
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
