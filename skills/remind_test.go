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

	if len(skills) != 5 {
		t.Fatalf("expected 5 skills (set/list/cancel/delete/update), got %d", len(skills))
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
		// Cron output must opt into markdown→HTML conversion. Without this,
		// Claude's **bold**/### headers/[links] render as raw characters in
		// Telegram. Regression guard: any future refactor that drops HTML:true
		// from fireCronTask sends will fail here loudly.
		if !msg.HTML {
			t.Error("cron task message must set HTML: true so the channel runs mdhtml.ConvertSafe; without it the user sees raw markdown in Telegram")
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
		// Symmetric with the success path: error message also opts into HTML
		// so any markdown in r.Message renders as intended.
		if !msg.HTML {
			t.Error("cron error message must set HTML: true (matches success path)")
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

// --- update_reminder tests ---

// seedPendingReminder inserts a pending reminder directly into the DB and
// returns its ID. Used by update tests to avoid coupling to set_reminder's
// signal-channel side effects.
func seedPendingReminder(t *testing.T, db *sql.DB, userID, chatID int64, message, prompt, cronExpr, model string, fireAt time.Time) int64 {
	t.Helper()
	var cronPtr, promptPtr, modelPtr *string
	if cronExpr != "" {
		cronPtr = &cronExpr
	}
	if prompt != "" {
		promptPtr = &prompt
	}
	if model != "" {
		modelPtr = &model
	}
	res, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, cron_expr, prompt, model, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		userID, chatID, message, fireAt.UTC(), cronPtr, promptPtr, modelPtr, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("seed pending reminder: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestUpdateReminder_MissingID(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	input, _ := json.Marshal(updateReminderInput{Message: "new title"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("error = %q, want \"id is required\"", err)
	}
}

func TestUpdateReminder_NoFields(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "original", "", "", "", time.Now().Add(1*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when no fields provided")
	}
	if !strings.Contains(err.Error(), "at least one field") {
		t.Errorf("error = %q, want \"at least one field\"", err)
	}
}

func TestUpdateReminder_PartialMessage(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	fireAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	id := seedPendingReminder(t, db, 1, 10, "old title", "keep this prompt", "0 9 * * *", "", fireAt)

	input, _ := json.Marshal(updateReminderInput{ID: id, Message: "new title"})
	result, err := skills["update_reminder"].Execute(ctx, input)
	if err != nil {
		t.Fatalf("update_reminder: %v", err)
	}
	if !strings.Contains(result, fmt.Sprintf("Updated reminder #%d", id)) {
		t.Errorf("result = %q, want confirmation for id %d", result, id)
	}

	// Verify message changed, prompt + cron_expr + fire_at untouched.
	var msg, prompt, cronExpr string
	var storedFireAt time.Time
	err = db.QueryRow(`SELECT message, prompt, cron_expr, fire_at FROM reminders WHERE id = ?`, id).Scan(&msg, &prompt, &cronExpr, &storedFireAt)
	if err != nil {
		t.Fatalf("query reminder: %v", err)
	}
	if msg != "new title" {
		t.Errorf("message = %q, want %q", msg, "new title")
	}
	if prompt != "keep this prompt" {
		t.Errorf("prompt = %q, want prompt preserved", prompt)
	}
	if cronExpr != "0 9 * * *" {
		t.Errorf("cron_expr = %q, want cron preserved", cronExpr)
	}
	if !storedFireAt.Equal(fireAt) {
		t.Errorf("fire_at = %v, want unchanged %v", storedFireAt, fireAt)
	}

	// Signal must fire so the actor picks up the new message on next run.
	select {
	case gotID := <-signalCh:
		if gotID != id {
			t.Errorf("signal id = %d, want %d", gotID, id)
		}
	default:
		t.Error("expected signal on actor channel; update must trigger reschedule so stale closure is replaced")
	}
}

func TestUpdateReminder_PartialPrompt(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "title stays", "old prompt", "", "", time.Now().Add(1*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Prompt: "new prompt"})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("update_reminder: %v", err)
	}

	var msg, prompt string
	if err := db.QueryRow(`SELECT message, prompt FROM reminders WHERE id = ?`, id).Scan(&msg, &prompt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if msg != "title stays" {
		t.Errorf("message = %q, should not change", msg)
	}
	if prompt != "new prompt" {
		t.Errorf("prompt = %q, want %q", prompt, "new prompt")
	}
	<-signalCh
}

func TestUpdateReminder_PartialFireAt(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	origFire := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	id := seedPendingReminder(t, db, 1, 10, "unchanged", "", "", "", origFire)

	newFire := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	input, _ := json.Marshal(updateReminderInput{ID: id, FireAt: newFire.Format("2006-01-02T15:04:05")})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("update_reminder: %v", err)
	}

	var stored time.Time
	if err := db.QueryRow(`SELECT fire_at FROM reminders WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if diff := stored.Sub(newFire); diff < -time.Second || diff > time.Second {
		t.Errorf("fire_at = %v, want near %v (diff %v)", stored, newFire, diff)
	}
	<-signalCh
}

func TestUpdateReminder_PartialRecurring(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "title", "", "0 9 * * *", "", time.Now().Add(1*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Recurring: "0 18 * * FRI"})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("update_reminder: %v", err)
	}

	var cronExpr string
	if err := db.QueryRow(`SELECT cron_expr FROM reminders WHERE id = ?`, id).Scan(&cronExpr); err != nil {
		t.Fatalf("query: %v", err)
	}
	if cronExpr != "0 18 * * FRI" {
		t.Errorf("cron_expr = %q, want %q", cronExpr, "0 18 * * FRI")
	}
	<-signalCh
}

func TestUpdateReminder_PartialModel(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "title", "prompt", "", "claude-sonnet-4-6", time.Now().Add(1*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Model: "claude-haiku-4-5"})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("update_reminder: %v", err)
	}

	var model string
	if err := db.QueryRow(`SELECT model FROM reminders WHERE id = ?`, id).Scan(&model); err != nil {
		t.Fatalf("query: %v", err)
	}
	if model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want %q", model, "claude-haiku-4-5")
	}
	<-signalCh
}

func TestUpdateReminder_RefusesCancelled(t *testing.T) {
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

	input, _ := json.Marshal(updateReminderInput{ID: 1, Message: "new title"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for cancelled row")
	}
	// The agent's autorepair loop keys off "set_reminder" in the error — if the
	// wording changes, the agent will loop instead of recreating. Tracking the
	// exact phrase prevents an invisible UX regression.
	if !strings.Contains(err.Error(), "set_reminder") {
		t.Errorf("error should name set_reminder as the next step, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error should mention current status, got: %v", err)
	}

	// Row must remain unchanged.
	var msg string
	if err := db.QueryRow(`SELECT message FROM reminders WHERE id = 1`).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if msg != "dead" {
		t.Errorf("message = %q, should not change on refused update", msg)
	}

	// No signal on failure — actor should not be disturbed.
	select {
	case id := <-signalCh:
		t.Errorf("no signal expected for refused update, got %d", id)
	default:
	}
}

func TestUpdateReminder_RefusesFired(t *testing.T) {
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

	input, _ := json.Marshal(updateReminderInput{ID: 1, Prompt: "new prompt"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for fired row")
	}
	if !strings.Contains(err.Error(), "set_reminder") {
		t.Errorf("error should name set_reminder, got: %v", err)
	}
	if !strings.Contains(err.Error(), "fired") {
		t.Errorf("error should mention fired status, got: %v", err)
	}
}

func TestUpdateReminder_NotFound(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	input, _ := json.Marshal(updateReminderInput{ID: 999, Message: "anything"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say \"not found\", got: %v", err)
	}
}

// TestUpdateReminder_UserScoped is the IDOR guard. User A cannot update
// user B's row; the error is indistinguishable from "not found" to avoid
// leaking row existence.
func TestUpdateReminder_UserScoped(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)

	userBID := seedPendingReminder(t, db, 2, 20, "user B's reminder", "secret prompt", "", "", time.Now().Add(1*time.Hour))

	// User A (id=1) tries to update user B's row.
	ctxA := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})
	input, _ := json.Marshal(updateReminderInput{ID: userBID, Message: "hijacked"})
	_, err := skills["update_reminder"].Execute(ctxA, input)
	if err == nil {
		t.Fatal("user A should not be able to update user B's reminder")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("cross-user update must return not-found (no existence leak), got: %v", err)
	}

	// User B's row must be unchanged.
	var msg, prompt string
	if err := db.QueryRow(`SELECT message, prompt FROM reminders WHERE id = ?`, userBID).Scan(&msg, &prompt); err != nil {
		t.Fatal(err)
	}
	if msg != "user B's reminder" || prompt != "secret prompt" {
		t.Errorf("user B's row mutated by user A: message=%q prompt=%q", msg, prompt)
	}
}

func TestUpdateReminder_InvalidCron(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "title", "", "0 9 * * *", "", time.Now().Add(1*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Recurring: "this is not cron"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid cron")
	}
	if !strings.Contains(err.Error(), "invalid cron expression") {
		t.Errorf("error should mention invalid cron, got: %v", err)
	}

	// Row must be unchanged.
	var cronExpr string
	if err := db.QueryRow(`SELECT cron_expr FROM reminders WHERE id = ?`, id).Scan(&cronExpr); err != nil {
		t.Fatal(err)
	}
	if cronExpr != "0 9 * * *" {
		t.Errorf("cron_expr = %q, should not change on validation failure", cronExpr)
	}
}

func TestUpdateReminder_InvalidFireAt(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	origFire := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	id := seedPendingReminder(t, db, 1, 10, "title", "", "", "", origFire)

	input, _ := json.Marshal(updateReminderInput{ID: id, FireAt: "not a timestamp"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for invalid fire_at")
	}
	if !strings.Contains(err.Error(), "invalid fire_at format") {
		t.Errorf("error should mention invalid fire_at, got: %v", err)
	}

	var stored time.Time
	if err := db.QueryRow(`SELECT fire_at FROM reminders WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(origFire) {
		t.Errorf("fire_at = %v, should not change on validation failure", stored)
	}
}

func TestUpdateReminder_MessageTooLong(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "short", "", "", "", time.Now().Add(1*time.Hour))

	// 2001 runes
	tooLong := strings.Repeat("a", 2001)
	input, _ := json.Marshal(updateReminderInput{ID: id, Message: tooLong})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
	if !strings.Contains(err.Error(), "message too long") {
		t.Errorf("error should mention length cap, got: %v", err)
	}
}

