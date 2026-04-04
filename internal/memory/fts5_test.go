package memory

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openFTS5TestDB creates a temporary SQLite database with a content table
// and an FTS5 virtual table backed by external content, plus triggers to
// keep them in sync.
func openFTS5TestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fts5_test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE test_items (
			rowid INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT,
			body  TEXT
		)`,
		`CREATE VIRTUAL TABLE test_items_fts USING fts5(
			title, body,
			content=test_items,
			content_rowid=rowid
		)`,
		// INSERT trigger: sync new rows into FTS.
		`CREATE TRIGGER test_items_ai AFTER INSERT ON test_items BEGIN
			INSERT INTO test_items_fts(rowid, title, body)
			VALUES (new.rowid, new.title, new.body);
		END`,
		// DELETE trigger: remove old content from FTS before the row disappears.
		`CREATE TRIGGER test_items_ad AFTER DELETE ON test_items BEGIN
			INSERT INTO test_items_fts(test_items_fts, rowid, title, body)
			VALUES ('delete', old.rowid, old.title, old.body);
		END`,
		// UPDATE trigger: delete old content, insert new content.
		`CREATE TRIGGER test_items_au AFTER UPDATE ON test_items BEGIN
			INSERT INTO test_items_fts(test_items_fts, rowid, title, body)
			VALUES ('delete', old.rowid, old.title, old.body);
			INSERT INTO test_items_fts(rowid, title, body)
			VALUES (new.rowid, new.title, new.body);
		END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec schema %q: %v", stmt[:40], err)
		}
	}
	return db
}

func TestFTS5_TableCreation(t *testing.T) {
	db := openFTS5TestDB(t)

	// Verify both tables exist.
	for _, name := range []string{"test_items", "test_items_fts"} {
		var count int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name,
		).Scan(&count)
		if err != nil {
			t.Fatalf("check table %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("expected table %s to exist, got count %d", name, count)
		}
	}
}

func TestFTS5_InsertTriggerSync(t *testing.T) {
	db := openFTS5TestDB(t)

	_, err := db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Go concurrency", "Goroutines and channels make concurrent programming simple.",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Python basics", "Python is a dynamically typed language.",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// FTS MATCH should find the Go row.
	var title string
	err = db.QueryRow(
		`SELECT title FROM test_items_fts WHERE test_items_fts MATCH ?`, `"goroutines"`,
	).Scan(&title)
	if err != nil {
		t.Fatalf("FTS match goroutines: %v", err)
	}
	if title != "Go concurrency" {
		t.Errorf("expected title %q, got %q", "Go concurrency", title)
	}

	// MATCH on a body term.
	err = db.QueryRow(
		`SELECT title FROM test_items_fts WHERE test_items_fts MATCH ?`, `"dynamically"`,
	).Scan(&title)
	if err != nil {
		t.Fatalf("FTS match dynamically: %v", err)
	}
	if title != "Python basics" {
		t.Errorf("expected title %q, got %q", "Python basics", title)
	}

	// Non-matching term should return no rows.
	err = db.QueryRow(
		`SELECT title FROM test_items_fts WHERE test_items_fts MATCH ?`, `"nonexistent_xyzzy"`,
	).Scan(&title)
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows for non-matching term, got: %v", err)
	}
}

func TestFTS5_DeleteTriggerSync(t *testing.T) {
	db := openFTS5TestDB(t)

	_, err := db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Ephemeral note", "This will be deleted shortly.",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Confirm it is findable.
	var count int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, `"ephemeral"`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count before delete: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 FTS match before delete, got %d", count)
	}

	// Delete the content row -- trigger should remove it from FTS.
	_, err = db.Exec(`DELETE FROM test_items WHERE title = ?`, "Ephemeral note")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	err = db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, `"ephemeral"`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 FTS matches after delete, got %d", count)
	}
}

