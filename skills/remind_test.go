package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/jialuohu/curlycatclaw/internal/telegram"

	_ "modernc.org/sqlite"
)

// newTestDBSingleConn opens an in-memory SQLite database with a single connection,
// which is necessary for tests that use the DB from multiple goroutines (e.g., actor tests)
// to ensure they all see the same in-memory database.
func newTestDBSingleConn(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// initRemindSkillsForTest creates remind skills and returns them keyed by name.
func initRemindSkillsForTest(t *testing.T, db *sql.DB, signalCh chan<- int64) map[string]*Skill {
	t.Helper()
	skills, err := InitRemindSkills(db, signalCh, time.UTC)
	if err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}
	m := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}
	return m
}

func TestInitRemindSkills_CreatesTable(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)

	skills, err := InitRemindSkills(db, signalCh, time.UTC)
	if err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	if len(skills) != 4 {
		t.Fatalf("expected 4 skills (set/list/cancel/delete), got %d", len(skills))
	}

	// Verify the reminders table exists.
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='reminders'`).Scan(&name)
	if err != nil {
		t.Fatalf("reminders table not found: %v", err)
	}
	if name != "reminders" {
		t.Errorf("table name = %q, want %q", name, "reminders")
	}

	// Verify idempotent.
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("second InitRemindSkills should be idempotent: %v", err)
	}
}

func TestSetReminder_ValidFutureTime(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{
		Message: "Buy groceries",
		FireAt:  futureTime,
	})

	result, err := skills["set_reminder"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("set_reminder: %v", err)
	}
	if !strings.Contains(result, "Reminder #1 set for") {
		t.Errorf("result = %q, want it to contain reminder confirmation", result)
	}
	if !strings.Contains(result, "Buy groceries") {
		t.Errorf("result = %q, want it to contain the message", result)
	}

	// Verify the reminder is in the database.
	var message, status string
	var userID int64
	err = db.QueryRow(`SELECT user_id, message, status FROM reminders WHERE id = 1`).Scan(&userID, &message, &status)
	if err != nil {
		t.Fatalf("query saved reminder: %v", err)
	}
	if userID != 1 {
		t.Errorf("user_id = %d, want 1", userID)
	}
	if message != "Buy groceries" {
		t.Errorf("message = %q, want %q", message, "Buy groceries")
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}

	// Verify signal was sent.
	select {
	case id := <-signalCh:
		if id != 1 {
			t.Errorf("signal id = %d, want 1", id)
		}
	default:
		t.Error("expected signal on channel, got none")
	}
}

func TestSetReminder_MissingMessage(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(setReminderInput{
		Message: "",
		FireAt:  "2099-01-15T09:00:00",
	})

	_, err := skills["set_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty message, got nil")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "message is required")
	}
}

func TestSetReminder_MissingFireAt(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(setReminderInput{
		Message: "test",
		FireAt:  "",
	})

	_, err := skills["set_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for empty fire_at, got nil")
	}
	if !strings.Contains(err.Error(), "fire_at is required") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "fire_at is required")
	}
}

func TestListReminders_ReturnsUserReminders(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Set two reminders.
	for _, msg := range []string{"Meeting at 3pm", "Call dentist"} {
		futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
		input, _ := json.Marshal(setReminderInput{Message: msg, FireAt: futureTime})
		if _, err := skills["set_reminder"].Execute(ctx, input); err != nil {
			t.Fatalf("set_reminder(%s): %v", msg, err)
		}
		// Drain signal.
		<-signalCh
	}

	// List all reminders.
	listInput, _ := json.Marshal(listRemindersInput{})
	result, err := skills["list_reminders"].Execute(ctx, listInput)
	if err != nil {
		t.Fatalf("list_reminders: %v", err)
	}
	if !strings.Contains(result, "Meeting at 3pm") {
		t.Errorf("result should contain 'Meeting at 3pm', got %q", result)
	}
	if !strings.Contains(result, "Call dentist") {
		t.Errorf("result should contain 'Call dentist', got %q", result)
	}
}

func TestListReminders_FilterByStatus(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Set a reminder and then cancel it.
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{Message: "Will cancel", FireAt: futureTime})
	if _, err := skills["set_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("set_reminder: %v", err)
	}
	<-signalCh

	cancelInput, _ := json.Marshal(cancelReminderInput{ID: 1})
	if _, err := skills["cancel_reminder"].Execute(ctx, cancelInput); err != nil {
		t.Fatalf("cancel_reminder: %v", err)
	}
	<-signalCh

	// List only pending — should be empty.
	listInput, _ := json.Marshal(listRemindersInput{Status: "pending"})
	result, err := skills["list_reminders"].Execute(ctx, listInput)
	if err != nil {
		t.Fatalf("list_reminders: %v", err)
	}
	if !strings.Contains(result, "No pending reminders found") {
		t.Errorf("result should contain \"No pending reminders found\", got %q", result)
	}
}

// TestListReminders_DefaultsToPending is the regression guard for the
// behavior change introduced when the default `list_reminders` filter
// flipped from "all statuses" to "pending only". Without this default,
// cancelled/fired tombstones accumulate in the output forever and
// make the agent's answer to "what's scheduled?" harder to read.
func TestListReminders_DefaultsToPending(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Seed: one pending + one cancelled + one fired.
	now := time.Now().UTC()
	future := now.Add(24 * time.Hour)
	if _, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?), (?, ?, ?, ?, 'cancelled', ?), (?, ?, ?, ?, 'fired', ?)`,
		1, 10, "active-one", future, now,
		1, 10, "cancelled-one", future, now,
		1, 10, "fired-one", future, now,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Default: no status → only pending.
	listInput, _ := json.Marshal(listRemindersInput{})
	result, err := skills["list_reminders"].Execute(ctx, listInput)
	if err != nil {
		t.Fatalf("list_reminders: %v", err)
	}
	if !strings.Contains(result, "active-one") {
		t.Errorf("default list should include pending reminder, got %q", result)
	}
	if strings.Contains(result, "cancelled-one") {
		t.Errorf("default list should NOT include cancelled tombstone, got %q", result)
	}
	if strings.Contains(result, "fired-one") {
		t.Errorf("default list should NOT include fired tombstone, got %q", result)
	}
}

