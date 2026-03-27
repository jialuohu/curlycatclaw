package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

	if len(skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(skills))
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
	if result != "No pending reminders found" {
		t.Errorf("result = %q, want %q", result, "No pending reminders found")
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
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh)

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
	ra := NewReminderActor(db, tgInbox, time.UTC, actorSignalCh)

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
