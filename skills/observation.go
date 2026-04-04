package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// AllowedObservationTypes is the whitelist of valid observation types.
var AllowedObservationTypes = []string{
	"decision",
	"preference",
	"project_state",
	"commitment",
	"discovery",
	"reference",
}

// uuidPattern matches a standard UUID v4 with hyphens (8-4-4-4-12 hex).
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Observation represents a single observation record.
type Observation struct {
	ID        string
	UserID    int64
	Type      string
	Content   string
	CreatedAt time.Time
}

// ObservationSearchResult represents a vector search result for observations.
type ObservationSearchResult struct {
	ID        string
	Title     string
	Type      string
	Score     float32
	CreatedAt string
}

// ObservationStore abstracts the observation operations needed by observation skills.
type ObservationStore interface {
	SearchObservations(ctx context.Context, query string, userID int64, obsType string, limit int) ([]ObservationSearchResult, error)
	DeleteObservation(id string, userID int64) error
	DeleteObservationVector(ctx context.Context, id string) error
}

// EntitySearchResult from FTS5 search on entity names.
type EntitySearchResult struct {
	ObservationID string
	Name          string
	EntityType    string
}

// EntityStore abstracts entity operations needed by observation skills.
type EntityStore interface {
	SearchEntitiesFTS(query string, entityType string, userID int64, limit int) ([]EntitySearchResult, error)
}