// TestListReminders_StatusAll asserts the explicit "all" opt-in returns
// every status — the escape hatch for users/agents that DO want to see
// history.
func TestListReminders_StatusAll(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	now := time.Now().UTC()
	future := now.Add(24 * time.Hour)
	if _, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?), (?, ?, ?, ?, 'cancelled', ?), (?, ?, ?, ?, 'fired', ?)`,
		1, 10, "active-one", future, now,
		1, 10, "cancelled-one", future, now,
		1, 10, "fired-one", future, now,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	listInput, _ := json.Marshal(listRemindersInput{Status: "all"})
	result, err := skills["list_reminders"].Execute(ctx, listInput)
	if err != nil {
		t.Fatalf("list_reminders: %v", err)
	}
	for _, want := range []string{"active-one", "cancelled-one", "fired-one"} {
		if !strings.Contains(result, want) {
			t.Errorf("status=all list should include %q, got %q", want, result)
		}
	}
}

func TestDeleteReminder_RemovesCancelled(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'cancelled', ?)`,
		1, 10, "dead", now.Add(1*time.Hour), now,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	input, _ := json.Marshal(deleteReminderInput{ID: 1})
	result, err := skills["delete_reminder"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("delete_reminder: %v", err)
	}
	if !strings.Contains(result, "Deleted reminder #1") {
		t.Errorf("expected deletion confirmation, got %q", result)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reminders WHERE id = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("row should be gone from DB, got count=%d", count)
	}
}

