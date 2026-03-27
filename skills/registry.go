package skills

import (
	"context"
	"encoding/json"
)

// Skill defines a built-in tool that Claude can call.
type Skill struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema
	Execute     func(ctx context.Context, input json.RawMessage) (string, error)
}

// Registry holds all built-in skills.
type Registry struct {
	skills map[string]*Skill
}

// NewRegistry creates an empty skill registry.
func NewRegistry() *Registry {
	return &Registry{
		skills: make(map[string]*Skill),
	}
}

// Register adds a skill to the registry.
func (r *Registry) Register(s *Skill) {
	r.skills[s.Name] = s
}

// Get returns a skill by name, or nil if not found.
func (r *Registry) Get(name string) *Skill {
	return r.skills[name]
}

// All returns all registered skills.
func (r *Registry) All() []*Skill {
	result := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		result = append(result, s)
	}
	return result
}