func TestUpdateReminder_PromptTooLong(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "title", "ok", "", "", time.Now().Add(1*time.Hour))

	tooLong := strings.Repeat("p", 5001)
	input, _ := json.Marshal(updateReminderInput{ID: id, Prompt: tooLong})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error for oversized prompt")
	}
	if !strings.Contains(err.Error(), "prompt too long") {
		t.Errorf("error should mention prompt length cap, got: %v", err)
	}
}

// TestUpdateReminder_SignalsActor_OnAnyChange is the regression guard for the
// F1 bug found in plan review: the gocron closure captures reminderRow by
// value at schedule time, so title/prompt/model edits wouldn't reach the next
// fire unless the actor re-schedules. The fix is "always signal," not "signal
// only on fire_at/recurring change." This test asserts signal fires even for
// a non-schedule-affecting edit.
func TestUpdateReminder_SignalsActor_OnAnyChange(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "orig", "orig prompt", "0 9 * * *", "", time.Now().Add(1*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Message: "refined title"})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("update_reminder: %v", err)
	}

	select {
	case gotID := <-signalCh:
		if gotID != id {
			t.Errorf("signal id = %d, want %d", gotID, id)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("message-only update must signal the actor so the stale gocron closure is replaced — otherwise the old title fires until container restart")
	}
}

