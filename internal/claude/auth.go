package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// LoadAPIKey returns configKey if it is non-empty, providing a simple
// passthrough for config-based API key resolution.
func LoadAPIKey(configKey string) string {
	return configKey
}

// oauthCredentials is the expected shape of ~/.claude/.credentials.json.
type oauthCredentials struct {
	Token string `json:"claudeAiOauth"`
}

// LoadOAuthToken reads an OAuth token from the given credentials file path
// (typically ~/.claude/.credentials.json). It retries once on read error
// as a race-condition guard — the file can be transiently unavailable when
// the Claude desktop app refreshes it.
func LoadOAuthToken(credPath string) (string, error) {
	data, err := os.ReadFile(credPath)
	if err != nil {
		// Retry once after a short delay (race protection from nanoclaw
		// experience: the desktop app can briefly lock/rewrite the file).
		time.Sleep(100 * time.Millisecond)
		data, err = os.ReadFile(credPath)
		if err != nil {
			return "", fmt.Errorf("claude: read credentials %q: %w", credPath, err)
		}
	}

	var creds oauthCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("claude: parse credentials %q: %w", credPath, err)
	}

	if creds.Token == "" {
		return "", fmt.Errorf("claude: no OAuth token found in %q", credPath)
	}

	return creds.Token, nil
}
