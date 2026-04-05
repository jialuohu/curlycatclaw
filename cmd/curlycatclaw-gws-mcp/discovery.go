package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gwsToolOutput is the typed output for MCP tool results.
type gwsToolOutput struct {
	Text string `json:"text"`
}

// skillInfo holds parsed metadata from a SKILL.md file.
type skillInfo struct {
	Name        string // e.g. "gws-gmail-send"
	Description string // e.g. "Gmail: Send an email."
	Service     string // e.g. "gmail"
	Helper      string // e.g. "+send" (empty for service-level skills)
	Flags       []flagInfo
}

// flagInfo describes a single CLI flag parsed from a ## Flags table.
type flagInfo struct {
	Name        string // e.g. "to" (without --)
	Required    bool
	Default     string
	Description string
	IsBoolean   bool // true for bare flags (no value), detected from --help output
}

// discoverAndRegister generates skills from gws, parses them, and registers
// MCP tools. Returns the number of tools registered.
func discoverAndRegister(server *mcp.Server, exec *Executor, gwsPath, skillsDir, filterStr string) (int, error) {
	// If no skills dir provided, generate skills to a temp directory.
	if skillsDir == "" {
		var err error
		skillsDir, err = generateSkills(gwsPath)
		if err != nil {
			return 0, fmt.Errorf("generate-skills: %w", err)
		}
		// skillsDir is baseDir/skills; remove the parent to clean up fully.
		defer func() { _ = os.RemoveAll(filepath.Dir(skillsDir)) }()
	}

	skills, err := parseSkillsDir(skillsDir)
	if err != nil {
		return 0, fmt.Errorf("parse skills: %w", err)
	}

	filters := parseFilter(filterStr)

	// Collect skills that pass the filter.
	var filtered []skillInfo
	for _, skill := range skills {
		if skill.Helper == "" {
			continue
		}
		toolName := strings.ReplaceAll(skill.Name, "-", "_")
		if len(filters) > 0 && !matchesFilter(toolName, filters) {
			continue
		}
		filtered = append(filtered, skill)
	}

	// Detect boolean flags concurrently (bounded to 5 workers).
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	results := make([]skillInfo, len(filtered))
	for i, skill := range filtered {
		results[i] = skill
		wg.Add(1)
		go func(idx int, s skillInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx].Flags = detectBooleanFlags(gwsPath, s.Service, s.Helper, s.Flags)
		}(i, skill)
	}
	wg.Wait()

	count := 0
	for _, skill := range results {
		registerHelperTool(server, exec, skill)
		count++
	}

	return count, nil
}

// generateSkills runs `gws generate-skills` and returns the output directory.
// gws rejects absolute --output-dir paths, so we create a writable base
// directory and run the command from there with a relative subdirectory name.
func generateSkills(gwsPath string) (string, error) {
	// Create a writable base directory in the system temp area.
	baseDir, err := os.MkdirTemp("", "gws-mcp-*")
	if err != nil {
		return "", err
	}

	const subDir = "skills"
	absDir := filepath.Join(baseDir, subDir)
	if err := os.Mkdir(absDir, 0755); err != nil {
		_ = os.RemoveAll(baseDir)
		return "", err
	}

	slog.Info("gws-mcp: running gws generate-skills (may take a moment on first run)")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Run from baseDir so the relative subDir resolves correctly.
	cmd := exec.CommandContext(ctx, gwsPath, "generate-skills", "--output-dir", subDir)
	cmd.Dir = baseDir
	cmd.Stderr = os.Stderr // let gws progress output through
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(baseDir)
		return "", fmt.Errorf("gws generate-skills: %w", err)
	}

	return absDir, nil
}

// parseSkillsDir reads all SKILL.md files from subdirectories.
func parseSkillsDir(dir string) ([]skillInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var skills []skillInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip missing SKILL.md
		}
		skill, err := parseSkillMD(data)
		if err != nil {
			slog.Warn("gws-mcp: failed to parse skill", "path", path, "err", err)
			continue
		}
		skills = append(skills, skill)
	}
	return skills, nil
}

// parseSkillMD parses a single SKILL.md file into skillInfo.
func parseSkillMD(data []byte) (skillInfo, error) {
	var info skillInfo

	// Parse YAML frontmatter (between --- delimiters).
	frontmatter, body, ok := splitFrontmatter(data)
	if !ok {
		return info, fmt.Errorf("no YAML frontmatter found")
	}

	info.Name = yamlValue(frontmatter, "name")
	info.Description = yamlValue(frontmatter, "description")
	if info.Name == "" {
		return info, fmt.Errorf("missing name in frontmatter")
	}

	// Derive service and helper from name: "gws-gmail-send" → service="gmail", helper="+send"
	info.Service, info.Helper = parseSkillName(info.Name)

	// Parse ## Flags table.
	info.Flags = parseFlagsTable(body)

	return info, nil
}