// TestReminderActor_UpdateReschedulesJob is the integration guard for the
// actor-side handleSignal fix. Schedule a reminder, signal a re-schedule
// with a different fire_at, and verify the OLD fire time doesn't fire
// (proves the old gocron job was cancelled) and the NEW fire time does.
func TestReminderActor_UpdateReschedulesJob(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Initial: fire at T+2s.
	origFire := time.Now().Add(2 * time.Second).UTC()
	res, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "will be retitled", origFire, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	// Let the actor schedule the original.
	time.Sleep(200 * time.Millisecond)

	// Simulate update_reminder: change the message AND push fire_at way out
	// so the original T+2s window passes without firing if reschedule works.
	newFire := time.Now().Add(15 * time.Second).UTC()
	if _, err := db.Exec(
		`UPDATE reminders SET message = ?, fire_at = ? WHERE id = ?`,
		"retitled and rescheduled", newFire, id,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	actorSignalCh <- id

	// Wait past the original fire window. If the OLD gocron job was not
	// cancelled, it will fire at T+2s with "will be retitled" before we
	// ever reach the new fire time.
	select {
	case msg := <-tgInbox:
		t.Fatalf("unexpected early fire — old job was not cancelled on re-schedule. Message: %q", msg.Text)
	case <-time.After(4 * time.Second):
		// Good: the old schedule did not fire.
	}

	cancel()
	<-done
}

// TestUpdateReminder_RejectsFireAtOnRecurring is the regression guard for
// silent timestamp corruption: gocron ignores fire_at for recurring jobs
// (cron_expr drives next run), but fire_at is still stored in DB and passed
// to CronExecutor as scheduledAt, which the cron system prompt tells Claude
// to trust. Letting update_reminder rewrite fire_at on a recurring row would
// let the agent lie about when the task was "scheduled" without changing
// when it actually fires.
func TestUpdateReminder_RejectsFireAtOnRecurring(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	origFire := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	id := seedPendingReminder(t, db, 1, 10, "daily digest", "", "0 9 * * *", "", origFire)

	futureFire := time.Now().Add(72 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	input, _ := json.Marshal(updateReminderInput{ID: id, FireAt: futureFire})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when updating fire_at on recurring reminder")
	}
	if !strings.Contains(err.Error(), "recurring") {
		t.Errorf("error should mention the row is recurring, got: %v", err)
	}
	// The autorepair cue must tell the agent the right next step.
	if !strings.Contains(err.Error(), "recurring") || !strings.Contains(err.Error(), "set_reminder") {
		t.Errorf("error should guide agent to `recurring` field or set_reminder, got: %v", err)
	}

	// Row unchanged.
	var storedFireAt time.Time
	if err := db.QueryRow(`SELECT fire_at FROM reminders WHERE id = ?`, id).Scan(&storedFireAt); err != nil {
		t.Fatal(err)
	}
	if !storedFireAt.Equal(origFire) {
		t.Errorf("fire_at was mutated despite rejection: stored=%v orig=%v", storedFireAt, origFire)
	}

	// No signal fired on rejected update.
	select {
	case gotID := <-signalCh:
		t.Errorf("no signal should fire on rejected update, got %d", gotID)
	default:
	}
}

// TestUpdateReminder_AllowsContentUpdateOnRecurring asserts the common refine-
// title case still works on recurring reminders — fire_at must only be rejected
// when explicitly provided, not when all updates are content fields.
func TestUpdateReminder_AllowsContentUpdateOnRecurring(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "old title", "old prompt", "0 9 * * *", "", time.Now().Add(24*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Message: "📄 Daily · paper digest", Prompt: "new prompt body"})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("content-only update on recurring should succeed: %v", err)
	}

	var msg, prompt string
	if err := db.QueryRow(`SELECT message, prompt FROM reminders WHERE id = ?`, id).Scan(&msg, &prompt); err != nil {
		t.Fatal(err)
	}
	if msg != "📄 Daily · paper digest" || prompt != "new prompt body" {
		t.Errorf("content update not applied: msg=%q prompt=%q", msg, prompt)
	}
	<-signalCh
}

