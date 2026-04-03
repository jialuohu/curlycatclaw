package memory

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qdrant/go-client/qdrant"
)

const migrationBatchSize = 32

// MigrationManager runs background embedding migration from an old embedder
// to a new one, using versioned Qdrant collections and an atomic alias swap.
type MigrationManager struct {
	store  *Store
	vs     *VectorStore
	oldEmb Embedder // for dual-write to old (serving) collections
	newEmb Embedder // for re-embedding into new versioned collections
	state  *EmbedderState

	// New embedder identity for persisting after completion.
	newType  string
	newModel string
	newDim   int

	cancel context.CancelFunc
	done   chan struct{}
}

// NewMigrationManager creates a manager but does not start it.
func NewMigrationManager(store *Store, vs *VectorStore, oldEmb, newEmb Embedder, state *EmbedderState, newType, newModel string, newDim int) *MigrationManager {
	return &MigrationManager{
		store:    store,
		vs:       vs,
		oldEmb:   oldEmb,
		newEmb:   newEmb,
		state:    state,
		newType:  newType,
		newModel: newModel,
		newDim:   newDim,
		done:     make(chan struct{}),
	}
}

// SetVectorStore sets the vector store after it's been initialized.
// Must be called before Start.
func (m *MigrationManager) SetVectorStore(vs *VectorStore) {
	m.vs = vs
}

// Start launches the background migration goroutine.
func (m *MigrationManager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	go m.run(ctx)
}

// Stop requests cancellation and waits for the goroutine to finish.
func (m *MigrationManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	<-m.done
}

// Done returns a channel that is closed when migration completes (success or failure).
func (m *MigrationManager) Done() <-chan struct{} {
	return m.done
}

func (m *MigrationManager) run(ctx context.Context) {
	defer close(m.done)

	newVersion := m.state.MigratingVersion
	if newVersion == 0 {
		newVersion = m.state.ActiveVersion + 1
	}
	newCollections := VersionedNames(newVersion)

	slog.Info("background migration starting",
		"from", m.state.ActiveEmbedder,
		"to", m.newEmb.Name(),
		"version", newVersion,
	)

	// Create target collections if resuming from a fresh start.
	if m.state.MigrationStatus != "completing" {
		if err := m.ensureTargetCollections(ctx, newCollections); err != nil {
			slog.Error("migration: create target collections failed", "err", err)
			m.fail()
			return
		}

		// Enable dual-write so new content goes to both old and new collections.
		m.vs.EnableDualWrite(m.newEmb, newCollections)

		// Backfill all existing content from SQLite.
		if err := m.backfill(ctx, newCollections); err != nil {
			if ctx.Err() != nil {
				slog.Info("migration cancelled")
				return
			}
			slog.Error("migration backfill failed", "err", err)
			m.fail()
			return
		}

		// Catch-up phase: re-scan for rows created during backfill (A3).
		if err := m.catchUp(ctx, newCollections); err != nil {
			if ctx.Err() != nil {
				slog.Info("migration cancelled during catch-up")
				return
			}
			slog.Error("migration catch-up failed", "err", err)
			m.fail()
			return
		}

		if err := m.store.SetMigrationStatus("completing"); err != nil {
			slog.Error("migration: set completing status", "err", err)
			m.fail()
			return
		}
	}

	// Alias swap.
	if err := m.swapAliases(ctx, newVersion, newCollections); err != nil {
		if ctx.Err() != nil {
			slog.Info("migration cancelled during swap")
			return
		}
		slog.Error("migration alias swap failed", "err", err)
		m.fail()
		return
	}

	// Swap the live embedder so queries use the new model against new collections.
	m.vs.SwapEmbedder(m.newEmb)

	// Disable dual-write — new collections are now live with new embedder.
	m.vs.DisableDualWrite()

	// Clean up old versioned collections (best-effort).
	if m.state.ActiveVersion > 0 {
		oldCollections := VersionedNames(m.state.ActiveVersion)
		m.vs.DeleteCollections(ctx, oldCollections)
	}

	// Update SQLite state.
	if err := m.store.CompleteMigration(m.newEmb.Name(), newVersion, m.newType, m.newModel, m.newDim); err != nil {
		slog.Error("migration: complete state update failed", "err", err)
		return
	}

	slog.Info("background migration completed", "embedder", m.newEmb.Name(), "version", newVersion)
}

func (m *MigrationManager) ensureTargetCollections(ctx context.Context, names [3]string) error {
	for _, name := range names {
		exists, err := m.vs.client.CollectionExists(ctx, name)
		if err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if !exists {
			if err := m.vs.CreateCollection(ctx, name, m.newEmb.Dimension()); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		}
	}
	return nil
}

