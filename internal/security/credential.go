package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrNotFound is returned when a credential key does not exist in the store.
var ErrNotFound = errors.New("credential not found")

const (
	// nonceSize is the size of the AES-GCM nonce in bytes.
	nonceSize = 12
	// encryptedRefPrefix is the prefix that marks a value as an encrypted
	// credential reference in MCP server environment configs.
	encryptedRefPrefix = "encrypted:ref:"
)

// CredentialStore provides encrypted-at-rest storage for credentials.
// Credentials are kept as key-value pairs serialized to JSON and encrypted
// with AES-256-GCM. The file format is: nonce (12 bytes) || ciphertext.
type CredentialStore struct {
	path string   // path to credentials.enc file
	key  [32]byte // AES-256-GCM encryption key
}

// NewCredentialStore creates a CredentialStore backed by the file at path,
// encrypted with the given 32-byte master key. If the file does not exist
// it will be created (with an empty credential set) on the first write.
func NewCredentialStore(path string, masterKey []byte) (*CredentialStore, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be exactly 32 bytes, got %d", len(masterKey))
	}

	cs := &CredentialStore{path: path}
	copy(cs.key[:], masterKey)

	// If the file does not exist yet, write an empty credential set so the
	// file is ready for subsequent reads.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := cs.writeCredentials(make(map[string]string)); err != nil {
			return nil, fmt.Errorf("initializing credential file: %w", err)
		}
	}

	return cs, nil
}

// Get retrieves a credential by name. It returns ErrNotFound if the key
// does not exist.
func (cs *CredentialStore) Get(name string) (string, error) {
	creds, err := cs.readCredentials()
	if err != nil {
		return "", err
	}

	val, ok := creds[name]
	if !ok {
		return "", ErrNotFound
	}
	return val, nil
}

// Set stores or updates a credential.
func (cs *CredentialStore) Set(name string, value string) error {
	creds, err := cs.readCredentials()
	if err != nil {
		return err
	}

	creds[name] = value

	return cs.writeCredentials(creds)
}

// Delete removes a credential. It returns ErrNotFound if the key does not
// exist.
func (cs *CredentialStore) Delete(name string) error {
	creds, err := cs.readCredentials()
	if err != nil {
		return err
	}

	if _, ok := creds[name]; !ok {
		return ErrNotFound
	}

	delete(creds, name)

	return cs.writeCredentials(creds)
}

// ResolveEnv takes an environment map (from MCP server config) and resolves
// any values prefixed with "encrypted:ref:" by looking up the referenced key
// in the credential store. Non-matching values are passed through unchanged.
func (cs *CredentialStore) ResolveEnv(env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return make(map[string]string), nil
	}

	// Only read credentials once, even if multiple refs need resolving.
	var creds map[string]string
	needCreds := false
	for _, v := range env {
		if strings.HasPrefix(v, encryptedRefPrefix) {
			needCreds = true
			break
		}
	}
	if needCreds {
		var err error
		creds, err = cs.readCredentials()
		if err != nil {
			return nil, fmt.Errorf("reading credentials for env resolution: %w", err)
		}
	}

	resolved := make(map[string]string, len(env))
	for k, v := range env {
		if strings.HasPrefix(v, encryptedRefPrefix) {
			refName := strings.TrimPrefix(v, encryptedRefPrefix)
			credVal, ok := creds[refName]
			if !ok {
				return nil, fmt.Errorf("credential %q referenced by env var %q: %w", refName, k, ErrNotFound)
			}
			resolved[k] = credVal
		} else {
			resolved[k] = v
		}
	}
	return resolved, nil
}

// readCredentials decrypts and deserializes the credential file. If the file
// does not exist, an empty map is returned (no credentials stored yet).
func (cs *CredentialStore) readCredentials() (map[string]string, error) {
	data, err := os.ReadFile(cs.path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading credential file: %w", err)
	}

	if len(data) < nonceSize {
		return nil, fmt.Errorf("credential file is corrupted: too short (%d bytes)", len(data))
	}

	plaintext, err := cs.decrypt(data)
	if err != nil {
		return nil, fmt.Errorf("decrypting credential file: %w", err)
	}

	creds := make(map[string]string)
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		// Best-effort zeroing of plaintext before returning error.
		zeroBytes(plaintext)
		return nil, fmt.Errorf("credential file is corrupted: %w", err)
	}

	// Best-effort zeroing of the raw plaintext buffer.
	zeroBytes(plaintext)

	return creds, nil
}

// writeCredentials serializes, encrypts, and writes the credential map to
// disk with 0600 permissions.
func (cs *CredentialStore) writeCredentials(creds map[string]string) error {
	plaintext, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshaling credentials: %w", err)
	}

	ciphertext, err := cs.encrypt(plaintext)
	// Best-effort zeroing of plaintext.
	zeroBytes(plaintext)
	if err != nil {
		return fmt.Errorf("encrypting credentials: %w", err)
	}

	if err := os.WriteFile(cs.path, ciphertext, 0600); err != nil {
		return fmt.Errorf("writing credential file: %w", err)
	}

	return nil
}

// encrypt produces nonce || AES-256-GCM(plaintext).
func (cs *CredentialStore) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(cs.key[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	// Seal appends ciphertext to nonce, giving us nonce || ciphertext.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt expects data formatted as nonce (12 bytes) || ciphertext.
func (cs *CredentialStore) decrypt(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(cs.key[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := data[:nonceSize]
	ciphertext := data[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong key or corrupted data): %w", err)
	}

	return plaintext, nil
}

// zeroBytes overwrites a byte slice with zeros. This is best-effort in Go
// because the garbage collector may have already copied the data elsewhere.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
