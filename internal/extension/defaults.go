package extension

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultExtension describes a pre-seeded extension.
type defaultExtension struct {
	Extension
	// SkillFiles lists GitHub raw URLs to download into the skill directory.
	// Only used for type=prompt. Key is the relative file path, value is the URL.
	SkillFiles map[string]string
}

// defaultExtensions are pre-installed on first startup.
var defaultExtensions = []defaultExtension{
	{
		Extension: Extension{
			Name:        "scrapling-mcp",
			Type:        TypeMCP,
			Command:     "uvx",
			Args:        []string{"--from", "scrapling[all]", "scrapling", "mcp"},
			Description: "AI-powered web scraping via Scrapling MCP server. Extracts targeted content server-side to reduce tokens and speed up scraping.",
		},
	},
	{
		Extension: Extension{
			Name:        "scrapling",
			Type:        TypePrompt,
			Description: "Scrapling web scraping framework reference. Use when writing scraping code, crawlers, or spiders with anti-bot bypass.",
		},
		SkillFiles: map[string]string{
			"SKILL.md":                                "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/SKILL.md",
			"examples/01_fetcher_session.py":           "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/examples/01_fetcher_session.py",
			"examples/02_dynamic_session.py":           "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/examples/02_dynamic_session.py",
			"examples/03_stealthy_session.py":          "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/examples/03_stealthy_session.py",
			"examples/04_spider.py":                    "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/examples/04_spider.py",
			"references/mcp-server.md":                 "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/references/mcp-server.md",
			"references/migrating_from_beautifulsoup.md": "https://raw.githubusercontent.com/D4Vinci/Scrapling/main/agent-skill/Scrapling-Skill/references/migrating_from_beautifulsoup.md",
		},
	},
	{
		Extension: Extension{
			Name:        "humanizer",
			Type:        TypePrompt,
			Description: "Remove signs of AI-generated writing from text. Detects and fixes 29 patterns from Wikipedia's AI writing guide. Supports voice calibration from writing samples.",
		},
		SkillFiles: map[string]string{
			"SKILL.md": "https://raw.githubusercontent.com/blader/humanizer/main/SKILL.md",
		},
	},
}

// EnsureDefaults pre-seeds default extensions on first startup.
// Skips extensions that are already registered. Downloads skill files
// for prompt extensions. Non-fatal: logs warnings on failure.
func EnsureDefaults(reg *Registry, wrappersDir string) {
	for _, def := range defaultExtensions {
		if reg.Get(def.Name) != nil {
			continue // already installed
		}

		ext := def.Extension
		ext.AddedAt = time.Now()

		// For prompt skills, download files first.
		if ext.Type == TypePrompt && len(def.SkillFiles) > 0 {
			skillDir := filepath.Join(wrappersDir, ext.Name)
			if err := downloadSkillFiles(skillDir, def.SkillFiles); err != nil {
				slog.Warn("extension: failed to download default skill files",
					"name", ext.Name, "err", err)
				continue
			}
			ext.Command = skillDir
		}

		if err := reg.Add(ext); err != nil {
			slog.Warn("extension: failed to add default extension",
				"name", ext.Name, "err", err)
			continue
		}
		slog.Info("extension: default extension added", "name", ext.Name, "type", ext.Type)
	}
}

// downloadSkillFiles fetches files from URLs and saves them to the skill directory.
func downloadSkillFiles(dir string, files map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for relPath, url := range files {
		fullPath := filepath.Join(dir, relPath)

		// Skip if already downloaded.
		if _, err := os.Stat(fullPath); err == nil {
			continue
		}

		// Create parent directories.
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("create dir for %s: %w", relPath, err)
		}

		// Validate URL (must be GitHub raw content).
		if !strings.HasPrefix(url, "https://raw.githubusercontent.com/") {
			return fmt.Errorf("refusing to download from non-GitHub URL: %s", url)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create request for %s: %w", relPath, err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("download %s: %w", relPath, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("download %s: HTTP %d", relPath, resp.StatusCode)
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", relPath, err)
		}

		if err := os.WriteFile(fullPath, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", relPath, err)
		}
	}
	return nil
}

// IsDefault returns true if the named extension is a pre-installed default.
func IsDefault(name string) bool {
	for _, def := range defaultExtensions {
		if def.Name == name {
			return true
		}
	}
	return false
}

func init() {
	// Validate defaultExtensions at init time.
	for _, def := range defaultExtensions {
		if def.Name == "" || def.Command == "" && def.Type != TypePrompt {
			panic(fmt.Sprintf("invalid default extension: %+v", def))
		}
		if def.Type == TypePrompt && len(def.SkillFiles) == 0 {
			panic(fmt.Sprintf("prompt extension %q has no skill files", def.Name))
		}
		// Prompt extensions get their Command set at runtime (after download).
		if def.Type == TypePrompt {
			for _, url := range def.SkillFiles {
				if !strings.HasPrefix(url, "https://raw.githubusercontent.com/") {
					panic(fmt.Sprintf("non-GitHub URL in default extension %q: %s", def.Name, url))
				}
			}
		}
	}
}

