package ingest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileSource implements Source for local file-based knowledge sources
// (Obsidian vaults, local note directories). It walks a root directory,
// discovers files modified after the cursor timestamp, and reads them
// with YAML front matter parsing.
type FileSource struct {
	name         string
	rootDir      string   // absolute path to vault/note directory
	patterns     []string // glob patterns (e.g., "*.md")
	includePaths []string // prefix-match include patterns
	excludePaths []string // prefix-match exclude patterns
}

// FileSourceConfig holds configuration for a FileSource.
type FileSourceConfig struct {
	Name         string
	RootDir      string
	Patterns     []string
	IncludePaths []string
	ExcludePaths []string
}

func NewFileSource(cfg FileSourceConfig) *FileSource {
	if len(cfg.Patterns) == 0 {
		cfg.Patterns = []string{"*.md"}
	}
	return &FileSource{
		name:         cfg.Name,
		rootDir:      cfg.RootDir,
		patterns:     cfg.Patterns,
		includePaths: cfg.IncludePaths,
		excludePaths: cfg.ExcludePaths,
	}
}

func (s *FileSource) Name() string { return s.name }

// Discover walks the root directory and returns files modified after the cursor.
// Cursor is a JSON-encoded RFC3339 timestamp of the last discovery time.
func (s *FileSource) Discover(_ context.Context, cursor json.RawMessage) ([]ItemRef, json.RawMessage, error) {
	var since time.Time
	if cursor != nil {
		var ts string
		if err := json.Unmarshal(cursor, &ts); err == nil && ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				since = t
			}
		}
	}

	discoveryTime := time.Now().UTC()
	var items []ItemRef

	err := filepath.WalkDir(s.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("ingest: walk error", "path", path, "err", err)
			return nil // continue walking
		}

		if d.IsDir() {
			// Skip hidden directories.
			if strings.HasPrefix(d.Name(), ".") && path != s.rootDir {
				return filepath.SkipDir
			}
			return nil
		}

		// Symlink escape prevention: resolve symlinks and verify
		// the resolved path is still under rootDir.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			slog.Warn("ingest: symlink resolve error", "path", path, "err", err)
			return nil
		}
		resolvedRoot, err := filepath.EvalSymlinks(s.rootDir)
		if err != nil {
			return nil
		}
		if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) && resolved != resolvedRoot {
			slog.Warn("ingest: symlink escape detected, skipping", "path", path, "resolved", resolved)
			return nil
		}

		// Match file patterns.
		matched := false
		for _, pattern := range s.patterns {
			if ok, _ := filepath.Match(pattern, d.Name()); ok {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}

		// Check mtime.
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !since.IsZero() && !info.ModTime().After(since) {
			return nil
		}

		relPath, _ := filepath.Rel(s.rootDir, path)

		items = append(items, ItemRef{
			ID:    relPath,
			Title: strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())),
			Metadata: map[string]string{
				"path":  relPath,
				"mtime": info.ModTime().UTC().Format(time.RFC3339),
			},
		})

		return nil
	})
	if err != nil {
		return nil, cursor, fmt.Errorf("ingest: walk %s: %w", s.rootDir, err)
	}

	newCursor, _ := json.Marshal(discoveryTime.Format(time.RFC3339))
	return items, newCursor, nil
}

// Read reads a file and parses YAML front matter from its content.
func (s *FileSource) Read(_ context.Context, id string) (Content, error) {
	fullPath := filepath.Join(s.rootDir, id)

	// Symlink escape check on read too.
	resolved, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return Content{}, fmt.Errorf("ingest: resolve path: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(s.rootDir)
	if err != nil {
		return Content{}, fmt.Errorf("ingest: resolve root: %w", err)
	}
	if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) && resolved != resolvedRoot {
		return Content{}, fmt.Errorf("ingest: path escapes root directory")
	}

	// Check file size to avoid loading huge files into memory.
	info, err := os.Stat(resolved)
	if err != nil {
		return Content{}, fmt.Errorf("ingest: stat file: %w", err)
	}
	const maxFileSize = 1 << 20 // 1 MB
	if info.Size() > maxFileSize {
		return Content{}, fmt.Errorf("ingest: file too large (%d bytes, max %d)", info.Size(), maxFileSize)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return Content{}, fmt.Errorf("ingest: read file: %w", err)
	}

	body := string(data)
	metadata := make(map[string]string)

	// Parse YAML front matter.
	frontMatter, content, hasFM := parseFrontMatter(body)
	if hasFM {
		body = content
		metadata["has_front_matter"] = "true"
		for k, v := range frontMatter {
			metadata[k] = v
		}
	}

	var mtime string
	if info != nil {
		mtime = info.ModTime().UTC().Format(time.RFC3339)
	}

	// Content fingerprint for mutable-source re-extraction.
	fingerprint := contentHash(id, body)

	title := metadata["title"]
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(id), filepath.Ext(id))
	}

	return Content{
		ID:                 id,
		SourceID:           s.name,
		Title:              title,
		Body:               body,
		Date:               mtime,
		Metadata:           metadata,
		ContentFingerprint: fingerprint,
	}, nil
}

// Prefilter checks include/exclude path patterns.
func (s *FileSource) Prefilter(item ItemRef) bool {
	relPath := item.Metadata["path"]
	if relPath == "" {
		relPath = item.ID
	}

	// Exclude check first.
	for _, exc := range s.excludePaths {
		if strings.HasPrefix(relPath, exc) {
			return false
		}
	}

	// Include check (if configured).
	if len(s.includePaths) > 0 {
		found := false
		for _, inc := range s.includePaths {
			if strings.HasPrefix(relPath, inc) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

// parseFrontMatter extracts YAML front matter from a markdown file.
// Returns the front matter key-value pairs, the remaining body, and
// whether front matter was found.
func parseFrontMatter(content string) (map[string]string, string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return nil, content, false
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	fm := make(map[string]string)

	// Skip leading whitespace and first ---.
	inFrontMatter := false
	var bodyLines []string
	pastFrontMatter := false

	for scanner.Scan() {
		line := scanner.Text()

		if !inFrontMatter && !pastFrontMatter {
			trimmed := strings.TrimSpace(line)
			if trimmed == "---" {
				inFrontMatter = true
				continue
			}
			// No front matter found.
			return nil, content, false
		}

		if inFrontMatter {
			trimmed := strings.TrimSpace(line)
			if trimmed == "---" || trimmed == "..." {
				inFrontMatter = false
				pastFrontMatter = true
				continue
			}
			// Simple YAML key: value parsing (no nested structures).
			if idx := strings.Index(line, ":"); idx > 0 {
				key := strings.TrimSpace(line[:idx])
				val := strings.TrimSpace(line[idx+1:])
				// Strip quotes.
				val = strings.Trim(val, "\"'")
				// Handle YAML arrays like [tag1, tag2].
				if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
					val = val[1 : len(val)-1]
				}
				fm[key] = val
			}
		} else {
			bodyLines = append(bodyLines, line)
		}
	}

	if len(fm) == 0 {
		return nil, content, false
	}

	return fm, strings.Join(bodyLines, "\n"), true
}