func TestDeleteReminder_RemovesFired(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'fired', ?)`,
		1, 10, "done", now.Add(-1*time.Hour), now,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	input, _ := json.Marshal(deleteReminderInput{ID: 1})
	if _, err := skills["delete_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("delete_reminder on fired row: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reminders WHERE id = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("row should be gone from DB, got count=%d", count)
	}
}

func TestDeleteReminder_RefusesPending(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "still-alive", now.Add(1*time.Hour), now,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	input, _ := json.Marshal(deleteReminderInput{ID: 1})
	_, err := skills["delete_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("delete_reminder on pending row should error")
	}
	// The agent's autorepair loop keys off the phrase "cancel_reminder" in the
	// error — if the wording changes, the agent will loop instead of fixing.
	if !strings.Contains(err.Error(), "cancel_reminder") {
		t.Errorf("error should mention cancel_reminder as the next step, got: %v", err)
	}

	// Row must remain.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reminders WHERE id = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("pending row should still exist, got count=%d", count)
	}
}

func TestDeleteReminder_NotFound(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	input, _ := json.Marshal(deleteReminderInput{ID: 999})
	_, err := skills["delete_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("delete_reminder on missing id should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say \"not found\", got: %v", err)
	}
}

// TestDeleteReminder_UserScoped is the IDOR guard: user A cannot delete
// user B's reminder, even if A guesses the id.
func TestDeleteReminder_UserScoped(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	now := time.Now().UTC()
	// user B (id=2) owns a cancelled reminder.
	if _, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'cancelled', ?)`,
		2, 20, "user-B-tombstone", now, now,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// user A (id=1) tries to delete it.
	ctxA := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(deleteReminderInput{ID: 1})
	_, err := skills["delete_reminder"].Execute(ctxA, input)
	if err == nil {
		t.Fatal("user A should not be able to delete user B's reminder")
	}
	// From A's perspective it's indistinguishable from "not found" — deliberate
	// to avoid leaking existence of other users' rows.
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("cross-user delete should return not-found (no existence leak), got: %v", err)
	}

	// Row must still exist.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reminders WHERE id = 1 AND user_id = 2`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("user B's row must not be deleted by user A, got count=%d", count)
	}
}

func TestCancelReminder_UpdatesStatus(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Set a reminder.
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{Message: "To cancel", FireAt: futureTime})
	if _, err := skills["set_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("set_reminder: %v", err)
	}
	<-signalCh

	// Cancel it.
	cancelInput, _ := json.Marshal(cancelReminderInput{ID: 1})
	result, err := skills["cancel_reminder"].Execute(ctx, cancelInput)
	if err != nil {
		t.Fatalf("cancel_reminder: %v", err)
	}
	if result != "Reminder #1 cancelled" {
		t.Errorf("result = %q, want %q", result, "Reminder #1 cancelled")
	}

	// Verify status in DB.
	var status string
	err = db.QueryRow(`SELECT status FROM reminders WHERE id = 1`).Scan(&status)
	if err != nil {
		t.Fatalf("query reminder: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("status = %q, want %q", status, "cancelled")
	}

	// Verify signal was sent.
	select {
	case id := <-signalCh:
		if id != 1 {
			t.Errorf("signal id = %d, want 1", id)
		}
	default:
		t.Error("expected signal on channel, got none")
	}
}

