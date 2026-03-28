package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Fact represents a persistent user fact.
type Fact struct {
	ID               int64
	UserID           int64
	Fact             string
	Category         string
	Source           string
	LastReferencedAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// FactStore provides CRUD for user_facts.
type FactStore struct {
	db       *sql.DB
	maxFacts int
}

// NewFactStore creates a FactStore backed by the given DB.
func NewFactStore(db *sql.DB, maxFacts int) *FactStore {
	if maxFacts <= 0 {
		maxFacts = 50
	}
	return &FactStore{db: db, maxFacts: maxFacts}
}

// GetFacts returns all facts for a user, ordered by category then ID.
func (fs *FactStore) GetFacts(userID int64) ([]Fact, error) {
	rows, err := fs.db.Query(
		`SELECT id, user_id, fact, category, source, last_referenced_at, created_at, updated_at
		 FROM user_facts WHERE user_id = ? ORDER BY category, id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("facts: get: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.ID, &f.UserID, &f.Fact, &f.Category, &f.Source, &f.LastReferencedAt, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("facts: scan: %w", err)
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

// AddFact inserts a new fact. Returns the new fact ID.
// The fact text is sanitized (control chars stripped, max 200 chars).
func (fs *FactStore) AddFact(userID int64, fact, category, source string) (int64, error) {
	fact = sanitizeFact(fact)
	if fact == "" {
		return 0, fmt.Errorf("facts: fact text is empty after sanitization")
	}
	if category == "" {
		category = "general"
	}
	if source == "" {
		source = "explicit"
	}

	// Check max_facts limit.
	var count int
	if err := fs.db.QueryRow(`SELECT COUNT(*) FROM user_facts WHERE user_id = ?`, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("facts: count: %w", err)
	}
	if count >= fs.maxFacts {
		return 0, fmt.Errorf("facts: limit reached (%d/%d). Remove old facts first", count, fs.maxFacts)
	}

	now := time.Now().UTC()
	result, err := fs.db.Exec(
		`INSERT INTO user_facts (user_id, fact, category, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, fact, category, source, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("facts: insert: %w", err)
	}
	return result.LastInsertId()
}

// DeleteFact removes a fact by ID, scoped to the user (IDOR protection).
func (fs *FactStore) DeleteFact(factID int64, userID int64) error {
	result, err := fs.db.Exec(
		`DELETE FROM user_facts WHERE id = ? AND user_id = ?`,
		factID, userID,
	)
	if err != nil {
		return fmt.Errorf("facts: delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("facts: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("facts: fact %d not found", factID)
	}
	return nil
}

// UpdateLastReferenced updates last_referenced_at for the given fact IDs.
func (fs *FactStore) UpdateLastReferenced(factIDs []int64) error {
	if len(factIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(factIDs))
	args := make([]any, 0, len(factIDs)+1)
	args = append(args, time.Now().UTC())
	for i, id := range factIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	_, err := fs.db.Exec(
		fmt.Sprintf(`UPDATE user_facts SET last_referenced_at = ? WHERE id IN (%s)`, strings.Join(placeholders, ",")),
		args...,
	)
	return err
}

// sanitizeFact strips control characters and limits length to 200 chars.
func sanitizeFact(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) && r != ' ' {
			continue // strip control chars (but keep spaces)
		}
		b.WriteRune(r)
	}
	result := strings.TrimSpace(b.String())
	if len(result) > 200 {
		result = result[:200]
	}
	return result
}