// splitFrontmatter splits YAML frontmatter from body content.
func splitFrontmatter(data []byte) (frontmatter, body string, ok bool) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return "", "", false
	}
	rest := s[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+5:], true
}

// yamlValue extracts a simple key: value from YAML text. Handles quoted values.
func yamlValue(yaml, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(yaml, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			val := strings.TrimSpace(line[len(prefix):])
			// Strip surrounding quotes.
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			return val
		}
	}
	return ""
}

// parseSkillName extracts service and helper from a skill name.
// "gws-gmail-send" → ("gmail", "+send")
// "gws-gmail"      → ("gmail", "")
// "gws-calendar-agenda" → ("calendar", "+agenda")
func parseSkillName(name string) (service, helper string) {
	// Strip "gws-" prefix.
	name = strings.TrimPrefix(name, "gws-")

	// Known services (ordered longest first to avoid prefix matching issues).
	// This list matches gws's service aliases.
	knownServices := []string{
		"admin-reports", "classroom", "calendar", "modelarmor",
		"people", "script", "sheets", "slides", "events", "forms",
		"gmail", "tasks", "drive", "chat", "docs", "keep", "meet",
		"workflow",
	}

	for _, svc := range knownServices {
		if name == svc {
			return svc, ""
		}
		if strings.HasPrefix(name, svc+"-") {
			rest := name[len(svc)+1:]
			return svc, "+" + rest
		}
	}

	// Fallback: first segment is service, rest is helper.
	if idx := strings.IndexByte(name, '-'); idx > 0 {
		return name[:idx], "+" + name[idx+1:]
	}
	return name, ""
}

// parseFlagsTable extracts flag definitions from a ## Flags markdown table.
func parseFlagsTable(body string) []flagInfo {
	var flags []flagInfo
	inTable := false
	headerSkipped := false

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "## Flags" {
			inTable = true
			continue
		}
		if inTable && strings.HasPrefix(line, "##") {
			break // next section
		}
		if !inTable {
			continue
		}

		// Skip table header row and separator.
		if !headerSkipped {
			if strings.HasPrefix(line, "|") && strings.Contains(line, "Flag") {
				continue // header
			}
			if strings.HasPrefix(line, "|") && strings.Contains(line, "---") {
				headerSkipped = true
				continue
			}
			continue
		}

		if !strings.HasPrefix(line, "|") {
			if line == "" {
				continue
			}
			break // end of table
		}

		flag := parseTableRow(line)
		if flag.Name != "" {
			flags = append(flags, flag)
		}
	}

	return flags
}

// parseTableRow parses a single markdown table row into a flagInfo.
// Format: | `--flag` | ✓ | — | Description |
func parseTableRow(line string) flagInfo {
	parts := strings.Split(line, "|")
	if len(parts) < 5 {
		return flagInfo{}
	}

	// parts[0] is empty (before first |), parts[1] is flag, etc.
	flagStr := strings.TrimSpace(parts[1])
	requiredStr := strings.TrimSpace(parts[2])
	defaultStr := strings.TrimSpace(parts[3])
	descStr := strings.TrimSpace(parts[4])

	// Extract flag name: `--to` → "to", `-a/--attach` → "attach"
	flagName := extractFlagName(flagStr)
	if flagName == "" {
		return flagInfo{}
	}

	return flagInfo{
		Name:        flagName,
		Required:    requiredStr == "✓" || requiredStr == "yes" || requiredStr == "Yes",
		Default:     cleanDash(defaultStr),
		Description: descStr,
	}
}

// extractFlagName extracts the long flag name from backtick-wrapped text.
func extractFlagName(s string) string {
	// Remove backticks.
	s = strings.ReplaceAll(s, "`", "")
	s = strings.TrimSpace(s)

	// Handle combined short/long: "-a/--attach" or "--attach"
	for _, part := range strings.Split(s, "/") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "--") {
			name := strings.TrimPrefix(part, "--")
			// Strip <TYPE> suffix: "--to <EMAILS>" → "to"
			if idx := strings.IndexByte(name, ' '); idx > 0 {
				name = name[:idx]
			}
			return name
		}
	}
	return ""
}

// cleanDash returns empty string for markdown dash placeholders.
func cleanDash(s string) string {
	if s == "—" || s == "-" || s == "–" {
		return ""
	}
	return s
}