func TestCancelReminder_NonExistent(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	cancelInput, _ := json.Marshal(cancelReminderInput{ID: 999})
	_, err := skills["cancel_reminder"].Execute(ctx, cancelInput)
	if err == nil {
		t.Fatal("expected error for non-existent reminder, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "not found")
	}
}

func TestReminders_UserScoped(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	// User A sets a reminder.
	ctxA := WithUser(context.Background(), UserInfo{UserID: 100, ChatID: 1})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	inputA, _ := json.Marshal(setReminderInput{Message: "User A reminder", FireAt: futureTime})
	if _, err := skills["set_reminder"].Execute(ctxA, inputA); err != nil {
		t.Fatalf("set_reminder(userA): %v", err)
	}
	<-signalCh

	// User B sets a reminder.
	ctxB := WithUser(context.Background(), UserInfo{UserID: 200, ChatID: 2})
	inputB, _ := json.Marshal(setReminderInput{Message: "User B reminder", FireAt: futureTime})
	if _, err := skills["set_reminder"].Execute(ctxB, inputB); err != nil {
		t.Fatalf("set_reminder(userB): %v", err)
	}
	<-signalCh

	// User A lists — should only see their own.
	listInput, _ := json.Marshal(listRemindersInput{})
	resultA, err := skills["list_reminders"].Execute(ctxA, listInput)
	if err != nil {
		t.Fatalf("list_reminders(userA): %v", err)
	}
	if !strings.Contains(resultA, "User A reminder") {
		t.Errorf("userA list should contain their reminder, got %q", resultA)
	}
	if strings.Contains(resultA, "User B reminder") {
		t.Errorf("userA list should NOT contain userB's reminder, got %q", resultA)
	}

	// User B lists — should only see their own.
	resultB, err := skills["list_reminders"].Execute(ctxB, listInput)
	if err != nil {
		t.Fatalf("list_reminders(userB): %v", err)
	}
	if !strings.Contains(resultB, "User B reminder") {
		t.Errorf("userB list should contain their reminder, got %q", resultB)
	}
	if strings.Contains(resultB, "User A reminder") {
		t.Errorf("userB list should NOT contain userA's reminder, got %q", resultB)
	}

	// User B cannot cancel user A's reminder.
	cancelInput, _ := json.Marshal(cancelReminderInput{ID: 1}) // ID 1 belongs to user A
	_, err = skills["cancel_reminder"].Execute(ctxB, cancelInput)
	if err == nil {
		t.Fatal("expected error when userB cancels userA's reminder, got nil")
	}
}

func TestReminderActor_FiresPastDueOnStartup(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)

	// Create the reminders table.
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Insert a past-due reminder directly into the DB.
	pastTime := time.Now().Add(-1 * time.Hour).UTC()
	_, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "Past due reminder", pastTime, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert past-due reminder: %v", err)
	}

	// Create a buffered channel to capture outgoing messages.
	tgInbox := make(chan telegram.OutgoingMessage, 16)

	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	// Run the actor in a goroutine with a short-lived context.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ra.Run(ctx)
	}()

	// Wait for the reminder to be fired.
	select {
	case msg := <-tgInbox:
		if msg.ChatID != 10 {
			t.Errorf("chat_id = %d, want 10", msg.ChatID)
		}
		if msg.Text != "Reminder: Past due reminder" {
			t.Errorf("text = %q, want %q", msg.Text, "Reminder: Past due reminder")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for past-due reminder to fire")
	}

	// Allow time for the DB status update (runs after successful send).
	time.Sleep(100 * time.Millisecond)

	// Verify status updated to "fired".
	var status string
	err = db.QueryRow(`SELECT status FROM reminders WHERE id = 1`).Scan(&status)
	if err != nil {
		t.Fatalf("query reminder status: %v", err)
	}
	if status != "fired" {
		t.Errorf("status = %q, want %q", status, "fired")
	}

	cancel()
	<-done
}

func TestReminderActor_CancellationPreventsFireing(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)

	// Create the reminders table.
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Insert a future reminder.
	futureTime := time.Now().Add(5 * time.Second).UTC()
	_, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "Should not fire", futureTime, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert future reminder: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)

	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ra.Run(ctx)
	}()

	// Give the actor time to start and schedule the reminder.
	time.Sleep(500 * time.Millisecond)

	// Cancel the reminder in the DB and signal the actor.
	_, err = db.Exec(`UPDATE reminders SET status = 'cancelled' WHERE id = 1`)
	if err != nil {
		t.Fatalf("cancel reminder in db: %v", err)
	}
	actorSignalCh <- 1

	// Wait and verify no message was sent.
	select {
	case msg := <-tgInbox:
		t.Errorf("expected no message, but got: %+v", msg)
	case <-time.After(3 * time.Second):
		// Good: no message received.
	}

	cancel()
	<-done
}

func TestSetReminder_InvalidCronExpression(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{
		Message:   "Bad cron",
		FireAt:    futureTime,
		Recurring: "not a cron expression",
	})

	_, err := skills["set_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid cron expression, got nil")
	}
	if !strings.Contains(err.Error(), "invalid cron expression") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "invalid cron expression")
	}

	// Verify nothing was inserted into the database.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reminders`).Scan(&count); err != nil {
		t.Fatalf("count reminders: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 reminders in DB after invalid cron, got %d", count)
	}
}

