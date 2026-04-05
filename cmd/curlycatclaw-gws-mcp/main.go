package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "curlycatclaw-gws-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	gwsPath := os.Getenv("GWS_PATH")
	if gwsPath == "" {
		gwsPath = "gws"
	}

	filterStr := os.Getenv("GWS_FILTER")

	server := mcp.NewServer(
		&mcp.Implementation{Name: "curlycatclaw-gws-mcp", Version: version},
		nil,
	)

	exec := &Executor{GWSPath: gwsPath}

	// Parse multi-account configuration from GWS_ACCOUNT_* env vars.
	accounts := parseAccountsFromEnv()
	if len(accounts) > 0 {
		defaultAccount := os.Getenv("GWS_DEFAULT_ACCOUNT")
		if defaultAccount == "" {
			// Pick first alphabetically for determinism.
			for name := range accounts {
				if defaultAccount == "" || name < defaultAccount {
					defaultAccount = name
				}
			}
		}
		if _, ok := accounts[defaultAccount]; !ok {
			log.Fatalf("gws-mcp: GWS_DEFAULT_ACCOUNT %q not found in accounts: %v", defaultAccount, accountNames(accounts))
		}
		for name, path := range accounts {
			if !filepath.IsAbs(path) {
				log.Fatalf("gws-mcp: credential path for account %q must be absolute: %s", name, path)
			}
			if _, err := os.Stat(path); err != nil {
				log.Fatalf("gws-mcp: credential file for account %q not found: %s", name, path)
			}
		}
		exec.Accounts = accounts
		exec.DefaultAccount = defaultAccount
		exec.Services = parseServicesFromEnv(accounts)
		slog.Info("gws-mcp: multi-account mode", "accounts", accountNames(accounts), "default", defaultAccount)
	}

	// Discover tools from gws generate-skills output.
	skillsDir := os.Getenv("GWS_SKILLS_DIR")
	count, err := discoverAndRegister(server, exec, gwsPath, skillsDir, filterStr)
	if err != nil {
		slog.Warn("gws-mcp: skill discovery failed, generic tool only", "err", err)
	}

	// Always register the generic API escape hatch.
	registerGenericTool(server, exec)

	// Register account listing tool when multi-account is active.
	if len(accounts) > 0 {
		registerAccountsTool(server, accounts, exec.DefaultAccount, exec.Services)
	}

	slog.Info("gws-mcp: starting", "discovered_tools", count, "gws_path", gwsPath, "multi_account", len(accounts) > 0)

	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// accountNameRe restricts account names to safe characters.
var accountNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const accountEnvPrefix = "GWS_ACCOUNT_"

const servicesSuffix = "_SERVICES"

// parseAccountsFromEnv scans environment variables for GWS_ACCOUNT_* entries
// and builds a map of lowercase account name -> credential file path.
// Skips keys ending in _SERVICES to avoid collision with parseServicesFromEnv.
func parseAccountsFromEnv() map[string]string {
	accounts := make(map[string]string)
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, accountEnvPrefix) {
			continue
		}
		suffix := key[len(accountEnvPrefix):]
		if suffix == "" || strings.HasSuffix(strings.ToUpper(suffix), servicesSuffix) {
			continue
		}
		name := strings.ToLower(suffix)
		if !accountNameRe.MatchString(name) {
			log.Fatalf("gws-mcp: invalid account name %q (from env %s); must match [a-zA-Z0-9_-]+", name, key)
		}
		accounts[name] = value
	}
	if len(accounts) == 0 {
		return nil
	}
	return accounts
}

// parseServicesFromEnv scans for GWS_ACCOUNT_<NAME>_SERVICES env vars and builds
// a map of account name -> allowed service list. Returns nil if no _SERVICES vars found.
func parseServicesFromEnv(accounts map[string]string) map[string][]string {
	services := make(map[string][]string)
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, accountEnvPrefix) {
			continue
		}
		suffix := key[len(accountEnvPrefix):]
		upper := strings.ToUpper(suffix)
		if !strings.HasSuffix(upper, servicesSuffix) {
			continue
		}
		// Extract account name: remove _SERVICES suffix.
		namePart := suffix[:len(suffix)-len(servicesSuffix)]
		if namePart == "" {
			continue
		}
		name := strings.ToLower(namePart)
		if _, ok := accounts[name]; !ok {
			log.Fatalf("gws-mcp: GWS_ACCOUNT_%s_SERVICES references unknown account %q", strings.ToUpper(namePart), name)
		}
		var svcs []string
		for _, s := range strings.Split(value, ",") {
			s = strings.TrimSpace(strings.ToLower(s))
			if s != "" {
				svcs = append(svcs, s)
			}
		}
		if len(svcs) > 0 {
			services[name] = svcs
		}
	}
	if len(services) == 0 {
		return nil
	}
	return services
}

// accountNames returns a sorted list of account names for error messages.
func accountNames(accounts map[string]string) []string {
	names := make([]string, 0, len(accounts))
	for k := range accounts {
		names = append(names, k)
	}
	// Sort not needed for log output but helps determinism.
	return names
}
