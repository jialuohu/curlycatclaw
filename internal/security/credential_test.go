package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// testKey is a fixed 32-byte master key used across all credential tests.
var testKey = []byte("01234567890123456789012345678901")

// newTestStore creates a CredentialStore backed by a temporary file.
func newTestStore(t *testing.T) *CredentialStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.enc")
	cs, err := NewCredentialStore(path, testKey)
	if err != nil {
		t.Fatalf("NewCredentialStore: %v", err)
	}
	return cs
}

// TestCredentialStore_RoundTrip sets a credential and reads it back.
func TestCredentialStore_RoundTrip(t *testing.T) {
	cs := newTestStore(t)

	if err := cs.Set("api_key", "sk-secret-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := cs.Get("api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-secret-123" {
		t.Errorf("expected %q, got %q", "sk-secret-123", got)
	}
}

// TestCredentialStore_NotFound verifies that Get returns ErrNotFound for a
// key that does not exist.
func TestCredentialStore_NotFound(t *testing.T) {
	cs := newTestStore(t)

	_, err := cs.Get("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCredentialStore_Update sets a key, updates its value, and verifies.
func TestCredentialStore_Update(t *testing.T) {
	cs := newTestStore(t)

	if err := cs.Set("token", "old_value"); err != nil {
		t.Fatalf("Set (initial): %v", err)
	}
	if err := cs.Set("token", "new_value"); err != nil {
		t.Fatalf("Set (update): %v", err)
	}

	got, err := cs.Get("token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "new_value" {
		t.Errorf("expected %q, got %q", "new_value", got)
	}
}

// TestCredentialStore_Delete sets a key, deletes it, then verifies ErrNotFound.
func TestCredentialStore_Delete(t *testing.T) {
	cs := newTestStore(t)

	if err := cs.Set("temp", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cs.Delete("temp"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := cs.Get("temp")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestCredentialStore_DeleteNotFound verifies that deleting a non-existent key
// returns ErrNotFound.
func TestCredentialStore_DeleteNotFound(t *testing.T) {
	cs := newTestStore(t)

	err := cs.Delete("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCredentialStore_ResolveEnv_PlainValues verifies that env maps with no
// encrypted:ref: values pass through unchanged.
func TestCredentialStore_ResolveEnv_PlainValues(t *testing.T) {
	cs := newTestStore(t)

	env := map[string]string{
		"HOME":   "/home/user",
		"EDITOR": "vim",
	}
	resolved, err := cs.ResolveEnv(env)
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}

	for k, want := range env {
		if got := resolved[k]; got != want {
			t.Errorf("key %q: expected %q, got %q", k, want, got)
		}
	}
}

// TestCredentialStore_ResolveEnv_EncryptedRef verifies that encrypted:ref:
// values are resolved from the credential store.
func TestCredentialStore_ResolveEnv_EncryptedRef(t *testing.T) {
	cs := newTestStore(t)

	if err := cs.Set("db_password", "s3cret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	env := map[string]string{
		"DB_HOST":     "localhost",
		"DB_PASSWORD": "encrypted:ref:db_password",
	}
	resolved, err := cs.ResolveEnv(env)
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}

	if got := resolved["DB_HOST"]; got != "localhost" {
		t.Errorf("DB_HOST: expected %q, got %q", "localhost", got)
	}
	if got := resolved["DB_PASSWORD"]; got != "s3cret" {
		t.Errorf("DB_PASSWORD: expected %q, got %q", "s3cret", got)
	}
}

// TestCredentialStore_ResolveEnv_MissingRef verifies that a reference to a
// non-existent credential key returns an error.
func TestCredentialStore_ResolveEnv_MissingRef(t *testing.T) {
	cs := newTestStore(t)

	env := map[string]string{
		"API_KEY": "encrypted:ref:missing_key",
	}
	_, err := cs.ResolveEnv(env)
	if err == nil {
		t.Fatal("expected error for missing ref, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound in error chain, got %v", err)
	}
}

// TestCredentialStore_InvalidKeyLength verifies that NewCredentialStore
// rejects master keys that are not exactly 32 bytes.
func TestCredentialStore_InvalidKeyLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.enc")

	for _, keyLen := range []int{0, 16, 31, 33, 64} {
		key := make([]byte, keyLen)
		_, err := NewCredentialStore(path, key)
		if err == nil {
			t.Errorf("expected error for key length %d, got nil", keyLen)
		}
	}
}

// TestCredentialStore_FilePermissions verifies that the credential file is
// created with 0600 permissions.
func TestCredentialStore_FilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.enc")
	cs, err := NewCredentialStore(path, testKey)
	if err != nil {
		t.Fatalf("NewCredentialStore: %v", err)
	}

	// Write something to ensure the file exists.
	if err := cs.Set("perm_test", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected file permissions 0600, got %04o", perm)
	}
}