func TestSetReminder_ValidCronExpression(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{
		Message:   "Valid cron",
		FireAt:    futureTime,
		Recurring: "0 9 * * MON-FRI",
	})

	result, err := skills["set_reminder"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("set_reminder with valid cron: %v", err)
	}
	if !strings.Contains(result, "recurring: 0 9 * * MON-FRI") {
		t.Errorf("result = %q, want it to contain the cron expression", result)
	}

	// Drain signal.
	<-signalCh
}

func TestReminderActor_CancelStopsScheduledJob(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)

	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Insert a one-time reminder that fires 2 seconds from now.
	futureTime := time.Now().Add(2 * time.Second).UTC()
	_, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "Should not fire", futureTime, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert reminder: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)

	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ra.Run(ctx)
	}()

	// Give the actor time to schedule the job.
	time.Sleep(200 * time.Millisecond)

	// Verify the job was tracked in the actor's map.
	ra.mu.Lock()
	_, tracked := ra.jobs[1]
	ra.mu.Unlock()
	if !tracked {
		t.Fatal("expected job to be tracked in actor's jobs map")
	}

	// Cancel the reminder in the DB and signal the actor.
	_, err = db.Exec(`UPDATE reminders SET status = 'cancelled' WHERE id = 1`)
	if err != nil {
		t.Fatalf("cancel reminder in db: %v", err)
	}
	actorSignalCh <- 1

	// Give the actor time to process the cancellation.
	time.Sleep(200 * time.Millisecond)

	// Verify the job was removed from the actor's map.
	ra.mu.Lock()
	_, stillTracked := ra.jobs[1]
	ra.mu.Unlock()
	if stillTracked {
		t.Error("expected job to be removed from actor's jobs map after cancellation")
	}

	// Wait past the original fire time and verify no message was sent.
	// The job was scheduled at +2s, we've used ~400ms, wait 3s more to be safe.
	select {
	case msg := <-tgInbox:
		t.Errorf("expected no message after cancellation, but got: %+v", msg)
	case <-time.After(3 * time.Second):
		// Good: the cancelled job did not fire.
	}

	cancel()
	<-done
}

// --- Cron task tests ---

func TestSetReminder_WithPrompt(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{
		Message: "Morning briefing",
		FireAt:  futureTime,
		Prompt:  "Check my notes and summarize what I need to do today",
	})

	result, err := skills["set_reminder"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("set_reminder: %v", err)
	}
	if !strings.Contains(result, "Morning briefing") {
		t.Errorf("result = %q, want it to contain the message", result)
	}
	if !strings.Contains(result, "[cron:") {
		t.Errorf("result = %q, want it to contain [cron: tag", result)
	}

	// Verify prompt stored in DB.
	var prompt sql.NullString
	err = db.QueryRow(`SELECT prompt FROM reminders WHERE id = 1`).Scan(&prompt)
	if err != nil {
		t.Fatalf("query prompt: %v", err)
	}
	if !prompt.Valid || prompt.String != "Check my notes and summarize what I need to do today" {
		t.Errorf("prompt = %v, want the stored prompt text", prompt)
	}

	<-signalCh // drain
}

func TestSetReminder_WithoutPrompt_NoPromptInDB(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(setReminderInput{
		Message: "Plain reminder",
		FireAt:  futureTime,
	})

	_, err := skills["set_reminder"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("set_reminder: %v", err)
	}

	var prompt sql.NullString
	err = db.QueryRow(`SELECT prompt FROM reminders WHERE id = 1`).Scan(&prompt)
	if err != nil {
		t.Fatalf("query prompt: %v", err)
	}
	if prompt.Valid {
		t.Errorf("prompt should be NULL for non-cron reminder, got %q", prompt.String)
	}

	<-signalCh // drain
}

func TestListReminders_CronTag(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	futureTime := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")

	// Create one regular and one cron reminder.
	input1, _ := json.Marshal(setReminderInput{Message: "Plain", FireAt: futureTime})
	input2, _ := json.Marshal(setReminderInput{Message: "Cron", FireAt: futureTime, Prompt: "do stuff"})

	skills["set_reminder"].Execute(ctx, input1)
	<-signalCh
	skills["set_reminder"].Execute(ctx, input2)
	<-signalCh

	result, err := skills["list_reminders"].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_reminders: %v", err)
	}
	if !strings.Contains(result, "[pending]") {
		t.Errorf("result should contain [pending] for plain reminder: %s", result)
	}
	if !strings.Contains(result, "[cron:pending]") {
		t.Errorf("result should contain [cron:pending] for cron reminder: %s", result)
	}
}

