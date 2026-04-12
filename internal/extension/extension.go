// Package extension provides a runtime extension registry for dynamically
// adding and removing MCP servers and exec-based skills via chat.
// Extensions are persisted to a JSON file and survive daemon restarts.
package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Type distinguishes between MCP server and exec-based skill extensions.
type Type string

const (
	TypeMCP    Type = "mcp"
	TypeExec   Type = "exec"
	TypePrompt Type = "prompt"
)

// Extension represents a runtime-added MCP server, exec-based skill, or prompt skill.
type Extension struct {
	Name        string            `json:"name"`
	Type        Type              `json:"type"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	InputSchema json.RawMessage   `json:"input_schema,omitempty"`
	AddedAt     time.Time         `json:"added_at"`
	// HTTP transport fields (MCP type only).
	Transport string            `json:"transport,omitempty"` // "" or "stdio" (default), "http"
	URL       string            `json:"url,omitempty"`       // required when transport is "http"
	Headers   map[string]string `json:"headers,omitempty"`   // HTTP request headers
	// Companion Docker service fields (HTTP MCP extensions only).
	// When set, add_extension can auto-register and start the service.
	Image string            `json:"image,omitempty"` // Docker image for managed service
	Ports map[string]string `json:"ports,omitempty"` // host:container port mappings
}

// Registry manages a persistent set of runtime extensions.
type Registry struct {
	mu         sync.RWMutex
	extensions map[string]*Extension
	path       string
}

// namePattern allows alphanumeric characters, hyphens, and underscores.
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// persistedFile is the JSON structure written to disk.
type persistedFile struct {
	Extensions []*Extension `json:"extensions"`
}

// Empty creates an empty registry that will persist to the given path.
func Empty(path string) *Registry {
	return &Registry{
		extensions: make(map[string]*Extension),
		path:       path,
	}
}

// Load reads the extension registry from the given JSON file path.
// If the file does not exist, an empty registry is returned.
func Load(path string) (*Registry, error) {
	r := &Registry{
		extensions: make(map[string]*Extension),
		path:       path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return nil, fmt.Errorf("extension: read registry: %w", err)
	}

	var pf persistedFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("extension: parse registry: %w", err)
	}

	for _, ext := range pf.Extensions {
		r.extensions[ext.Name] = ext
	}
	return r, nil
}

// Add validates and stores an extension, persisting to disk atomically.
// The caller is responsible for starting MCP servers or registering skills
// before calling Add, so that failures can be rolled back without stale
// persistence.
func (r *Registry) Add(ext Extension) error {
	if err := validate(ext); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.extensions[ext.Name]; exists {
		return fmt.Errorf("extension: %q already exists", ext.Name)
	}

	if ext.AddedAt.IsZero() {
		ext.AddedAt = time.Now()
	}

	r.extensions[ext.Name] = &ext
	if err := r.persistLocked(); err != nil {
		delete(r.extensions, ext.Name)
		return err
	}
	return nil
}

// Remove deletes an extension by name and persists the change.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ext, exists := r.extensions[name]
	if !exists {
		return fmt.Errorf("extension: %q not found", name)
	}

	delete(r.extensions, name)
	if err := r.persistLocked(); err != nil {
		r.extensions[name] = ext // rollback
		return err
	}
	return nil
}

// Update modifies an existing extension in place via a mutate callback and
// persists the result atomically. Returns an error if the extension is not
// found or if persistence fails (in which case the change is rolled back).
func (r *Registry) Update(name string, mutate func(*Extension)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ext, exists := r.extensions[name]
	if !exists {
		return fmt.Errorf("extension: %q not found", name)
	}

	// Deep snapshot for rollback (maps and slices are reference types).
	snapshot := *ext
	if ext.Env != nil {
		snapshot.Env = make(map[string]string, len(ext.Env))
		for k, v := range ext.Env {
			snapshot.Env[k] = v
		}
	}
	if ext.Args != nil {
		snapshot.Args = make([]string, len(ext.Args))
		copy(snapshot.Args, ext.Args)
	}
	if ext.Headers != nil {
		snapshot.Headers = make(map[string]string, len(ext.Headers))
		for k, v := range ext.Headers {
			snapshot.Headers[k] = v
		}
	}
	mutate(ext)

	if err := r.persistLocked(); err != nil {
		*ext = snapshot // rollback
		return err
	}
	return nil
}

// Get returns the extension with the given name, or nil if not found.
func (r *Registry) Get(name string) *Extension {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ext := r.extensions[name]
	if ext == nil {
		return nil
	}
	copy := *ext
	return &copy
}

// All returns all extensions sorted by AddedAt (oldest first).
func (r *Registry) All() []*Extension {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Extension, 0, len(r.extensions))
	for _, ext := range r.extensions {
		copy := *ext
		out = append(out, &copy)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AddedAt.Before(out[j].AddedAt)
	})
	return out
}

// ByType returns extensions of the given type, sorted by AddedAt.
func (r *Registry) ByType(t Type) []*Extension {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*Extension
	for _, ext := range r.extensions {
		if ext.Type == t {
			copy := *ext
			out = append(out, &copy)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AddedAt.Before(out[j].AddedAt)
	})
	return out
}

// maxNameLen is the maximum allowed length for extension names.
const maxNameLen = 128

// validate checks extension fields for correctness.
func validate(ext Extension) error {
	if ext.Name == "" {
		return errors.New("extension: name is required")
	}
	if len(ext.Name) > maxNameLen {
		return fmt.Errorf("extension: name exceeds %d characters", maxNameLen)
	}
	if !namePattern.MatchString(ext.Name) {
		return fmt.Errorf("extension: name %q must be alphanumeric with hyphens/underscores", ext.Name)
	}
	if ext.Type != TypeMCP && ext.Type != TypeExec && ext.Type != TypePrompt {
		return fmt.Errorf("extension: type must be %q, %q, or %q, got %q", TypeMCP, TypeExec, TypePrompt, ext.Type)
	}
	switch ext.Type {
	case TypeMCP:
		switch ext.Transport {
		case "", "stdio":
			if ext.Command == "" {
				return errors.New("extension: command is required for stdio MCP servers")
			}
			if ext.URL != "" {
				return errors.New("extension: url is not allowed for stdio transport")
			}
		case "http":
			if ext.URL == "" {
				return errors.New("extension: url is required for http transport")
			}
			u, err := url.Parse(ext.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("extension: url must be http:// or https:// with a host, got %q", ext.URL)
			}
			if ext.Command != "" {
				return errors.New("extension: command is not allowed for http transport")
			}
		default:
			return fmt.Errorf("extension: transport must be \"\", \"stdio\", or \"http\", got %q", ext.Transport)
		}
	case TypeExec:
		if ext.Command == "" {
			return errors.New("extension: command is required")
		}
		if ext.Description == "" {
			return errors.New("extension: description is required for exec extensions")
		}
	case TypePrompt:
		if ext.Command == "" {
			return errors.New("extension: command is required")
		}
		if ext.Description == "" {
			return errors.New("extension: description is required for prompt skills")
		}
		skillPath := filepath.Join(ext.Command, "SKILL.md")
		if _, err := os.Stat(skillPath); err != nil {
			return fmt.Errorf("extension: prompt skill directory must contain SKILL.md: %w", err)
		}
	}
	return nil
}

// persistLocked writes the registry to disk atomically using a temp file
// and rename. Must be called with r.mu held.
func (r *Registry) persistLocked() error {
	pf := persistedFile{
		Extensions: make([]*Extension, 0, len(r.extensions)),
	}
	for _, ext := range r.extensions {
		pf.Extensions = append(pf.Extensions, ext)
	}
	sort.Slice(pf.Extensions, func(i, j int) bool {
		return pf.Extensions[i].AddedAt.Before(pf.Extensions[j].AddedAt)
	})

	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return fmt.Errorf("extension: marshal: %w", err)
	}

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("extension: create dir: %w", err)
	}

	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("extension: write tmp: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("extension: rename: %w", err)
	}
	return nil
}
