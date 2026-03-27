package skills

import (
	"context"
	"encoding/json"
)

type ctxKey string

const userCtxKey ctxKey = "skill_user"

// UserInfo identifies the caller of a skill for user-scoped data.
type UserInfo struct {
	UserID int64
	ChatID int64
}

// WithUser attaches user identity to a context for skill execution.
func WithUser(ctx context.Context, u UserInfo) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// GetUser retrieves user identity from context. Returns zero UserInfo if absent.
func GetUser(ctx context.Context) UserInfo {
	if u, ok := ctx.Value(userCtxKey).(UserInfo); ok {
		return u
	}
	return UserInfo{}
}

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

// Unregister removes a skill from the registry by name.
func (r *Registry) Unregister(name string) {
	delete(r.skills, name)
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