// InitObservationSkills creates the observations table (if not exists) and
// returns the search_observations, list_observations, get_observation, and
// forget_observation skills. The db should be the same *sql.DB used by the
// memory store.
func InitObservationSkills(db *sql.DB, store ObservationStore, entityStore EntityStore) ([]*Skill, error) {
	// Schema is created by internal/memory/store.go migrate(). No need to create here.
	return []*Skill{
		{
			Name:        "search_observations",
			Description: "Search past observations by semantic query. Use when looking for patterns, preferences, or behaviors observed about the user.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Natural language query to search observations"},"type":{"type":"string","enum":["decision","preference","project_state","commitment","discovery","reference"],"description":"Filter by observation type"},"limit":{"type":"integer","description":"Maximum number of results (1-50, default 10)"}},"required":["query"]}`),
			Execute:     makeSearchObservationsExecute(store),
		},
		{
			Name:        "list_observations",
			Description: "List recent observations, optionally filtered by type.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","enum":["decision","preference","project_state","commitment","discovery","reference"],"description":"Filter by observation type"},"limit":{"type":"integer","description":"Maximum number of results (1-50, default 10)"}}}`),
			Execute:     makeListObservationsExecute(db),
		},
		{
			Name:        "get_observation",
			Description: "Get full details of a specific observation by its ID.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"UUID of the observation"}},"required":["id"]}`),
			Execute:     makeGetObservationExecute(db),
		},
		{
			Name:        "forget_observation",
			Description: "Delete an observation by its ID. Also removes it from vector search.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"UUID of the observation to delete"}},"required":["id"]}`),
			Execute:     makeForgetObservationExecute(store),
		},
		{
			Name:        "search_entities",
			Description: "Search for entities (people, projects, files, tools) mentioned in observations. Use for 'what do I know about X?' queries.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Entity name to search for"},"type":{"type":"string","enum":["person","project","file","tool"],"description":"Filter by entity type"},"limit":{"type":"integer","description":"Maximum number of results (1-50, default 10)"}},"required":["query"]}`),
			Execute:     makeSearchEntitiesExecute(entityStore),
		},
	}, nil
}

// isAllowedEntityType checks whether the given entity type is valid.
func isAllowedEntityType(t string) bool {
	for _, allowed := range AllowedEntityTypes {
		if t == allowed {
			return true
		}
	}
	return false
}

// AllowedEntityTypes is the whitelist of valid entity types for skills.
var AllowedEntityTypes = []string{"person", "project", "file", "tool"}

// isAllowedObservationType checks whether the given type is in the whitelist.
func isAllowedObservationType(t string) bool {
	for _, allowed := range AllowedObservationTypes {
		if t == allowed {
			return true
		}
	}
	return false
}

// isValidUUID checks whether s is a valid UUID with hyphens.
func isValidUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

type searchObservationsInput struct {
	Query string `json:"query"`
	Type  string `json:"type"`
	Limit int    `json:"limit"`
}

func makeSearchObservationsExecute(store ObservationStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params searchObservationsInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		if params.Type != "" && !isAllowedObservationType(params.Type) {
			return "", fmt.Errorf("invalid observation type %q; allowed: %s", params.Type, strings.Join(AllowedObservationTypes, ", "))
		}
		if params.Limit <= 0 {
			params.Limit = 10
		}
		if params.Limit > 50 {
			params.Limit = 50
		}

		user := GetUser(ctx)
		results, err := store.SearchObservations(ctx, params.Query, user.UserID, params.Type, params.Limit)
		if err != nil {
			return "", fmt.Errorf("search observations: %w", err)
		}

		if len(results) == 0 {
			return fmt.Sprintf("No observations found for: %s", params.Query), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d observations for: %s\n\n", len(results), params.Query)
		for i, r := range results {
			fmt.Fprintf(&sb, "%d. [%s] (score: %.2f)\n", i+1, r.Type, r.Score)
			if r.CreatedAt != "" {
				fmt.Fprintf(&sb, "   Time: %s\n", r.CreatedAt)
			}
			fmt.Fprintf(&sb, "   %s\n\n", r.Title)
		}
		return strings.TrimSpace(sb.String()), nil
	}
}

type listObservationsInput struct {
	Type  string `json:"type"`
	Limit int    `json:"limit"`
}

func makeListObservationsExecute(db *sql.DB) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params listObservationsInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Type != "" && !isAllowedObservationType(params.Type) {
			return "", fmt.Errorf("invalid observation type %q; allowed: %s", params.Type, strings.Join(AllowedObservationTypes, ", "))
		}
		if params.Limit <= 0 {
			params.Limit = 10
		}
		if params.Limit > 50 {
			params.Limit = 50
		}

		user := GetUser(ctx)

		var rows *sql.Rows
		var err error
		if params.Type != "" {
			rows, err = db.QueryContext(ctx,
				`SELECT id, type, title, created_at FROM observations WHERE user_id = ? AND type = ? ORDER BY created_at DESC LIMIT ?`,
				user.UserID, params.Type, params.Limit,
			)
		} else {
			rows, err = db.QueryContext(ctx,
				`SELECT id, type, title, created_at FROM observations WHERE user_id = ? ORDER BY created_at DESC LIMIT ?`,
				user.UserID, params.Limit,
			)
		}
		if err != nil {
			return "", fmt.Errorf("list observations: %w", err)
		}
		defer rows.Close()

		var sb strings.Builder
		count := 0
		for rows.Next() {
			var id, obsType, title string
			var createdAt time.Time
			if err := rows.Scan(&id, &obsType, &title, &createdAt); err != nil {
				return "", fmt.Errorf("scan observation: %w", err)
			}
			count++
			// Truncate title preview to 120 runes.
			preview := title
			runes := []rune(preview)
			if len(runes) > 120 {
				preview = string(runes[:120]) + "..."
			}
			fmt.Fprintf(&sb, "[id=%s] %s [%s]: %s\n", id, createdAt.Format("2006-01-02"), obsType, preview)
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("iterate observations: %w", err)
		}

		if count == 0 {
			return "No observations found.", nil
		}

		header := fmt.Sprintf("Found %d observations:\n\n", count)
		return header + strings.TrimSpace(sb.String()), nil
	}
}

type getObservationInput struct {
	ID string `json:"id"`
}

func makeGetObservationExecute(db *sql.DB) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params getObservationInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		if !isValidUUID(params.ID) {
			return "", fmt.Errorf("invalid observation ID format")
		}

		user := GetUser(ctx)

		var id, obsType, title, summary string
		var createdAt time.Time
		err := db.QueryRowContext(ctx,
			`SELECT id, type, title, summary, created_at FROM observations WHERE id = ? AND user_id = ?`,
			params.ID, user.UserID,
		).Scan(&id, &obsType, &title, &summary, &createdAt)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("observation %s not found", params.ID)
		}
		if err != nil {
			return "", fmt.Errorf("get observation: %w", err)
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Observation %s\n", id)
		fmt.Fprintf(&sb, "Type: %s\n", obsType)
		fmt.Fprintf(&sb, "Created: %s\n\n", createdAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&sb, "%s\n%s", title, summary)
		return sb.String(), nil
	}
}

type forgetObservationInput struct {
	ID string `json:"id"`
}

func makeForgetObservationExecute(store ObservationStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params forgetObservationInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		if !isValidUUID(params.ID) {
			return "", fmt.Errorf("invalid observation ID format")
		}

		user := GetUser(ctx)

		if err := store.DeleteObservation(params.ID, user.UserID); err != nil {
			return "", fmt.Errorf("delete observation: %w", err)
		}
		// Best-effort vector cleanup; log but don't fail the skill.
		if err := store.DeleteObservationVector(ctx, params.ID); err != nil {
			return fmt.Sprintf("Deleted observation %s (vector cleanup failed: %v).", params.ID, err), nil
		}
		return fmt.Sprintf("Deleted observation %s.", params.ID), nil
	}
}

type searchEntitiesInput struct {
	Query string `json:"query"`
	Type  string `json:"type"`
	Limit int    `json:"limit"`
}

func makeSearchEntitiesExecute(store EntityStore) func(ctx context.Context, input json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var params searchEntitiesInput
		if err := json.Unmarshal(input, &params); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if params.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		limit := params.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		if params.Type != "" && !isAllowedEntityType(params.Type) {
			return "", fmt.Errorf("invalid entity type %q; allowed: person, project, file, tool", params.Type)
		}

		user := GetUser(ctx)
		if store == nil {
			return "Entity search is not available.", nil
		}

		results, err := store.SearchEntitiesFTS(params.Query, params.Type, user.UserID, limit)
		if err != nil {
			return "", fmt.Errorf("search entities: %w", err)
		}
		if len(results) == 0 {
			return "No entities found matching your query.", nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d entities:\n\n", len(results))
		for _, r := range results {
			obsShort := r.ObservationID
			if len(obsShort) > 8 {
				obsShort = obsShort[:8]
			}
			fmt.Fprintf(&sb, "- %s (%s) — observation %s\n", r.Name, r.EntityType, obsShort)
		}
		return sb.String(), nil
	}
}