// mockCronRunner is a test double for CronRunner.
type mockCronRunner struct {
	result      string
	err         error
	called      bool
	gotPrompt   string
	gotSchedule time.Time
}

func (m *mockCronRunner) Execute(_ context.Context, _, _ int64, prompt, _ string, scheduledAt time.Time) (string, error) {
	m.called = true
	m.gotPrompt = prompt
	m.gotSchedule = scheduledAt
	return m.result, m.err
}

func TestFireCronTask_Success(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	_, err := InitRemindSkills(db, signalCh, time.UTC)
	if err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	mock := &mockCronRunner{result: "Here's your summary: all good"}
	ra := NewReminderActor(db, tgInbox, time.UTC, signalCh, mock)

	// Insert a cron reminder directly.
	prompt := "summarize my notes"
	fireAt := time.Now().Add(-1 * time.Second).UTC() // past due
	db.Exec(`INSERT INTO reminders (user_id, chat_id, message, fire_at, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		1, 10, "Morning briefing", fireAt, prompt, time.Now().UTC())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	// Wait for the cron task to fire.
	select {
	case msg := <-tgInbox:
		if !strings.Contains(msg.Text, "Morning briefing") {
			t.Errorf("expected header with reminder label, got: %s", msg.Text)
		}
		if !strings.Contains(msg.Text, "Here's your summary") {
			t.Errorf("expected cron result in message, got: %s", msg.Text)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for cron task to fire")
	}

	if !mock.called {
		t.Error("CronRunner.Execute was not called")
	}
	// Regression: the scheduled fire time must be plumbed through so the cron
	// system prompt can reference the intended time, not the lagged wall time.
	if mock.gotSchedule.IsZero() {
		t.Error("CronRunner.Execute called with zero scheduledAt — fire_at must be propagated")
	}
	if diff := mock.gotSchedule.Sub(fireAt); diff < -time.Second || diff > time.Second {
		t.Errorf("scheduledAt = %v, want close to fire_at %v (diff %v)", mock.gotSchedule, fireAt, diff)
	}

	cancel()
	<-done
}

func TestFireCronTask_Error(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	_, err := InitRemindSkills(db, signalCh, time.UTC)
	if err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	mock := &mockCronRunner{err: fmt.Errorf("rate limit exceeded")}
	ra := NewReminderActor(db, tgInbox, time.UTC, signalCh, mock)

	prompt := "do stuff"
	fireAt := time.Now().Add(-1 * time.Second).UTC()
	db.Exec(`INSERT INTO reminders (user_id, chat_id, message, fire_at, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		1, 10, "Failed task", fireAt, prompt, time.Now().UTC())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	select {
	case msg := <-tgInbox:
		if !strings.Contains(msg.Text, "[Cron task failed]") {
			t.Errorf("expected error notification, got: %s", msg.Text)
		}
		if !strings.Contains(msg.Text, "rate limit") {
			t.Errorf("expected error message in notification, got: %s", msg.Text)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for error notification")
	}

	cancel()
	<-done
}

// mockPersister records GetActiveConversation and AppendMessage calls so tests
// can assert the cron-turn persistence path without a real memory.Store.
type mockPersister struct {
	mu       sync.Mutex
	convID   string    // returned from GetActiveConversation
	convErr  error     // returned from GetActiveConversation
	appendOK []persistCall
	appendNErr int      // fail the Nth AppendMessage (0 = never); counts from 1
	appendErr  error
}

type persistCall struct {
	convID  string
	role    string
	content string
}

func (m *mockPersister) GetActiveConversation(_, _ int64, _ string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.convID, "", m.convErr
}

func (m *mockPersister) AppendMessage(convID, role string, content json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendOK = append(m.appendOK, persistCall{convID: convID, role: role, content: string(content)})
	if m.appendNErr > 0 && len(m.appendOK) == m.appendNErr {
		return m.appendErr
	}
	return nil
}

func (m *mockPersister) calls() []persistCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]persistCall, len(m.appendOK))
	copy(out, m.appendOK)
	return out
}

