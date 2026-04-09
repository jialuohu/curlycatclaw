package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ServiceSpec describes a companion service managed by the updater.
type ServiceSpec struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Ports       map[string]string `json:"ports,omitempty"`       // host:container
	Volumes     map[string]string `json:"volumes,omitempty"`     // volume-name:container-path
	Env         map[string]string `json:"env,omitempty"`
	Healthcheck *Healthcheck      `json:"healthcheck,omitempty"`
	AddedAt     time.Time         `json:"added_at"`
}

// Healthcheck defines a Docker healthcheck for a managed service.
type Healthcheck struct {
	Test     []string `json:"test"`
	Interval string   `json:"interval,omitempty"` // default "10s"
	Timeout  string   `json:"timeout,omitempty"`  // default "5s"
	Retries  int      `json:"retries,omitempty"`  // default 3
}

// ServiceCatalog manages a persistent set of companion service specs.
type ServiceCatalog struct {
	mu       sync.RWMutex
	services map[string]*ServiceSpec
	path     string // e.g. /data/managed-services.json
}

// persistedCatalog is the JSON structure written to disk.
type persistedCatalog struct {
	Services []*ServiceSpec `json:"services"`
}

// LoadServiceCatalog reads the catalog from the given JSON file path.
// If the file does not exist, an empty catalog is returned.
func LoadServiceCatalog(path string) (*ServiceCatalog, error) {
	c := &ServiceCatalog{
		services: make(map[string]*ServiceSpec),
		path:     path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("catalog: read: %w", err)
	}

	var pf persistedCatalog
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}

	for _, spec := range pf.Services {
		c.services[spec.Name] = spec
	}
	return c, nil
}

// Add validates and stores a service spec, persisting to disk atomically.
func (c *ServiceCatalog) Add(spec ServiceSpec) error {
	if err := validateServiceSpec(spec); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.services[spec.Name]; exists {
		return fmt.Errorf("catalog: service %q already exists", spec.Name)
	}

	if spec.AddedAt.IsZero() {
		spec.AddedAt = time.Now()
	}

	c.services[spec.Name] = &spec
	if err := c.persistLocked(); err != nil {
		delete(c.services, spec.Name)
		return err
	}
	return nil
}

// Remove deletes a service by name and persists the change.
func (c *ServiceCatalog) Remove(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	spec, exists := c.services[name]
	if !exists {
		return fmt.Errorf("catalog: service %q not found", name)
	}

	delete(c.services, name)
	if err := c.persistLocked(); err != nil {
		c.services[name] = spec // rollback
		return err
	}
	return nil
}

// Get returns a copy of the service spec, or nil if not found.
func (c *ServiceCatalog) Get(name string) *ServiceSpec {
	c.mu.RLock()
	defer c.mu.RUnlock()

	spec := c.services[name]
	if spec == nil {
		return nil
	}
	cp := copyServiceSpec(spec)
	return &cp
}

// All returns all service specs sorted by AddedAt (oldest first).
func (c *ServiceCatalog) All() []*ServiceSpec {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*ServiceSpec, 0, len(c.services))
	for _, spec := range c.services {
		cp := copyServiceSpec(spec)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AddedAt.Before(out[j].AddedAt)
	})
	return out
}

// persistLocked writes the catalog to disk atomically using a temp file
// and rename. Must be called with c.mu held.
func (c *ServiceCatalog) persistLocked() error {
	pf := persistedCatalog{
		Services: make([]*ServiceSpec, 0, len(c.services)),
	}
	for _, spec := range c.services {
		pf.Services = append(pf.Services, spec)
	}
	sort.Slice(pf.Services, func(i, j int) bool {
		return pf.Services[i].AddedAt.Before(pf.Services[j].AddedAt)
	})

	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return fmt.Errorf("catalog: marshal: %w", err)
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("catalog: create dir: %w", err)
	}

	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("catalog: write tmp: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("catalog: rename: %w", err)
	}
	return nil
}

// validateServiceSpec checks that a ServiceSpec has valid fields.
func validateServiceSpec(spec ServiceSpec) error {
	if spec.Name == "" {
		return errors.New("catalog: name is required")
	}
	if !isValidServiceName(spec.Name) {
		return fmt.Errorf("catalog: name %q must match [a-zA-Z0-9][a-zA-Z0-9_-]* and be at most 64 chars", spec.Name)
	}
	if spec.Image == "" {
		return errors.New("catalog: image is required")
	}
	return nil
}

// copyServiceSpec returns a deep copy of a ServiceSpec (maps are reference types).
func copyServiceSpec(spec *ServiceSpec) ServiceSpec {
	cp := *spec
	if spec.Ports != nil {
		cp.Ports = make(map[string]string, len(spec.Ports))
		for k, v := range spec.Ports {
			cp.Ports[k] = v
		}
	}
	if spec.Volumes != nil {
		cp.Volumes = make(map[string]string, len(spec.Volumes))
		for k, v := range spec.Volumes {
			cp.Volumes[k] = v
		}
	}
	if spec.Env != nil {
		cp.Env = make(map[string]string, len(spec.Env))
		for k, v := range spec.Env {
			cp.Env[k] = v
		}
	}
	if spec.Healthcheck != nil {
		hc := *spec.Healthcheck
		if spec.Healthcheck.Test != nil {
			hc.Test = make([]string, len(spec.Healthcheck.Test))
			copy(hc.Test, spec.Healthcheck.Test)
		}
		cp.Healthcheck = &hc
	}
	return cp
}
