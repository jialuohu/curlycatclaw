package skillloader

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/skills"
)

const defaultExecTimeout = 30 * time.Second

// SkillDescriptor is parsed from each skill's skill.toml file.
type SkillDescriptor struct {
	Name        string     `toml:"name"`
	Description string     `toml:"description"`
	Type        string     `toml:"type"` // "exec" only for now
	Timeout     string     `toml:"timeout"`
	Exec        ExecConfig `toml:"exec"`
	InputSchema string     `toml:"input_schema"` // JSON Schema as string (TOML stores it as a string)
}

// ExecConfig holds the command and environment for an exec skill.
type ExecConfig struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
}

// CollectionMeta is parsed from collection.toml (optional).
type CollectionMeta struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
}

// Collection groups skills loaded from a single directory.
type Collection struct {
	Name   string
	Path   string
	Skills map[string]*LoadedSkill
}

// LoadedSkill pairs a descriptor with its runtime adapter.
type LoadedSkill struct {
	Descriptor SkillDescriptor
	Adapter    SkillAdapter
}

// Loader discovers, loads, and manages external skill collections.
type Loader struct {
	registry    *skills.Registry
	collections map[string]*Collection
	mu          sync.RWMutex
}

// New creates a Loader backed by the given skill registry.
func New(registry *skills.Registry) *Loader {
	return &Loader{
		registry:    registry,
		collections: make(map[string]*Collection),
	}
}

// LoadAll loads all configured skill collections.
func (l *Loader) LoadAll(ctx context.Context, configs []config.SkillCollectionConfig) error {
	var errs []string
	for _, cfg := range configs {
		if err := l.loadCollection(ctx, cfg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", cfg.Path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("skill collections: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (l *Loader) loadCollection(_ context.Context, cfg config.SkillCollectionConfig) error {
	absPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// Determine collection name: config namespace > collection.toml name > directory name.
	namespace := cfg.Namespace
	metaPath := filepath.Join(absPath, "collection.toml")
	var meta CollectionMeta
	if data, err := os.ReadFile(metaPath); err == nil {
		if err := toml.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("parse collection.toml: %w", err)
		}
	}
	if namespace == "" && meta.Name != "" {
		namespace = meta.Name
	}
	if namespace == "" {
		namespace = filepath.Base(absPath)
	}

	col := &Collection{
		Name:   namespace,
		Path:   absPath,
		Skills: make(map[string]*LoadedSkill),
	}

	// Walk immediate subdirectories for skill.toml files.
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return fmt.Errorf("read collection dir: %w", err)
	}

	loaded := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(absPath, entry.Name())
		skillToml := filepath.Join(skillDir, "skill.toml")
		if _, err := os.Stat(skillToml); os.IsNotExist(err) {
			continue
		}

		ls, err := l.loadSkill(skillDir, namespace)
		if err != nil {
			slog.Warn("skillloader: failed to load skill",
				"dir", skillDir, "err", err)
			continue
		}
		col.Skills[ls.Descriptor.Name] = ls
		loaded++
	}

	l.mu.Lock()
	l.collections[namespace] = col
	l.mu.Unlock()

	slog.Info("skillloader: collection loaded",
		"namespace", namespace, "path", absPath, "skills", loaded)
	return nil
}

// loadSkill parses skill.toml, creates an adapter, and registers in the
// skill registry.
func (l *Loader) loadSkill(skillDir, namespace string) (*LoadedSkill, error) {
	skillToml := filepath.Join(skillDir, "skill.toml")
	data, err := os.ReadFile(skillToml)
	if err != nil {
		return nil, fmt.Errorf("read skill.toml: %w", err)
	}

	var desc SkillDescriptor
	if err := toml.Unmarshal(data, &desc); err != nil {
		return nil, fmt.Errorf("parse skill.toml: %w", err)
	}

	if desc.Type != "exec" {
		return nil, fmt.Errorf("unsupported skill type %q (only \"exec\" is supported)", desc.Type)
	}
	if desc.Name == "" {
		return nil, fmt.Errorf("skill.toml missing required field: name")
	}
	if desc.Exec.Command == "" {
		return nil, fmt.Errorf("skill.toml missing required field: exec.command")
	}

	// Resolve command relative to skill directory.
	command := desc.Exec.Command
	if !filepath.IsAbs(command) {
		command = filepath.Join(skillDir, command)
	}

	absCommand, err := filepath.Abs(command)
	if err != nil {
		return nil, fmt.Errorf("resolve command path: %w", err)
	}
	absSkillDir, _ := filepath.Abs(skillDir)
	if !strings.HasPrefix(absCommand, absSkillDir+string(filepath.Separator)) && absCommand != absSkillDir {
		return nil, fmt.Errorf("command %q resolves outside skill directory %q", desc.Exec.Command, skillDir)
	}
	command = absCommand

	// Verify the executable exists and is executable.
	info, err := os.Stat(command)
	if err != nil {
		return nil, fmt.Errorf("executable not found: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("executable %q is a directory", command)
	}
	if info.Mode()&0111 == 0 {
		return nil, fmt.Errorf("file %q is not executable", command)
	}

	timeout := defaultExecTimeout
	if desc.Timeout != "" {
		d, err := time.ParseDuration(desc.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout %q: %w", desc.Timeout, err)
		}
		timeout = d
	}

	adapter := NewExecAdapter(command, desc.Exec.Args, skillDir, desc.Exec.Env, timeout)

	// Build the registry name: namespace__skillname.
	registryName := namespace + "__" + desc.Name

	// Build input schema (default to empty object if not specified).
	var inputSchema json.RawMessage
	if desc.InputSchema != "" {
		if !json.Valid([]byte(desc.InputSchema)) {
			return nil, fmt.Errorf("skill.toml: input_schema is not valid JSON")
		}
		inputSchema = json.RawMessage(desc.InputSchema)
	} else {
		inputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	}

	// Register as a skills.Skill.
	l.registry.Register(&skills.Skill{
		Name:        registryName,
		Description: desc.Description,
		InputSchema: inputSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			user := skills.GetUser(ctx)
			return adapter.Execute(ctx, input, user)
		},
	})

	slog.Info("skillloader: skill registered",
		"name", registryName, "command", command)

	return &LoadedSkill{
		Descriptor: desc,
		Adapter:    adapter,
	}, nil
}