// registerHelperTool registers a gws helper command as an MCP tool.
func registerHelperTool(server *mcp.Server, e *Executor, skill skillInfo) {
	schema := injectAccountField(buildInputSchema(skill.Flags), e.Accounts, e.DefaultAccount)

	toolName := strings.ReplaceAll(skill.Name, "-", "_")
	tool := &mcp.Tool{
		Name:        toolName,
		Description: skill.Description,
		InputSchema: schema,
	}

	svc := skill.Service
	helper := skill.Helper

	// Build an allowlist of known flag names for this tool.
	allowedFlags := make(map[string]bool, len(skill.Flags))
	for _, f := range skill.Flags {
		allowedFlags[f.Name] = true
	}

	mcp.AddTool(server, tool, func(
		ctx context.Context,
		req *mcp.CallToolRequest,
		input map[string]any,
	) (*mcp.CallToolResult, gwsToolOutput, error) {
		// Resolve account and validate service access.
		accountInput, _ := input["account"].(string)
		resolvedName, credPath, err := e.ResolveAccount(accountInput)
		if err != nil {
			return errResult(err.Error()), gwsToolOutput{}, nil
		}
		if err := e.ValidateService(resolvedName, svc); err != nil {
			return errResult(err.Error()), gwsToolOutput{}, nil
		}

		// Strip any input keys not in the discovered flag set.
		filtered := make(map[string]any, len(input))
		for k, v := range input {
			if allowedFlags[k] {
				filtered[k] = v
			}
		}
		result, err := e.ExecuteHelper(ctx, svc, helper, filtered, AccountEnv(credPath))
		if err != nil {
			return errResult(err.Error()), gwsToolOutput{}, nil
		}
		return nil, gwsToolOutput{Text: result}, nil
	})
}

// registerGenericTool registers the gws_api escape hatch tool.
func registerGenericTool(server *mcp.Server, e *Executor) {
	schema := injectAccountField(json.RawMessage(`{
		"type": "object",
		"properties": {
			"service":  {"type": "string", "description": "Google Workspace service (e.g. gmail, calendar, drive, sheets, docs)"},
			"resource": {"type": "string", "description": "API resource (e.g. messages, events, files). Optional for helper commands."},
			"method":   {"type": "string", "description": "API method (e.g. list, get, create) or helper command (e.g. +send, +agenda)"},
			"params":   {"type": "object", "description": "Query parameters as key-value pairs (passed via --params JSON)"},
			"body":     {"type": "object", "description": "Request body as key-value pairs (passed via --json JSON)"}
		},
		"required": ["service", "method"]
	}`), e.Accounts, e.DefaultAccount)

	tool := &mcp.Tool{
		Name:        "gws_api",
		Description: "Execute any Google Workspace API command via gws CLI. Use this for operations not covered by the dedicated tools.",
		InputSchema: schema,
	}

	mcp.AddTool(server, tool, func(
		ctx context.Context,
		req *mcp.CallToolRequest,
		input map[string]any,
	) (*mcp.CallToolResult, gwsToolOutput, error) {
		// Resolve account and validate service access.
		accountInput, _ := input["account"].(string)
		resolvedName, credPath, err := e.ResolveAccount(accountInput)
		if err != nil {
			return errResult(err.Error()), gwsToolOutput{}, nil
		}

		service, _ := input["service"].(string)
		resource, _ := input["resource"].(string)
		method, _ := input["method"].(string)

		if service == "" || method == "" {
			return errResult("service and method are required"), gwsToolOutput{}, nil
		}

		if err := e.ValidateService(resolvedName, service); err != nil {
			return errResult(err.Error()), gwsToolOutput{}, nil
		}

		params := toStringMap(input["params"])
		body := toStringMap(input["body"])

		result, err := e.ExecuteAPI(ctx, service, resource, method, params, body, AccountEnv(credPath))
		if err != nil {
			return errResult(err.Error()), gwsToolOutput{}, nil
		}
		return nil, gwsToolOutput{Text: result}, nil
	})
}

