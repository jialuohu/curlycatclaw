package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCatalogLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}
	if got := cat.All(); len(got) != 0 {
		t.Errorf("All() returned %d entries, want 0", len(got))
	}
}

func TestCatalogAddAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	spec := ServiceSpec{
		Name:  "redis",
		Image: "redis:7-alpine",
		Ports: map[string]string{"6379": "6379"},
		Env:   map[string]string{"REDIS_ARGS": "--maxmemory 256mb"},
	}
	if err := cat.Add(spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Verify file was created on disk.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("catalog file not created: %v", err)
	}

	// Reload from disk and verify round-trip.
	cat2, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("reload LoadServiceCatalog: %v", err)
	}

	all := cat2.All()
	if len(all) != 1 {
		t.Fatalf("All() returned %d entries after reload, want 1", len(all))
	}
	if all[0].Name != "redis" {
		t.Errorf("Name = %q, want %q", all[0].Name, "redis")
	}
	if all[0].Image != "redis:7-alpine" {
		t.Errorf("Image = %q, want %q", all[0].Image, "redis:7-alpine")
	}
	if all[0].Ports["6379"] != "6379" {
		t.Errorf("Ports[6379] = %q, want %q", all[0].Ports["6379"], "6379")
	}
	if all[0].Env["REDIS_ARGS"] != "--maxmemory 256mb" {
		t.Errorf("Env[REDIS_ARGS] = %q, want %q", all[0].Env["REDIS_ARGS"], "--maxmemory 256mb")
	}
	if all[0].AddedAt.IsZero() {
		t.Error("AddedAt is zero, want populated timestamp")
	}
}

func TestCatalogAddDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	spec := ServiceSpec{Name: "redis", Image: "redis:7"}
	if err := cat.Add(spec); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	err = cat.Add(spec)
	if err == nil {
		t.Fatal("second Add should return error for duplicate")
	}
}

func TestCatalogRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	spec := ServiceSpec{Name: "redis", Image: "redis:7"}
	if err := cat.Add(spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := cat.Remove("redis"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if got := cat.Get("redis"); got != nil {
		t.Errorf("Get after Remove returned non-nil: %+v", got)
	}

	// Remove non-existent should error.
	if err := cat.Remove("nonexistent"); err == nil {
		t.Error("Remove nonexistent should return error")
	}

	// Verify persistence after Remove.
	cat2, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cat2.All()) != 0 {
		t.Errorf("All() after reload = %d, want 0", len(cat2.All()))
	}
}

func TestCatalogGetReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	spec := ServiceSpec{
		Name:    "redis",
		Image:   "redis:7",
		Ports:   map[string]string{"6379": "6379"},
		Volumes: map[string]string{"redis-data": "/data"},
		Env:     map[string]string{"FOO": "bar"},
	}
	if err := cat.Add(spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got := cat.Get("redis")
	if got == nil {
		t.Fatal("Get returned nil")
	}

	// Mutate the returned copy and verify the original is unchanged.
	got.Image = "modified"
	got.Ports["9999"] = "9999"
	got.Volumes["extra"] = "/extra"
	got.Env["EXTRA"] = "value"

	original := cat.Get("redis")
	if original.Image != "redis:7" {
		t.Errorf("original Image mutated to %q", original.Image)
	}
	if _, ok := original.Ports["9999"]; ok {
		t.Error("original Ports map was mutated")
	}
	if _, ok := original.Volumes["extra"]; ok {
		t.Error("original Volumes map was mutated")
	}
	if _, ok := original.Env["EXTRA"]; ok {
		t.Error("original Env map was mutated")
	}
}

func TestCatalogNameValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	tests := []struct {
		name    string
		spec    ServiceSpec
		wantErr bool
	}{
		{"valid", ServiceSpec{Name: "redis", Image: "redis:7"}, false},
		{"valid with dash", ServiceSpec{Name: "my-service", Image: "img:1"}, false},
		{"valid with underscore", ServiceSpec{Name: "my_svc", Image: "img:1"}, false},
		{"empty name", ServiceSpec{Name: "", Image: "img:1"}, true},
		{"starts with dash", ServiceSpec{Name: "-bad", Image: "img:1"}, true},
		{"starts with underscore", ServiceSpec{Name: "_bad", Image: "img:1"}, true},
		{"shell injection", ServiceSpec{Name: "svc;rm -rf /", Image: "img:1"}, true},
		{"spaces", ServiceSpec{Name: "svc name", Image: "img:1"}, true},
		{"too long", ServiceSpec{Name: string(make([]byte, 65)), Image: "img:1"}, true},
		{"empty image", ServiceSpec{Name: "svc", Image: ""}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a fresh catalog to avoid duplicate errors.
			freshCat, err := LoadServiceCatalog(filepath.Join(t.TempDir(), "cat.json"))
			if err != nil {
				t.Fatalf("LoadServiceCatalog: %v", err)
			}
			err = freshCat.Add(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Errorf("Add(%q) error = %v, wantErr = %v", tt.spec.Name, err, tt.wantErr)
			}
		})
	}

	// Ensure the first valid add from the table didn't leak into later tests.
	_ = cat
}

func TestCatalogAllSortedByAddedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	now := time.Now()
	specs := []ServiceSpec{
		{Name: "charlie", Image: "img:3", AddedAt: now.Add(2 * time.Second)},
		{Name: "alpha", Image: "img:1", AddedAt: now},
		{Name: "bravo", Image: "img:2", AddedAt: now.Add(1 * time.Second)},
	}
	for _, s := range specs {
		if err := cat.Add(s); err != nil {
			t.Fatalf("Add(%s): %v", s.Name, err)
		}
	}

	all := cat.All()
	if len(all) != 3 {
		t.Fatalf("All() returned %d, want 3", len(all))
	}
	if all[0].Name != "alpha" {
		t.Errorf("[0].Name = %q, want alpha", all[0].Name)
	}
	if all[1].Name != "bravo" {
		t.Errorf("[1].Name = %q, want bravo", all[1].Name)
	}
	if all[2].Name != "charlie" {
		t.Errorf("[2].Name = %q, want charlie", all[2].Name)
	}
}

func TestCatalogGetNonexistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	if got := cat.Get("nonexistent"); got != nil {
		t.Errorf("Get(nonexistent) = %+v, want nil", got)
	}
}

func TestCatalogHealthcheckCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat, err := LoadServiceCatalog(path)
	if err != nil {
		t.Fatalf("LoadServiceCatalog: %v", err)
	}

	spec := ServiceSpec{
		Name:  "redis",
		Image: "redis:7",
		Healthcheck: &Healthcheck{
			Test:     []string{"CMD", "redis-cli", "ping"},
			Interval: "10s",
			Timeout:  "5s",
			Retries:  3,
		},
	}
	if err := cat.Add(spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got := cat.Get("redis")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Healthcheck == nil {
		t.Fatal("Healthcheck is nil")
	}

	// Mutate returned healthcheck.
	got.Healthcheck.Test[0] = "MUTATED"
	got.Healthcheck.Retries = 99

	original := cat.Get("redis")
	if original.Healthcheck.Test[0] != "CMD" {
		t.Errorf("original healthcheck Test[0] mutated to %q", original.Healthcheck.Test[0])
	}
	if original.Healthcheck.Retries != 3 {
		t.Errorf("original healthcheck Retries mutated to %d", original.Healthcheck.Retries)
	}
}