// unregisterSkill removes a skill from both the collection and the registry.
func (l *Loader) unregisterSkill(namespace, skillName string) {
	registryName := namespace + "__" + skillName
	l.registry.Unregister(registryName)

	l.mu.Lock()
	defer l.mu.Unlock()
	if col, ok := l.collections[namespace]; ok {
		if ls, ok := col.Skills[skillName]; ok {
			ls.Adapter.Stop() //nolint:errcheck
			delete(col.Skills, skillName)
		}
	}
	slog.Info("skillloader: skill unregistered", "name", registryName)
}

// WatchForChanges monitors all collection directories for skill.toml
// changes and hot-reloads affected skills. It blocks until ctx is cancelled.
func (l *Loader) WatchForChanges(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("skillloader: create watcher: %w", err)
	}
	defer watcher.Close()

	l.mu.RLock()
	for _, col := range l.collections {
		// Watch each skill subdirectory for skill.toml changes.
		entries, err := os.ReadDir(col.Path)
		if err != nil {
			l.mu.RUnlock()
			return fmt.Errorf("skillloader: read dir %s: %w", col.Path, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				skillDir := filepath.Join(col.Path, entry.Name())
				watcher.Add(skillDir) //nolint:errcheck
			}
		}
		// Also watch the collection root for new skill directories.
		watcher.Add(col.Path) //nolint:errcheck
	}
	l.mu.RUnlock()

	slog.Info("skillloader: watching for changes")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			l.handleFSEvent(event, watcher)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("skillloader: watcher error", "err", err)
		}
	}
}

// handleFSEvent processes a single filesystem event.
func (l *Loader) handleFSEvent(event fsnotify.Event, watcher *fsnotify.Watcher) {
	// Determine if this is a skill.toml event.
	base := filepath.Base(event.Name)

	if base == "skill.toml" {
		skillDir := filepath.Dir(event.Name)
		namespace, skillName := l.findSkillContext(skillDir)
		if namespace == "" {
			return
		}

		if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
			slog.Info("skillloader: skill.toml changed, reloading",
				"dir", skillDir)
			l.unregisterSkill(namespace, skillName)
			if ls, err := l.loadSkill(skillDir, namespace); err != nil {
				slog.Warn("skillloader: reload failed",
					"dir", skillDir, "err", err)
			} else {
				l.mu.Lock()
				if col, ok := l.collections[namespace]; ok {
					col.Skills[ls.Descriptor.Name] = ls
				}
				l.mu.Unlock()
			}
		}

		if event.Op&fsnotify.Remove != 0 {
			slog.Info("skillloader: skill.toml removed, unregistering",
				"dir", skillDir)
			l.unregisterSkill(namespace, skillName)
		}
		return
	}

	// New directory created inside a collection: watch it and try loading.
	if event.Op&fsnotify.Create != 0 {
		info, err := os.Stat(event.Name)
		if err != nil || !info.IsDir() {
			return
		}
		// Check if the parent is a collection path.
		parentDir := filepath.Dir(event.Name)
		l.mu.RLock()
		var matchedNS string
		for ns, col := range l.collections {
			if col.Path == parentDir {
				matchedNS = ns
				break
			}
		}
		l.mu.RUnlock()

		if matchedNS != "" {
			watcher.Add(event.Name) //nolint:errcheck
			// Try loading if skill.toml exists.
			skillToml := filepath.Join(event.Name, "skill.toml")
			if _, err := os.Stat(skillToml); err == nil {
				if ls, err := l.loadSkill(event.Name, matchedNS); err != nil {
					slog.Warn("skillloader: load new skill failed",
						"dir", event.Name, "err", err)
				} else {
					l.mu.Lock()
					if col, ok := l.collections[matchedNS]; ok {
						col.Skills[ls.Descriptor.Name] = ls
					}
					l.mu.Unlock()
				}
			}
		}
	}
}

// findSkillContext maps a skill directory to its namespace and skill name.
func (l *Loader) findSkillContext(skillDir string) (namespace, skillName string) {
	parentDir := filepath.Dir(skillDir)
	dirName := filepath.Base(skillDir)

	l.mu.RLock()
	defer l.mu.RUnlock()

	for ns, col := range l.collections {
		if col.Path == parentDir {
			// Find the loaded skill whose adapter directory matches.
			// The skill name in the descriptor may differ from the
			// directory name, so match by adapter working directory.
			for sn, ls := range col.Skills {
				if ea, ok := ls.Adapter.(*ExecAdapter); ok && ea.dir == skillDir {
					return ns, sn
				}
			}
			// No loaded skill found, but this is still a valid collection.
			// Return namespace with the directory name as a fallback.
			return ns, dirName
		}
	}
	return "", ""
}

// Shutdown stops all adapters and clears the registry entries.
func (l *Loader) Shutdown() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for ns, col := range l.collections {
		for name, ls := range col.Skills {
			registryName := ns + "__" + name
			l.registry.Unregister(registryName)
			ls.Adapter.Stop() //nolint:errcheck
		}
	}
	l.collections = make(map[string]*Collection)
	return nil
}