func TestFTS5_BM25Scoring(t *testing.T) {
	db := openFTS5TestDB(t)

	// Insert rows with varying relevance to the term "database".
	rows := []struct {
		title string
		body  string
	}{
		{"Cooking recipes", "How to make pasta from scratch."},
		{"Database internals", "A database stores data. Database indexing improves database query speed."},
		{"Intro to storage", "A database is one way to persist data."},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO test_items (title, body) VALUES (?, ?)`, r.title, r.body,
		); err != nil {
			t.Fatalf("insert %q: %v", r.title, err)
		}
	}

	// Query with BM25 ranking; lower rank value = more relevant.
	resultRows, err := db.Query(
		`SELECT title, rank FROM test_items_fts WHERE test_items_fts MATCH ? ORDER BY rank`,
		`"database"`,
	)
	if err != nil {
		t.Fatalf("BM25 query: %v", err)
	}
	defer resultRows.Close()

	var titles []string
	var prevRank float64
	first := true
	for resultRows.Next() {
		var title string
		var rank float64
		if err := resultRows.Scan(&title, &rank); err != nil {
			t.Fatalf("scan: %v", err)
		}
		titles = append(titles, title)
		if !first && rank < prevRank {
			t.Errorf("ranks not non-decreasing: %f came after %f", rank, prevRank)
		}
		prevRank = rank
		first = false
	}
	if err := resultRows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if len(titles) != 2 {
		t.Fatalf("expected 2 matching rows, got %d: %v", len(titles), titles)
	}

	// "Database internals" has 4 occurrences of "database", should rank first.
	if titles[0] != "Database internals" {
		t.Errorf("expected most relevant to be %q, got %q", "Database internals", titles[0])
	}
}

func TestFTS5_RebuildCommand(t *testing.T) {
	db := openFTS5TestDB(t)

	// Insert some data first so rebuild has something to process.
	if _, err := db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Rebuild test", "Verifying the rebuild command works.",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The rebuild command should succeed without error.
	if _, err := db.Exec(
		`INSERT INTO test_items_fts(test_items_fts) VALUES('rebuild')`,
	); err != nil {
		t.Fatalf("FTS5 rebuild failed: %v", err)
	}

	// Data should still be searchable after rebuild.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, `"rebuild"`,
	).Scan(&count); err != nil {
		t.Fatalf("match after rebuild: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 match after rebuild, got %d", count)
	}
}

func TestEscapeFTS5Query(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple term",
			input: "hello world",
			want:  `"hello world"`,
		},
		{
			name:  "injection attempt with OR",
			input: `test" OR "hack`,
			want:  `"test"" OR ""hack"`,
		},
		{
			name:  "asterisk wildcard",
			input: "*",
			want:  `"*"`,
		},
		{
			name:  "embedded double quotes",
			input: `""`,
			want:  `""""""`,
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "whitespace only",
			input: "   ",
			want:  "",
		},
		{
			name:  "unicode",
			input: "cafe\u0301 r\u00e9sum\u00e9",
			want:  "\"cafe\u0301 r\u00e9sum\u00e9\"",
		},
		{
			name:  "FTS5 NEAR operator",
			input: "NEAR(a, b)",
			want:  `"NEAR(a, b)"`,
		},
		{
			name:  "FTS5 AND/OR/NOT operators",
			input: "foo AND bar OR NOT baz",
			want:  `"foo AND bar OR NOT baz"`,
		},
		{
			name:  "leading/trailing whitespace",
			input: "  trimmed  ",
			want:  `"trimmed"`,
		},
		{
			name:  "single double quote",
			input: `"`,
			want:  `""""`,
		},
		{
			name:  "column filter attempt",
			input: "title:injection",
			want:  `"title:injection"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EscapeFTS5Query(tc.input)
			if got != tc.want {
				t.Errorf("EscapeFTS5Query(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFTS5_EscapedQueryIntegration(t *testing.T) {
	db := openFTS5TestDB(t)

	if _, err := db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Safe item", "This is perfectly normal content.",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// An adversarial input should be safely quoted and not cause SQL injection
	// or FTS5 syntax errors.
	adversarial := `normal" OR "hack`
	escaped := EscapeFTS5Query(adversarial)
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, escaped,
	).Scan(&count)
	if err != nil {
		t.Fatalf("adversarial query should not error: %v", err)
	}
	// The adversarial string should not match our content.
	if count != 0 {
		t.Errorf("adversarial query matched %d rows, expected 0", count)
	}

	// A legitimate escaped query should match.
	legitimate := EscapeFTS5Query("perfectly normal")
	err = db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, legitimate,
	).Scan(&count)
	if err != nil {
		t.Fatalf("legitimate query: %v", err)
	}
	if count != 1 {
		t.Errorf("legitimate query matched %d rows, expected 1", count)
	}
}

func TestFTS5_EmptyMatchQuery(t *testing.T) {
	db := openFTS5TestDB(t)

	if _, err := db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Some item", "Some body text.",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// EscapeFTS5Query returns "" for empty input; callers should skip MATCH.
	escaped := EscapeFTS5Query("")
	if escaped != "" {
		t.Fatalf("expected empty string for empty input, got %q", escaped)
	}

	// Passing empty string to MATCH is a syntax error in FTS5 -- verify the
	// caller's responsibility to guard against this.
	_, err := db.Query(
		`SELECT title FROM test_items_fts WHERE test_items_fts MATCH ?`, "",
	)
	if err == nil {
		t.Error("expected error when passing empty string to FTS5 MATCH")
	}
}

func TestFTS5_SpecialOperatorsInUserInput(t *testing.T) {
	db := openFTS5TestDB(t)

	// Insert content that contains words that look like FTS5 operators.
	if _, err := db.Exec(
		`INSERT INTO test_items (title, body) VALUES (?, ?)`,
		"Meeting notes", "We decided NOT to use OR logic AND keep it simple.",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Raw "NOT" as a MATCH query is an FTS5 operator; escaped, it is a phrase.
	escaped := EscapeFTS5Query("NOT")
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, escaped,
	).Scan(&count)
	if err != nil {
		t.Fatalf("match escaped NOT: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 match for escaped NOT, got %d", count)
	}

	// Asterisk is the FTS5 prefix token; escaped, it is literal.
	escaped = EscapeFTS5Query("*")
	err = db.QueryRow(
		`SELECT COUNT(*) FROM test_items_fts WHERE test_items_fts MATCH ?`, escaped,
	).Scan(&count)
	if err != nil {
		t.Fatalf("match escaped asterisk: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 matches for literal asterisk, got %d", count)
	}
}
