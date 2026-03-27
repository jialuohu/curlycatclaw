package skills

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	s := &Skill{
		Name:        "test_skill",
		Description: "A test skill",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	r.Register(s)
	got := r.Get("test_skill")

	if got == nil {
		t.Fatal("expected to get registered skill, got nil")
	}
	if got.Name != "test_skill" {
		t.Errorf("expected name %q, got %q", "test_skill", got.Name)
	}
	if got.Description != "A test skill" {
		t.Errorf("expected description %q, got %q", "A test skill", got.Description)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	r := NewRegistry()
	got := r.Get("nonexistent")

	if got != nil {
		t.Errorf("expected nil for unknown skill, got %+v", got)
	}
}

func TestRegistry_All(t *testing.T) {
	r := NewRegistry()

	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		r.Register(&Skill{
			Name:        name,
			Description: "Skill " + name,
			InputSchema: json.RawMessage(`{}`),
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				return "", nil
			},
		})
	}

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(all))
	}

	// Verify all names are present (order is not guaranteed with maps).
	found := make(map[string]bool)
	for _, s := range all {
		found[s.Name] = true
	}
	for _, name := range names {
		if !found[name] {
			t.Errorf("expected skill %q in All() results", name)
		}
	}
}
