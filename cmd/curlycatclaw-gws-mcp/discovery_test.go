package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantFM     string
		wantBody   string
		wantOK     bool
	}{
		{
			name:     "valid frontmatter",
			input:    "---\nname: test\n---\nbody content",
			wantFM:   "name: test",
			wantBody: "body content",
			wantOK:   true,
		},
		{
			name:   "no frontmatter",
			input:  "just plain text",
			wantOK: false,
		},
		{
			name:   "missing closing delimiter",
			input:  "---\nname: test\nno end",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, ok := splitFrontmatter([]byte(tt.input))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if fm != tt.wantFM {
					t.Errorf("frontmatter = %q, want %q", fm, tt.wantFM)
				}
				if body != tt.wantBody {
					t.Errorf("body = %q, want %q", body, tt.wantBody)
				}
			}
		})
	}
}

func TestYamlValue(t *testing.T) {
	yaml := `name: gws-gmail-send
description: "Gmail: Send an email."
version: 0.22.5`

	tests := []struct {
		key  string
		want string
	}{
		{"name", "gws-gmail-send"},
		{"description", "Gmail: Send an email."},
		{"version", "0.22.5"},
		{"missing", ""},
	}

	for _, tt := range tests {
		got := yamlValue(yaml, tt.key)
		if got != tt.want {
			t.Errorf("yamlValue(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestParseSkillName(t *testing.T) {
	tests := []struct {
		name        string
		wantService string
		wantHelper  string
	}{
		{"gws-gmail-send", "gmail", "+send"},
		{"gws-gmail", "gmail", ""},
		{"gws-calendar-agenda", "calendar", "+agenda"},
		{"gws-sheets-read", "sheets", "+read"},
		{"gws-drive-upload", "drive", "+upload"},
		{"gws-admin-reports", "admin-reports", ""},
		{"gws-gmail-reply-all", "gmail", "+reply-all"},
		{"gws-shared", "shared", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, helper := parseSkillName(tt.name)
			if svc != tt.wantService {
				t.Errorf("service = %q, want %q", svc, tt.wantService)
			}
			if helper != tt.wantHelper {
				t.Errorf("helper = %q, want %q", helper, tt.wantHelper)
			}
		})
	}
}

func TestParseFlagsTable(t *testing.T) {
	body := `## Usage

` + "```bash" + `
gws gmail +send --to <EMAILS> --subject <SUBJECT> --body <TEXT>
` + "```" + `

## Flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| ` + "`--to`" + ` | ✓ | — | Recipient email addresses |
| ` + "`--subject`" + ` | ✓ | — | Email subject |
| ` + "`--body`" + ` | ✓ | — | Email body |
| ` + "`--cc`" + ` | — | — | CC addresses |
| ` + "`--html`" + ` | — | — | Treat body as HTML |

## Examples
`

	flags := parseFlagsTable(body)
	if len(flags) != 5 {
		t.Fatalf("got %d flags, want 5", len(flags))
	}

	// Check required flags.
	for _, name := range []string{"to", "subject", "body"} {
		found := false
		for _, f := range flags {
			if f.Name == name {
				found = true
				if !f.Required {
					t.Errorf("flag %q should be required", name)
				}
			}
		}
		if !found {
			t.Errorf("missing expected flag %q", name)
		}
	}

	// Check optional flags.
	for _, f := range flags {
		if f.Name == "cc" && f.Required {
			t.Error("flag 'cc' should not be required")
		}
	}
}

func TestExtractFlagName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"`--to`", "to"},
		{"`--subject`", "subject"},
		{"`-a/--attach`", "attach"},
		{"`--to <EMAILS>`", "to"},
		{"plain text", ""},
	}

	for _, tt := range tests {
		got := extractFlagName(tt.input)
		if got != tt.want {
			t.Errorf("extractFlagName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseSkillMD(t *testing.T) {
	md := `---
name: gws-gmail-send
description: "Gmail: Send an email."
metadata:
  version: 0.22.5
---

# gmail +send

## Usage

` + "```bash" + `
gws gmail +send --to <EMAILS> --subject <SUBJECT> --body <TEXT>
` + "```" + `

## Flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| ` + "`--to`" + ` | ✓ | — | Recipient email addresses |
| ` + "`--subject`" + ` | ✓ | — | Email subject |
| ` + "`--body`" + ` | ✓ | — | Email body |
`

	info, err := parseSkillMD([]byte(md))
	if err != nil {
		t.Fatal(err)
	}

	if info.Name != "gws-gmail-send" {
		t.Errorf("Name = %q, want %q", info.Name, "gws-gmail-send")
	}
	if info.Description != "Gmail: Send an email." {
		t.Errorf("Description = %q", info.Description)
	}
	if info.Service != "gmail" {
		t.Errorf("Service = %q, want %q", info.Service, "gmail")
	}
	if info.Helper != "+send" {
		t.Errorf("Helper = %q, want %q", info.Helper, "+send")
	}
	if len(info.Flags) != 3 {
		t.Fatalf("got %d flags, want 3", len(info.Flags))
	}
}

func TestParseSkillsDir(t *testing.T) {
	dir := t.TempDir()

	// Create a skill subdirectory with SKILL.md.
	skillDir := filepath.Join(dir, "gws-gmail-send")
	if err := os.Mkdir(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	md := `---
name: gws-gmail-send
description: "Gmail: Send an email."
---

# gmail +send

## Flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| ` + "`--to`" + ` | ✓ | — | Recipient email addresses |
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a non-skill directory (no SKILL.md).
	if err := os.Mkdir(filepath.Join(dir, "other"), 0755); err != nil {
		t.Fatal(err)
	}

	skills, err := parseSkillsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	if skills[0].Name != "gws-gmail-send" {
		t.Errorf("skill name = %q", skills[0].Name)
	}
}

func TestMatchesFilter(t *testing.T) {
	// Tool names use underscores (dashes in skill names are converted).
	tests := []struct {
		name    string
		filters []string
		want    bool
	}{
		{"gws_gmail_send", []string{"gws_gmail_*"}, true},
		{"gws_gmail_send", []string{"gmail_*"}, true},
		{"gws_calendar_agenda", []string{"gmail_*"}, false},
		{"gws_calendar_agenda", []string{"gmail_*", "calendar_*"}, true},
		{"gws_drive_list", []string{"*"}, true},
		// Dash-based names should NOT match underscore filters (this was a bug).
		{"gws-gmail-send", []string{"gmail_*"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesFilter(tt.name, tt.filters)
			if got != tt.want {
				t.Errorf("matchesFilter(%q, %v) = %v, want %v", tt.name, tt.filters, got, tt.want)
			}
		})
	}
}

func TestBuildInputSchema(t *testing.T) {
	flags := []flagInfo{
		{Name: "to", Required: true, Description: "Recipient"},
		{Name: "cc", Required: false, Description: "CC"},
		{Name: "format", Required: false, Default: "json", Description: "Output format"},
	}

	schema := buildInputSchema(flags)
	if schema == nil {
		t.Fatal("schema is nil")
	}

	// Verify it's valid JSON.
	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if m["type"] != "object" {
		t.Errorf("type = %v, want object", m["type"])
	}

	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties is not a map")
	}
	if len(props) != 3 {
		t.Errorf("got %d properties, want 3", len(props))
	}

	req, ok := m["required"].([]any)
	if !ok {
		t.Fatal("required is not a slice")
	}
	if len(req) != 1 || req[0] != "to" {
		t.Errorf("required = %v, want [to]", req)
	}

	// additionalProperties should NOT be set (curlycatclaw injects _user_context).
	if _, ok := m["additionalProperties"]; ok {
		t.Error("additionalProperties should not be set (breaks _user_context injection)")
	}
}

func TestBuildInputSchemaWithBooleans(t *testing.T) {
	flags := []flagInfo{
		{Name: "to", Required: true, Description: "Recipient"},
		{Name: "html", IsBoolean: true, Description: "Use HTML"},
		{Name: "draft", IsBoolean: true, Description: "Save as draft"},
	}

	schema := buildInputSchema(flags)
	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	props := m["properties"].(map[string]any)

	toProp := props["to"].(map[string]any)
	if toProp["type"] != "string" {
		t.Errorf("to type = %v, want string", toProp["type"])
	}

	htmlProp := props["html"].(map[string]any)
	if htmlProp["type"] != "boolean" {
		t.Errorf("html type = %v, want boolean", htmlProp["type"])
	}

	draftProp := props["draft"].(map[string]any)
	if draftProp["type"] != "boolean" {
		t.Errorf("draft type = %v, want boolean", draftProp["type"])
	}
}

func TestDetectBooleanFlags(t *testing.T) {
	// Test with the helpFlagRe regex directly on sample help output.
	helpOutput := `[Helper] Send an email

Usage: gws +send [OPTIONS] --to <EMAILS> --subject <SUBJECT> --body <TEXT>

Options:
      --to <EMAILS>          Recipient email address(es)
      --subject <SUBJECT>    Email subject
      --body <TEXT>          Email body
      --from <EMAIL>         Sender address
  -a, --attach <PATH>        Attach a file
      --html                 Treat --body as HTML content
      --dry-run              Show the request without executing
      --draft                Save as draft instead of sending
  -h, --help                 Print help
`

	// Parse the help output the same way detectBooleanFlags does.
	booleans := make(map[string]bool)
	inOptions := false
	for _, line := range strings.Split(helpOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Options:" || strings.HasPrefix(trimmed, "Options:") {
			inOptions = true
			continue
		}
		if !inOptions {
			continue
		}
		m := helpFlagRe.FindStringSubmatch(line)
		if m != nil && m[2] == "" {
			booleans[m[1]] = true
		}
	}

	// Boolean flags (no <TYPE>).
	for _, name := range []string{"html", "dry-run", "draft", "help"} {
		if !booleans[name] {
			t.Errorf("expected %q to be detected as boolean", name)
		}
	}

	// Value flags (have <TYPE>).
	for _, name := range []string{"to", "subject", "body", "from", "attach"} {
		if booleans[name] {
			t.Errorf("expected %q to NOT be detected as boolean", name)
		}
	}
}

func TestDetectBooleanFlagsFallback(t *testing.T) {
	// When the gws binary doesn't exist, detectBooleanFlags should return
	// the flags unchanged (graceful fallback).
	flags := []flagInfo{
		{Name: "to", Description: "Recipient"},
		{Name: "html", Description: "Use HTML"},
	}

	result := detectBooleanFlags("/nonexistent/gws", "gmail", "+send", flags)
	if len(result) != 2 {
		t.Fatalf("got %d flags, want 2", len(result))
	}
	for _, f := range result {
		if f.IsBoolean {
			t.Errorf("flag %q should not be boolean on fallback", f.Name)
		}
	}
}

func TestCleanDash(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"—", ""},
		{"-", ""},
		{"–", ""},
		{"default_value", "default_value"},
		{"", ""},
	}

	for _, tt := range tests {
		got := cleanDash(tt.input)
		if got != tt.want {
			t.Errorf("cleanDash(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseFilter(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"gmail_*", 1},
		{"gmail_*, calendar_*", 2},
		{"gmail_*,calendar_*,drive_*", 3},
	}

	for _, tt := range tests {
		got := parseFilter(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseFilter(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}