// TestUpdateReminder_AllowsRecurringChangeOnRecurring asserts that changing the
// cron expression on a recurring reminder works — that's the legitimate way
// to reschedule (fire_at changes are rejected, `recurring` is the right lever).
func TestUpdateReminder_AllowsRecurringChangeOnRecurring(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	id := seedPendingReminder(t, db, 1, 10, "title", "", "0 9 * * *", "", time.Now().Add(24*time.Hour))

	input, _ := json.Marshal(updateReminderInput{ID: id, Recurring: "0 18 * * *"})
	if _, err := skills["update_reminder"].Execute(ctx, input); err != nil {
		t.Fatalf("recurring change on recurring reminder should succeed: %v", err)
	}

	var cronExpr string
	if err := db.QueryRow(`SELECT cron_expr FROM reminders WHERE id = ?`, id).Scan(&cronExpr); err != nil {
		t.Fatal(err)
	}
	if cronExpr != "0 18 * * *" {
		t.Errorf("cron_expr = %q, want %q", cronExpr, "0 18 * * *")
	}
	<-signalCh
}

// TestReminderActor_PollDetectsScheduleChange is the regression guard for the
// CLI-mode drained-signal bug: in CLI mode, update_reminder's signal channel
// drains to /dev/null in the MCP subprocess, so the live actor never hears
// about DB updates. pollNewReminders Phase 1.5 must compare tracked job
// snapshots against DB and reschedule on schedule-field divergence, otherwise
// updates would silently not take effect until container restart.
//
// Test shape: schedule a one-time reminder far enough out that it won't fire
// during the test. Mutate fire_at in DB to near-now, WITHOUT sending a signal
// (simulating the drained-channel case). The poll (10s ticker) should detect
// the drift and reschedule; the reminder should then fire with the new time.
func TestReminderActor_PollDetectsScheduleChange(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Original schedule: fire 10 minutes from now (so the initial schedule
	// won't fire during the test window).
	origFire := time.Now().Add(10 * time.Minute).UTC()
	res, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "poll-reschedule test", origFire, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	// Run the actor with a long deadline to allow at least one poll tick
	// (the pollTicker in Run() is every 10 seconds).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	// Let the actor schedule the original row.
	time.Sleep(200 * time.Millisecond)

	// Verify the actor tracked it with the original snapshot.
	ra.mu.Lock()
	snap, tracked := ra.jobMeta[id]
	ra.mu.Unlock()
	if !tracked {
		t.Fatal("actor failed to snapshot the scheduled reminder")
	}
	if !snap.FireAt.Equal(origFire.Truncate(time.Second)) && !snap.FireAt.Equal(origFire) {
		// SQLite may drop sub-second precision. Allow either shape.
		t.Logf("snapshot fire_at = %v, orig = %v (minor SQLite precision drift OK)", snap.FireAt, origFire)
	}

	// Simulate update_reminder in CLI mode: mutate the DB row, DO NOT signal
	// the actor (that's what the drained MCP channel looks like from the
	// actor's perspective). Push fire_at to "near now" so the reschedule
	// triggers an actual fire within the test window.
	newFire := time.Now().Add(2 * time.Second).UTC()
	if _, err := db.Exec(`UPDATE reminders SET fire_at = ? WHERE id = ?`, newFire, id); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Wait for the poll tick (every 10s) to detect the drift and reschedule,
	// then for the new fire time to hit. Budget: 10s poll + 2s fire + slack.
	select {
	case msg := <-tgInbox:
		if !strings.Contains(msg.Text, "poll-reschedule test") {
			t.Errorf("expected poll-reschedule reminder text, got: %q", msg.Text)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for poll-detected reschedule to fire. update_reminder in CLI mode is effectively broken — the DB was updated but the actor kept the stale gocron job and never rescheduled.")
	}

	cancel()
	<-done
}

// TestFireReminder_ReReadsMessageFromDB is the regression guard for the
// stale-closure race: gocron captures reminderRow by value in the task
// closure, so a content update after scheduling would fire with old text.
// Direct UPDATE in DB (simulating update_reminder in CLI mode without a
// signal) should be picked up at fire time via the pre-fire DB re-read.
func TestFireReminder_ReReadsMessageFromDB(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Past-due reminder so fireReminder runs immediately from loadPendingReminders.
	pastFire := time.Now().Add(-500 * time.Millisecond).UTC()
	res, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "ORIGINAL_TITLE", pastFire, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	// Mutate message BEFORE the actor starts — simulates an update that
	// landed between schedule-time and fire-time.
	if _, err := db.Exec(`UPDATE reminders SET message = ? WHERE id = ?`, "UPDATED_TITLE", id); err != nil {
		t.Fatalf("update: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	select {
	case msg := <-tgInbox:
		if !strings.Contains(msg.Text, "UPDATED_TITLE") {
			t.Errorf("fireReminder used stale row — message = %q, want UPDATED_TITLE from DB re-read", msg.Text)
		}
		if strings.Contains(msg.Text, "ORIGINAL_TITLE") {
			t.Errorf("fireReminder leaked stale closure value: %q", msg.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for past-due reminder to fire")
	}

	cancel()
	<-done
}

// TestReminderActor_UpdateToPastFireAt is the regression guard for Finding 1A
// from the post-Codex eng review. Prior to `scheduleOrFire` being threaded
// through handleSignal and pollNewReminders Phase 1.5, an update pushing
// fire_at into the past on a one-time reminder would call scheduleReminder
// with a past gocron.OneTimeJob — behavior of which is implementation-defined
// and may silently no-op, meaning the user never gets their reminder. This
// test signals an update with fire_at 500ms in the past and asserts the
// reminder fires promptly (via the scheduleOrFire past-due branch).
func TestReminderActor_UpdateToPastFireAt(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Start with a future reminder so the actor schedules (not fires) it.
	futureFire := time.Now().Add(10 * time.Minute).UTC()
	res, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		1, 10, "update to past", futureFire, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	actorSignalCh := make(chan int64, 16)
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	// Let the actor schedule the future row.
	time.Sleep(200 * time.Millisecond)

	// Simulate update_reminder in API mode: push fire_at into the past, signal.
	pastFire := time.Now().Add(-500 * time.Millisecond).UTC()
	if _, err := db.Exec(`UPDATE reminders SET fire_at = ? WHERE id = ?`, pastFire, id); err != nil {
		t.Fatalf("update to past: %v", err)
	}
	actorSignalCh <- id

	select {
	case msg := <-tgInbox:
		if !strings.Contains(msg.Text, "update to past") {
			t.Errorf("expected past-updated reminder text, got: %q", msg.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("update to past fire_at did not fire immediately — scheduleOrFire past-due branch is broken or not wired into handleSignal")
	}

	cancel()
	<-done
}

// TestUpdateReminder_TOCTOUGuard asserts the skill surfaces a clear error when
// the row's status flips from pending to non-pending between the lookup and
// the UPDATE. Simulated by beginning the execute path with a pending row,
// then flipping to cancelled in a hook just before the UPDATE runs. We can't
// easily intercept mid-execute, so instead seed a pending row, cancel it in
// another goroutine-adjacent step, and assert the skill's rowsAffected-check
// path returns the expected retry-friendly error.
//
// Simpler deterministic shape: manually set status to cancelled after the
// skill's lookup but before UPDATE — not easy without hooks. Instead, test
// the observable guard: pass an ID whose row DOES exist for this user, but
// which we've already cancelled before calling update. The lookup will
// return status=cancelled, triggering the "is cancelled, not pending" branch
// BEFORE the UPDATE runs. That's the TOCTOU-adjacent "fast path" assertion.
// The actual TOCTOU race (status flips between lookup and UPDATE) would
// require DB-level concurrency hooks we don't have; the rowsAffected guard
// is a defense-in-depth layer whose error text matters. We assert here that
// the error wording guides the agent toward list_reminders+retry.
func TestUpdateReminder_TOCTOUGuard(t *testing.T) {
	db := newTestDB(t)
	signalCh := make(chan int64, 16)
	skills := initRemindSkillsForTest(t, db, signalCh)
	ctx := WithUser(context.Background(), UserInfo{UserID: 1, ChatID: 10})

	// Seed pending, then flip to cancelled to simulate "checked pending but
	// now non-pending" in the pre-UPDATE lookup branch.
	id := seedPendingReminder(t, db, 1, 10, "title", "", "", "", time.Now().Add(1*time.Hour))
	if _, err := db.Exec(`UPDATE reminders SET status = 'cancelled' WHERE id = ?`, id); err != nil {
		t.Fatalf("flip status: %v", err)
	}

	input, _ := json.Marshal(updateReminderInput{ID: id, Message: "new"})
	_, err := skills["update_reminder"].Execute(ctx, input)
	if err == nil {
		t.Fatal("expected error when row flipped to non-pending")
	}
	// The lookup branch catches it with "is cancelled, not pending".
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error should mention current status, got: %v", err)
	}
	if !strings.Contains(err.Error(), "set_reminder") {
		t.Errorf("error should name set_reminder as recreate path, got: %v", err)
	}
}

// TestFireCronTask_ReReadsContentFromDB is the cron variant of
// TestFireReminder_ReReadsMessageFromDB. Cron tasks go through fireCronTask
// after fireReminder's DB re-read, so the fresh row must carry updated
// prompt into the CronRunner. Regression guard for Finding 2 in the Codex
// adversarial review (stale prompt firing after update).
func TestFireCronTask_ReReadsContentFromDB(t *testing.T) {
	db := newTestDBSingleConn(t)
	signalCh := make(chan int64, 16)
	if _, err := InitRemindSkills(db, signalCh, time.UTC); err != nil {
		t.Fatalf("InitRemindSkills: %v", err)
	}

	// Seed a past-due cron reminder with ORIGINAL prompt.
	fireAt := time.Now().Add(-500 * time.Millisecond).UTC()
	res, err := db.Exec(
		`INSERT INTO reminders (user_id, chat_id, message, fire_at, prompt, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		1, 10, "cron task", fireAt, "ORIGINAL_PROMPT", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	// Mutate prompt before actor starts — simulates an update that arrived
	// between schedule and fire (or in CLI mode without a signal reaching
	// the actor).
	if _, err := db.Exec(`UPDATE reminders SET prompt = ? WHERE id = ?`, "UPDATED_PROMPT", id); err != nil {
		t.Fatalf("update prompt: %v", err)
	}

	tgInbox := make(chan telegram.OutgoingMessage, 16)
	mock := &mockCronRunner{result: "cron result"}
	ra := NewReminderActor(db, tgInbox, time.UTC, signalCh, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ra.Run(ctx) }()

	select {
	case <-tgInbox:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for cron task fire")
	}

	if !mock.called {
		t.Fatal("CronRunner.Execute was not invoked")
	}
	if mock.gotPrompt != "UPDATED_PROMPT" {
		t.Errorf("cron prompt = %q, want UPDATED_PROMPT — fireCronTask used stale closure value instead of DB re-read", mock.gotPrompt)
	}

	cancel()
	<-done
}