// registerAccountsTool registers a read-only tool that lists available accounts.
func registerAccountsTool(server *mcp.Server, accounts map[string]string, defaultAccount string, services map[string][]string) {
	schema := json.RawMessage(`{"type": "object", "properties": {}}`)
	tool := &mcp.Tool{
		Name:        "gws_list_accounts",
		Description: "List available Google Workspace accounts and the default.",
		InputSchema: schema,
	}

	mcp.AddTool(server, tool, func(
		ctx context.Context,
		req *mcp.CallToolRequest,
		input map[string]any,
	) (*mcp.CallToolResult, gwsToolOutput, error) {
		type accountInfo struct {
			Name      string   `json:"name"`
			IsDefault bool     `json:"is_default,omitempty"`
			Services  []string `json:"services"` // explicit list, or ["all"] for unrestricted
		}
		var list []accountInfo
		for name := range accounts {
			ai := accountInfo{Name: name, IsDefault: name == defaultAccount}
			if svcs, ok := services[name]; ok {
				ai.Services = svcs
			} else {
				ai.Services = []string{"all"}
			}
			list = append(list, ai)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		data, _ := json.Marshal(list)
		return nil, gwsToolOutput{Text: string(data)}, nil
	})
}

// injectAccountField adds an optional "account" property to a JSON schema
// when multi-account mode is active (accounts map is non-empty).
// Returns the schema unchanged if accounts is nil or empty.
func injectAccountField(schema json.RawMessage, accounts map[string]string, defaultAccount string) json.RawMessage {
	if len(accounts) == 0 {
		return schema
	}

	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return schema
	}

	props, _ := s["properties"].(map[string]any)
	if props == nil {
		props = make(map[string]any)
		s["properties"] = props
	}

	names := make([]string, 0, len(accounts))
	for k := range accounts {
		names = append(names, k)
	}
	sort.Strings(names)

	desc := fmt.Sprintf("Google account to use. Available: %s. Default: %s",
		strings.Join(names, ", "), defaultAccount)
	props["account"] = map[string]any{
		"type":        "string",
		"description": desc,
	}

	data, err := json.Marshal(s)
	if err != nil {
		return schema
	}
	return data
}

// buildInputSchema creates a JSON schema from flag definitions as json.RawMessage.
func buildInputSchema(flags []flagInfo) json.RawMessage {
	properties := make(map[string]any, len(flags))
	var required []string

	for _, f := range flags {
		flagType := "string"
		if f.IsBoolean {
			flagType = "boolean"
		}
		prop := map[string]any{
			"type":        flagType,
			"description": f.Description,
		}
		if f.Default != "" {
			prop["default"] = f.Default
		}
		properties[f.Name] = prop

		if f.Required {
			required = append(required, f.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	data, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return data
}

// errResult creates an MCP error result.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// toStringMap converts an any value to map[string]any, returning nil if not a map.
func toStringMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// helpFlagRe matches a flag definition at the start of a --help option line.
// Anchored to leading whitespace to avoid matching flags mentioned in descriptions.
// Matches lines like:
//
//	      --flagname <TYPE>      Description        (value flag)
//	      --flagname             Description        (boolean flag)
//	  -x, --flagname <TYPE>      Description        (value flag with short alias)
var helpFlagRe = regexp.MustCompile(`^\s+(?:-\w,\s+)?--([a-zA-Z0-9][-a-zA-Z0-9]*)\s*(<[^>]+>)?`)

// detectBooleanFlags runs `gws <service> +<helper> --help` and marks flags
// that have no <TYPE> annotation as boolean. On error, returns flags unchanged.
func detectBooleanFlags(gwsPath, service, helper string, flags []flagInfo) []flagInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, gwsPath, service, helper, "--help")
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("gws-mcp: --help failed, skipping boolean detection", "service", service, "helper", helper, "err", err)
		return flags
	}

	// Build a set of boolean flag names from the help output.
	booleans := make(map[string]bool)
	inOptions := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Options:" || strings.HasPrefix(trimmed, "Options:") {
			inOptions = true
			continue
		}
		if !inOptions {
			continue
		}
		// Stop at the next section (EXAMPLES:, TIPS:, etc.)
		if len(trimmed) > 0 && trimmed[0] >= 'A' && trimmed[0] <= 'Z' && strings.HasSuffix(trimmed, ":") && !strings.Contains(trimmed, "--") {
			break
		}

		m := helpFlagRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		flagName := m[1]
		hasType := m[2] != ""
		if !hasType {
			booleans[flagName] = true
		}
	}

	// Apply boolean detection to the parsed flags.
	result := make([]flagInfo, len(flags))
	copy(result, flags)
	for i, f := range result {
		if booleans[f.Name] {
			result[i].IsBoolean = true
		}
	}
	return result
}

// parseFilter splits a comma-separated filter string into glob patterns.
func parseFilter(s string) []string {
	if s == "" {
		return nil
	}
	var filters []string
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			filters = append(filters, f)
		}
	}
	return filters
}

// matchesFilter checks if a skill name matches any filter pattern.
// Supports simple glob: "gmail_*" matches "gws_gmail_send".
func matchesFilter(name string, filters []string) bool {
	for _, f := range filters {
		matched, err := filepath.Match(f, name)
		if err != nil {
			slog.Warn("gws-mcp: invalid filter pattern", "pattern", f, "err", err)
			continue
		}
		if matched {
			return true
		}
		// Also try matching against the name with gws_ prefix stripped.
		stripped := strings.TrimPrefix(name, "gws_")
		if matched, _ = filepath.Match(f, stripped); matched {
			return true
		}
	}
	return false
}

