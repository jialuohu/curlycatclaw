package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jialuohu/curlycatclaw/config"
	"github.com/jialuohu/curlycatclaw/internal/memory"
	"github.com/qdrant/go-client/qdrant"
)

const migrateBatchSize = 128

// runMigrateEmbedder wipes all vector collections and rebuilds them using the
// configured embedder. The bot must be stopped during migration.
//
// Flow:
//  1. Load config, open SQLite, connect to Qdrant
//  2. If dryRun: count texts per source, print summary, exit
//  3. Delete existing collections
//  4. Create new collections with target embedder dimensions
//  5. Read all texts from SQLite (messages, notes, summaries)
//  6. Batch embed and upsert in chunks of 128
//  7. Print summary
func runMigrateEmbedder(configPath string, dryRun bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := setupLogging(cfg.Logging); err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}

	if !cfg.Vector.Enabled {
		return fmt.Errorf("vector search is not enabled in config; nothing to migrate")
	}

	// Open SQLite store (read-only for content, no writes).
	store, err := memory.NewStore(cfg.Storage.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// Load all texts from SQLite.
	messages, err := store.AllMessageTexts()
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}
	notes, err := store.AllNoteTexts()
	if err != nil {
		return fmt.Errorf("load notes: %w", err)
	}
	summaries, err := store.AllSummaryTexts()
	if err != nil {
		return fmt.Errorf("load summaries: %w", err)
	}

	total := len(messages) + len(notes) + len(summaries)
	slog.Info("texts loaded from SQLite",
		"messages", len(messages),
		"notes", len(notes),
		"summaries", len(summaries),
		"total", total,
	)

	if dryRun {
		fmt.Printf("dry-run: would migrate %d messages, %d notes, %d summaries (%d total)\n",
			len(messages), len(notes), len(summaries), total)
		return nil
	}

	if total == 0 {
		fmt.Println("no texts to migrate")
		return nil
	}

	// Initialize embedder.
	embedder := newEmbedder(cfg.Vector)
	slog.Info("target embedder", "name", embedder.Name(), "dim", embedder.Dimension())

	// Connect to Qdrant (without auto-creating collections).
	ctx := context.Background()
	vs, err := memory.NewVectorStoreRaw(ctx, cfg.Vector.QdrantAddr)
	if err != nil {
		return fmt.Errorf("connect to qdrant: %w", err)
	}
	defer vs.Close()

	// Delete existing collections.
	collections := memory.CollectionNames()
	for _, name := range collections {
		slog.Info("deleting collection", "name", name)
		if err := vs.DeleteCollection(ctx, name); err != nil {
			return fmt.Errorf("delete collection %s: %w", name, err)
		}
	}

	// Create new collections with the target embedder's dimensions.
	for _, name := range collections {
		slog.Info("creating collection", "name", name, "dim", embedder.Dimension())
		if err := vs.CreateCollection(ctx, name, embedder.Dimension()); err != nil {
			return fmt.Errorf("create collection %s: %w", name, err)
		}
	}

	// Migrate each source.
	start := time.Now()
	if err := migrateTexts(ctx, vs, embedder, collections[0], messages); err != nil {
		return fmt.Errorf("migrate messages: %w", err)
	}
	if err := migrateTexts(ctx, vs, embedder, collections[1], notes); err != nil {
		return fmt.Errorf("migrate notes: %w", err)
	}
	if err := migrateSummaries(ctx, vs, embedder, collections[2], summaries); err != nil {
		return fmt.Errorf("migrate summaries: %w", err)
	}

	fmt.Printf("migration complete: %d messages, %d notes, %d summaries in %s\n",
		len(messages), len(notes), len(summaries), time.Since(start).Round(time.Millisecond))
	return nil
}

// migrateTexts embeds and upserts a slice of MigrationTexts in batches.
func migrateTexts(ctx context.Context, vs *memory.VectorStore, embedder memory.Embedder, collection string, texts []memory.MigrationText) error {
	for i := 0; i < len(texts); i += migrateBatchSize {
		end := i + migrateBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		// Extract text strings for batch embedding.
		strs := make([]string, len(batch))
		for j, t := range batch {
			strs[j] = t.Text
		}

		vecs, err := embedder.BatchEmbed(ctx, strs)
		if err != nil {
			return fmt.Errorf("embed batch [%d:%d]: %w", i, end, err)
		}

		// Build Qdrant points.
		points := make([]*qdrant.PointStruct, len(batch))
		for j, t := range batch {
			payload := qdrant.NewValueMap(map[string]any{
				"user_id":    t.UserID,
				"chat_id":    t.ChatID,
				"text":       t.Text,
				"created_at": time.Now().UTC().Format(time.RFC3339),
			})
			points[j] = &qdrant.PointStruct{
				Id:      qdrant.NewID(memory.ToUUID(t.ID)),
				Vectors: qdrant.NewVectorsDense(vecs[j]),
				Payload: payload,
			}
		}

		if err := vs.BatchUpsert(ctx, collection, points); err != nil {
			return fmt.Errorf("upsert batch [%d:%d]: %w", i, end, err)
		}

		slog.Info("migrated", "source", texts[0].Source, "progress", fmt.Sprintf("%d/%d", end, len(texts)))
	}
	return nil
}

// migrateSummaries is like migrateTexts but includes chat_type metadata.
func migrateSummaries(ctx context.Context, vs *memory.VectorStore, embedder memory.Embedder, collection string, texts []memory.MigrationText) error {
	for i := 0; i < len(texts); i += migrateBatchSize {
		end := i + migrateBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		strs := make([]string, len(batch))
		for j, t := range batch {
			strs[j] = t.Text
		}

		vecs, err := embedder.BatchEmbed(ctx, strs)
		if err != nil {
			return fmt.Errorf("embed summaries batch [%d:%d]: %w", i, end, err)
		}

		points := make([]*qdrant.PointStruct, len(batch))
		for j, t := range batch {
			payload := qdrant.NewValueMap(map[string]any{
				"user_id":    t.UserID,
				"chat_id":    t.ChatID,
				"chat_type":  t.ChatType,
				"text":       t.Text,
				"created_at": time.Now().UTC().Format(time.RFC3339),
			})
			points[j] = &qdrant.PointStruct{
				Id:      qdrant.NewID(memory.ToUUID(t.ID)),
				Vectors: qdrant.NewVectorsDense(vecs[j]),
				Payload: payload,
			}
		}

		if err := vs.BatchUpsert(ctx, collection, points); err != nil {
			return fmt.Errorf("upsert summaries batch [%d:%d]: %w", i, end, err)
		}

		slog.Info("migrated", "source", "summary", "progress", fmt.Sprintf("%d/%d", end, len(texts)))
	}
	return nil
}