func (m *MigrationManager) backfill(ctx context.Context, newCollections [3]string) error {
	lastMsg := m.state.LastMsgID
	lastNote := m.state.LastNoteID
	lastSummary := m.state.LastSummaryID
	batchesSinceFlush := 0

	// Messages
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		texts, maxID, err := m.store.MessageTextsAfter(lastMsg, migrationBatchSize)
		if err != nil {
			return fmt.Errorf("messages: %w", err)
		}
		if len(texts) == 0 {
			break
		}
		if err := m.embedAndUpsert(ctx, texts, newCollections[0]); err != nil {
			return fmt.Errorf("messages upsert: %w", err)
		}
		lastMsg = maxID
		batchesSinceFlush++
		if batchesSinceFlush >= 10 {
			m.store.UpdateMigrationCursor(lastMsg, lastNote, lastSummary) //nolint:errcheck
			batchesSinceFlush = 0
		}
	}

	// Notes
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		texts, maxID, err := m.store.NoteTextsAfter(lastNote, migrationBatchSize)
		if err != nil {
			return fmt.Errorf("notes: %w", err)
		}
		if len(texts) == 0 {
			break
		}
		if err := m.embedAndUpsert(ctx, texts, newCollections[1]); err != nil {
			return fmt.Errorf("notes upsert: %w", err)
		}
		lastNote = maxID
		batchesSinceFlush++
		if batchesSinceFlush >= 10 {
			m.store.UpdateMigrationCursor(lastMsg, lastNote, lastSummary) //nolint:errcheck
			batchesSinceFlush = 0
		}
	}

	// Summaries
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		texts, maxID, err := m.store.SummaryTextsAfter(lastSummary, migrationBatchSize)
		if err != nil {
			return fmt.Errorf("summaries: %w", err)
		}
		if len(texts) == 0 {
			break
		}
		if err := m.embedAndUpsert(ctx, texts, newCollections[2]); err != nil {
			return fmt.Errorf("summaries upsert: %w", err)
		}
		lastSummary = maxID
		batchesSinceFlush++
		if batchesSinceFlush >= 10 {
			m.store.UpdateMigrationCursor(lastMsg, lastNote, lastSummary) //nolint:errcheck
			batchesSinceFlush = 0
		}
	}

	// Final cursor flush.
	m.store.UpdateMigrationCursor(lastMsg, lastNote, lastSummary) //nolint:errcheck
	return nil
}

// catchUp rescans for rows created during the backfill window.
// Repeats until all three sources return zero new rows (convergence).
func (m *MigrationManager) catchUp(ctx context.Context, newCollections [3]string) error {
	st, err := m.store.GetEmbedderState()
	if err != nil {
		return fmt.Errorf("load cursor: %w", err)
	}
	lastMsg, lastNote, lastSummary := st.LastMsgID, st.LastNoteID, st.LastSummaryID

	for round := 0; round < 10; round++ { // cap catch-up rounds
		if err := ctx.Err(); err != nil {
			return err
		}
		totalNew := 0

		texts, maxID, err := m.store.MessageTextsAfter(lastMsg, migrationBatchSize)
		if err != nil {
			return err
		}
		if len(texts) > 0 {
			if err := m.embedAndUpsert(ctx, texts, newCollections[0]); err != nil {
				return err
			}
			lastMsg = maxID
			totalNew += len(texts)
		}

		texts, maxID, err = m.store.NoteTextsAfter(lastNote, migrationBatchSize)
		if err != nil {
			return err
		}
		if len(texts) > 0 {
			if err := m.embedAndUpsert(ctx, texts, newCollections[1]); err != nil {
				return err
			}
			lastNote = maxID
			totalNew += len(texts)
		}

		texts, maxID, err = m.store.SummaryTextsAfter(lastSummary, migrationBatchSize)
		if err != nil {
			return err
		}
		if len(texts) > 0 {
			if err := m.embedAndUpsert(ctx, texts, newCollections[2]); err != nil {
				return err
			}
			lastSummary = maxID
			totalNew += len(texts)
		}

		m.store.UpdateMigrationCursor(lastMsg, lastNote, lastSummary) //nolint:errcheck

		if totalNew == 0 {
			slog.Info("migration catch-up converged", "rounds", round+1)
			return nil
		}
		slog.Info("migration catch-up round", "round", round+1, "new_rows", totalNew)
	}

	slog.Warn("migration catch-up did not fully converge after 10 rounds, proceeding with swap")
	return nil
}

func (m *MigrationManager) swapAliases(ctx context.Context, _ int, newCollections [3]string) error {
	if m.state.ActiveVersion == 0 {
		// First migration: convert raw collections to aliased scheme.
		return m.vs.BootstrapAliases(ctx, newCollections)
	}

	// Check if aliases already point to new collections (crash recovery).
	targets, err := m.vs.ListAliasTargets(ctx)
	if err != nil {
		return err
	}
	aliasNames := CollectionNames()
	alreadySwapped := true
	for i, name := range aliasNames {
		if targets[name] != newCollections[i] {
			alreadySwapped = false
			break
		}
	}
	if alreadySwapped {
		slog.Info("aliases already point to target collections (crash recovery)")
		return nil
	}

	oldCollections := VersionedNames(m.state.ActiveVersion)
	return m.vs.SwapAliases(ctx, newCollections, oldCollections)
}

func (m *MigrationManager) embedAndUpsert(ctx context.Context, texts []MigrationText, collection string) error {
	strs := make([]string, len(texts))
	for i, t := range texts {
		strs[i] = t.Text
	}

	vecs, err := m.newEmb.BatchEmbed(ctx, strs)
	if err != nil {
		return fmt.Errorf("batch embed: %w", err)
	}

	points := make([]*qdrant.PointStruct, len(texts))
	for i, t := range texts {
		payload := map[string]any{
			"user_id":    t.UserID,
			"chat_id":    t.ChatID,
			"text":       t.Text,
			"created_at": t.CreatedAt,
		}
		if t.ChatType != "" {
			payload["chat_type"] = t.ChatType
		}
		points[i] = &qdrant.PointStruct{
			Id:      qdrant.NewID(ToUUID(t.ID)),
			Vectors: qdrant.NewVectorsDense(vecs[i]),
			Payload: qdrant.NewValueMap(payload),
		}
	}

	return m.vs.BatchUpsert(ctx, collection, points)
}

func (m *MigrationManager) fail() {
	if err := m.store.SetMigrationStatus("failed"); err != nil {
		slog.Error("migration: failed to set failed status", "err", err)
	}
	m.vs.DisableDualWrite()
}