// TestFireCronTask_PersistsToConversation is the regression guard for the
// Apr 15 incident where the cron-fired daily digest bypassed the messages
// table, so the agent could not see its own cron output on the next user
// turn and fabricated timestamps to cover the gap.
func TestFireCronTask_PersistsToConversation(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	mock := &mockCronRunner{result: "Digest body: ModServe, HydraInfer, ..."}
	persister := &mockPersister{convID: "conv-abc"}
	ra := NewReminderActor(db, tgInbox, time.UTC, signalCh, mock)
	ra.SetConversationPersister(persister)

	prompt := "search papers on multimodal model serving"
	fireAt := time.Now().Add(-1 * time.Second).UTC()
	if _, err := db.Exec(`INSERT INTO reminders (user_id, chat_id, message, fire_at, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		7, 99, "📄 Daily paper digest", fireAt, prompt, time.Now().UTC()); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	select {
	case <-tgInbox: // just drain; assertions are on persisted content
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for cron fire")
	}

	// Give the post-send persistence a brief moment to land (the telegram
	// send and the persist are sequential, but both happen in the gocron
	// goroutine — we yield to it).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(persister.calls()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	calls := persister.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 AppendMessage calls (user + assistant), got %d: %+v", len(calls), calls)
	}
	if calls[0].role != "user" {
		t.Errorf("first persisted message role = %q, want %q", calls[0].role, "user")
	}
	if calls[1].role != "assistant" {
		t.Errorf("second persisted message role = %q, want %q", calls[1].role, "assistant")
	}
	if !strings.Contains(calls[0].content, "Daily paper digest") {
		t.Errorf("user marker should include reminder title, got: %s", calls[0].content)
	}
	if !strings.Contains(calls[0].content, prompt) {
		t.Errorf("user marker should include prompt so the agent can reason about what triggered the output, got: %s", calls[0].content)
	}
	if !strings.Contains(calls[1].content, "Digest body") {
		t.Errorf("assistant content should include cron result, got: %s", calls[1].content)
	}
	if !strings.Contains(calls[1].content, "Daily paper digest") {
		t.Errorf("assistant content should match what was sent to Telegram (title prefix), got: %s", calls[1].content)
	}
	if calls[0].convID != "conv-abc" || calls[1].convID != "conv-abc" {
		t.Errorf("both messages should target the active conversation ID %q, got user=%q assistant=%q", "conv-abc", calls[0].convID, calls[1].convID)
	}

	cancel()
	<-done
}

// TestFireCronTask_PersistsOnError asserts the error path also records the
// Telegram-sent error message into the conversation history, so the agent
// can see that a cron task failed when the user asks about it later.
func TestFireCronTask_PersistsOnError(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	mock := &mockCronRunner{err: fmt.Errorf("rate limit exceeded")}
	persister := &mockPersister{convID: "conv-err"}
	ra := NewReminderActor(db, tgInbox, time.UTC, signalCh, mock)
	ra.SetConversationPersister(persister)

	fireAt := time.Now().Add(-1 * time.Second).UTC()
	prompt := "do the thing"
	if _, err := db.Exec(`INSERT INTO reminders (user_id, chat_id, message, fire_at, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		7, 99, "Broken task", fireAt, prompt, time.Now().UTC()); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	select {
	case <-tgInbox:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for error notification")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(persister.calls()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	calls := persister.calls()
	if len(calls) != 2 {
		t.Fatalf("error path should persist user+assistant turn, got %d calls: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[1].content, "Cron task failed") || !strings.Contains(calls[1].content, "rate limit") {
		t.Errorf("assistant content should include the error text delivered to the user, got: %s", calls[1].content)
	}

	cancel()
	<-done
}

// TestFireCronTask_NilPersisterIsSafe asserts that the cron-persist path is
// a no-op when no ConversationPersister is configured. This keeps the
// existing (pre-v0.36.8) behavior for any deployment that doesn't wire
// in memory.Store, and guards against a nil-panic regression.
func TestFireCronTask_NilPersisterIsSafe(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	mock := &mockCronRunner{result: "ok"}
	ra := NewReminderActor(db, tgInbox, time.UTC, signalCh, mock)
	// Note: SetConversationPersister NOT called.

	fireAt := time.Now().Add(-1 * time.Second).UTC()
	prompt := "nothing special"
	if _, err := db.Exec(`INSERT INTO reminders (user_id, chat_id, message, fire_at, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		7, 99, "No-persister task", fireAt, prompt, time.Now().UTC()); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	select {
	case <-tgInbox:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for cron fire (persister=nil path must still deliver)")
	}
	cancel()
	<-done
}

func TestMigration_DuplicateColumn(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)

	// First init creates the table with prompt column.
	_, err := InitRemindSkills(db, signalCh, time.UTC)
	if err != nil {
		t.Fatalf("first InitRemindSkills: %v", err)
	}

	// Second init should not fail (ALTER TABLE duplicate column is handled).
	_, err = InitRemindSkills(db, signalCh, time.UTC)
	if err != nil {
		t.Fatalf("second InitRemindSkills should be idempotent: %v", err)
	}
}

// TestNewCronScheduler_FiresInConfiguredLocation is the regression guard for
// the Apr 15 incident where the scheduler was created with
// `gocron.NewScheduler()` (no location), so cron expressions evaluated in
// container-local time (UTC) instead of the user's configured timezone. A
// daily digest scheduled as "0 6 * * *" with timezone America/Los_Angeles
// fired at 06:00 UTC = 23:00 PDT the previous day, 7 hours early.
//
// The test uses TWO schedulers in different timezones and asserts their
// NextRun for "0 6 * * *" resolves to DIFFERENT UTC instants. That's the
// only way to catch the regression regardless of the host's time.Local,
// since a PDT-local dev machine would make the pre-fix path coincidentally
// pass a PDT-only test.
func TestNewCronScheduler_FiresInConfiguredLocation(t *testing.T) {
	pdt, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load Asia/Tokyo: %v", err)
	}

	pdtNext := nextCronRun(t, pdt, "0 6 * * *")
	tokyoNext := nextCronRun(t, tokyo, "0 6 * * *")

	// PDT is UTC-7/8 and Tokyo is UTC+9. "0 6 * * *" in each zone must produce
	// different UTC instants. If the schedulers ignore WithLocation and fall
	// back to time.Local, both NextRun calls share the same tz and return the
	// same UTC instant. That's exactly the bug this test is here to catch.
	if pdtNext.Equal(tokyoNext) {
		t.Fatalf("cron \"0 6 * * *\" resolved to the SAME UTC instant %s in both America/Los_Angeles and Asia/Tokyo. gocron.WithLocation is being ignored — most likely because it was dropped from newCronScheduler.",
			pdtNext.UTC().Format(time.RFC3339))
	}

	// Sanity: each scheduler fires at 06:00 in its own tz.
	if h := pdtNext.In(pdt).Hour(); h != 6 {
		t.Errorf("PDT scheduler: expected 06:00 PDT, got hour=%d (%s)", h, pdtNext.In(pdt).Format(time.RFC3339))
	}
	if h := tokyoNext.In(tokyo).Hour(); h != 6 {
		t.Errorf("Tokyo scheduler: expected 06:00 JST, got hour=%d (%s)", h, tokyoNext.In(tokyo).Format(time.RFC3339))
	}
}

func nextCronRun(t *testing.T, loc *time.Location, expr string) time.Time {
	t.Helper()
	sched, err := newCronScheduler(loc)
	if err != nil {
		t.Fatalf("newCronScheduler(%s): %v", loc, err)
	}
	t.Cleanup(func() { _ = sched.Shutdown() })
	sched.Start()
	job, err := sched.NewJob(
		gocron.CronJob(expr, false),
		gocron.NewTask(func() {}),
	)
	if err != nil {
		t.Fatalf("schedule %q in %s: %v", expr, loc, err)
	}
	next, err := job.NextRun()
	if err != nil {
		t.Fatalf("NextRun (%s): %v", loc, err)
	}
	return next
}
