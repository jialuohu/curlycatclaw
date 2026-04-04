package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qdrant/go-client/qdrant"
)

const (
	collectionMessages     = "curlycatclaw_messages"
	collectionNotes        = "curlycatclaw_notes"
	collectionSummaries    = "curlycatclaw_summaries"
	observationsCollection = "curlycatclaw_observations"
)

// SearchResult holds a single vector search result.
type SearchResult struct {
	ID        string
	Text      string
	Source    string
	Score     float32
	CreatedAt string
	ChatType  string // "private", "group", "supergroup", or "" (legacy)
}

// dualWriteState holds the state for dual-writing to a new collection during migration.
type dualWriteState struct {
	embedder    Embedder  // new embedder for the target collection
	collections [3]string // versioned collection names [messages, notes, summaries]
}

// VectorStore provides vector search backed by Qdrant.
type VectorStore struct {
	client   *qdrant.Client
	embedder Embedder
	embSwap  atomic.Value // holds Embedder; non-nil after migration swap
	dw       atomic.Pointer[dualWriteState] // non-nil during migration
	obsCollMu   sync.Mutex // guards lazy creation of observations collection
	obsCollDone bool       // true after observations collection is successfully created
}

// activeEmbedder returns the current embedder, checking for a post-migration swap.
func (vs *VectorStore) activeEmbedder() Embedder {
	if swapped := vs.embSwap.Load(); swapped != nil {
		return swapped.(Embedder)
	}
	return vs.embedder
}

// SwapEmbedder atomically replaces the live embedder (called after migration alias swap).
func (vs *VectorStore) SwapEmbedder(newEmb Embedder) {
	vs.embSwap.Store(newEmb)
	slog.Info("live embedder swapped", "new", newEmb.Name())
}

// NewVectorStoreRaw connects to Qdrant at addr without creating collections or
// binding an embedder. Used by the migration command which manages collections
// and embeddings itself.
func NewVectorStoreRaw(ctx context.Context, addr string) (*VectorStore, error) {
	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: parse addr: %w", err)
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:     host,
		Port:     port,
		PoolSize: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("vectorstore: connect: %w", err)
	}

	// Verify connectivity by listing collections.
	_, err = client.ListCollections(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("vectorstore: verify connection: %w", err)
	}

	return &VectorStore{client: client}, nil
}

// NewVectorStore connects to Qdrant at addr and ensures required collections exist.
// The embedder determines vector dimensions and embedding strategy.
func NewVectorStore(ctx context.Context, addr string, embedder Embedder) (*VectorStore, error) {
	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: parse addr: %w", err)
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:     host,
		Port:     port,
		PoolSize: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("vectorstore: connect: %w", err)
	}

	vs := &VectorStore{client: client, embedder: embedder}

	// If aliases exist (versioned mode), skip ensureCollection to avoid
	// recreating raw collections that would shadow the aliases (A4).
	hasAliases, _ := vs.HasAliases(ctx)
	if !hasAliases {
		if err := vs.ensureCollection(ctx, collectionMessages); err != nil {
			client.Close()
			return nil, err
		}
		if err := vs.ensureCollection(ctx, collectionNotes); err != nil {
			client.Close()
			return nil, err
		}
		if err := vs.ensureCollection(ctx, collectionSummaries); err != nil {
			client.Close()
			return nil, err
		}
	}

	return vs, nil
}

// Index upserts a text document into the appropriate collection.
// source must be "message", "note", or "summary".
func (vs *VectorStore) Index(ctx context.Context, id string, text string, userID int64, chatID int64, source string) error {
	collection := collectionForSource(source)
	vec, err := vs.activeEmbedder().Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("vectorstore: embed: %w", err)
	}

	payload := qdrant.NewValueMap(map[string]any{
		"user_id":    userID,
		"chat_id":    chatID,
		"text":       text,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})

	pointID := qdrant.NewID(ToUUID(id))
	_, primaryErr := vs.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: collection,
		Points: []*qdrant.PointStruct{
			{
				Id:      pointID,
				Vectors: qdrant.NewVectorsDense(vec),
				Payload: payload,
			},
		},
	})

	// Dual-write to new collection during migration (best-effort).
	// Runs even if primary upsert failed (e.g., during v0→v1 bootstrap gap).
	if dw := vs.dw.Load(); dw != nil {
		dwCol := dw.collections[collectionIndex(source)]
		if vec2, err2 := dw.embedder.Embed(ctx, text); err2 == nil {
			if _, err2 = vs.client.Upsert(ctx, &qdrant.UpsertPoints{
				CollectionName: dwCol,
				Points:         []*qdrant.PointStruct{{Id: pointID, Vectors: qdrant.NewVectorsDense(vec2), Payload: payload}},
			}); err2 != nil {
				slog.Warn("dual-write upsert failed", "collection", dwCol, "err", err2)
			}
		} else {
			slog.Warn("dual-write embed failed", "err", err2)
		}
	}

	if primaryErr != nil {
		return fmt.Errorf("vectorstore: upsert: %w", primaryErr)
	}
	return nil
}

// IndexSummary upserts a summary with chat_type metadata for chat-type-aware retrieval.
func (vs *VectorStore) IndexSummary(ctx context.Context, id string, text string, userID int64, chatID int64, chatType string) error {
	vec, err := vs.activeEmbedder().Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("vectorstore: embed: %w", err)
	}

	payload := qdrant.NewValueMap(map[string]any{
		"user_id":    userID,
		"chat_id":    chatID,
		"chat_type":  chatType,
		"text":       text,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})

	pointID := qdrant.NewID(ToUUID(id))
	_, primaryErr := vs.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: collectionSummaries,
		Points: []*qdrant.PointStruct{
			{
				Id:      pointID,
				Vectors: qdrant.NewVectorsDense(vec),
				Payload: payload,
			},
		},
	})

	// Dual-write to new summaries collection during migration (best-effort).
	// Runs even if primary upsert failed (e.g., during v0→v1 bootstrap gap).
	if dw := vs.dw.Load(); dw != nil {
		if vec2, err2 := dw.embedder.Embed(ctx, text); err2 == nil {
			if _, err2 = vs.client.Upsert(ctx, &qdrant.UpsertPoints{
				CollectionName: dw.collections[2],
				Points:         []*qdrant.PointStruct{{Id: pointID, Vectors: qdrant.NewVectorsDense(vec2), Payload: payload}},
			}); err2 != nil {
				slog.Warn("dual-write summary upsert failed", "err", err2)
			}
		} else {
			slog.Warn("dual-write summary embed failed", "err", err2)
		}
	}

	if primaryErr != nil {
		return fmt.Errorf("vectorstore: upsert summary: %w", primaryErr)
	}
	return nil
}

// Search queries both collections for documents matching the query,
// filtered by userID, and returns the top limit results.
func (vs *VectorStore) Search(ctx context.Context, query string, userID int64, limit int) ([]SearchResult, error) {
	vec, err := vs.activeEmbedder().Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: embed query: %w", err)
	}
	if limit <= 0 {
		limit = 5
	}
	queryLimit := uint64(limit)

	filter := &qdrant.Filter{
		Must: []*qdrant.Condition{
			qdrant.NewMatchInt("user_id", userID),
		},
	}

	var allResults []SearchResult

	for _, coll := range []struct {
		name   string
		source string
	}{
		{collectionMessages, "message"},
		{collectionNotes, "note"},
	} {
		scored, err := vs.client.Query(ctx, &qdrant.QueryPoints{
			CollectionName: coll.name,
			Query:          qdrant.NewQueryDense(vec),
			Filter:         filter,
			Limit:          &queryLimit,
			WithPayload:    qdrant.NewWithPayload(true),
		})
		if err != nil {
			return nil, fmt.Errorf("vectorstore: query %s: %w", coll.name, err)
		}

		for _, sp := range scored {
			r := SearchResult{
				Source: coll.source,
				Score:  sp.Score,
			}
			if sp.Id != nil {
				if uuid := sp.Id.GetUuid(); uuid != "" {
					r.ID = uuid
				}
			}
			if v, ok := sp.Payload["text"]; ok {
				r.Text = v.GetStringValue()
			}
			if v, ok := sp.Payload["created_at"]; ok {
				r.CreatedAt = v.GetStringValue()
			}
			allResults = append(allResults, r)
		}
	}

	// Sort by score descending.
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})

	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	return allResults, nil
}

// SearchSummaries queries the summaries collection for documents matching the query.
// For private chats: returns all private summaries for this user (cross-DM memory).
// For group/supergroup chats: returns only summaries from this specific chat.
// Legacy vectors without chat_type are treated as private.
func (vs *VectorStore) SearchSummaries(ctx context.Context, query string, userID, chatID int64, chatType string, limit int, scoreThreshold float32) ([]SearchResult, error) {
	vec, err := vs.activeEmbedder().Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: embed query: %w", err)
	}
	if limit <= 0 {
		limit = 3
	}
	queryLimit := uint64(limit)

	var filter *qdrant.Filter
	if chatType == "private" || chatType == "" {
		// Private chats: search all private summaries for this user.
		// Include vectors with empty/missing chat_type (legacy) as private.
		filter = &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchInt("user_id", userID),
			},
			MustNot: []*qdrant.Condition{
				qdrant.NewMatchKeyword("chat_type", "group"),
				qdrant.NewMatchKeyword("chat_type", "supergroup"),
			},
		}
	} else {
		// Group/supergroup: only this chat's summaries.
		filter = &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchInt("user_id", userID),
				qdrant.NewMatchInt("chat_id", chatID),
			},
		}
	}

	scored, err := vs.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: collectionSummaries,
		Query:          qdrant.NewQueryDense(vec),
		Filter:         filter,
		Limit:          &queryLimit,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("vectorstore: query summaries: %w", err)
	}

	var results []SearchResult
	for _, sp := range scored {
		if sp.Score < scoreThreshold {
			continue
		}
		r := SearchResult{
			Source: "summary",
			Score:  sp.Score,
		}
		if sp.Id != nil {
			if uuid := sp.Id.GetUuid(); uuid != "" {
				r.ID = uuid
			}
		}
		if v, ok := sp.Payload["text"]; ok {
			r.Text = v.GetStringValue()
		}
		if v, ok := sp.Payload["created_at"]; ok {
			r.CreatedAt = v.GetStringValue()
		}
		if v, ok := sp.Payload["chat_type"]; ok {
			r.ChatType = v.GetStringValue()
		}
		results = append(results, r)
	}

	return results, nil
}

// IndexObservation upserts an observation into the observations collection.
// The text embedded is "Title. Summary". The collection is created on first write.
func (vs *VectorStore) IndexObservation(ctx context.Context, obs Observation) error {
	text := obs.Title + ". " + obs.Summary
	vec, err := vs.activeEmbedder().Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("vectorstore: embed observation: %w", err)
	}

	if err := vs.ensureObservationsCollection(ctx); err != nil {
		return err
	}

	payload := qdrant.NewValueMap(map[string]any{
		"user_id":    obs.UserID,
		"chat_id":    obs.ChatID,
		"chat_type":  obs.ChatType,
		"type":       obs.Type,
		"importance": obs.Importance,
		"obs_id":     obs.ID,
		"title":      obs.Title,
		"summary":    obs.Summary,
		"created_at": obs.CreatedAt.Format(time.RFC3339),
	})

	pointID := qdrant.NewID(ToUUID(obs.ID))
	_, primaryErr := vs.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: observationsCollection,
		Points: []*qdrant.PointStruct{
			{
				Id:      pointID,
				Vectors: qdrant.NewVectorsDense(vec),
				Payload: payload,
			},
		},
	})

	// Dual-write to new collection during migration (best-effort).
	if dw := vs.dw.Load(); dw != nil {
		if vec2, err2 := dw.embedder.Embed(ctx, text); err2 == nil {
			// Observations are not part of the standard [3]string triplet,
			// so dual-write uses the same collection name (Phase 1: no migration integration).
			if _, err2 = vs.client.Upsert(ctx, &qdrant.UpsertPoints{
				CollectionName: observationsCollection,
				Points:         []*qdrant.PointStruct{{Id: pointID, Vectors: qdrant.NewVectorsDense(vec2), Payload: payload}},
			}); err2 != nil {
				slog.Warn("dual-write observation upsert failed", "err", err2)
			}
		} else {
			slog.Warn("dual-write observation embed failed", "err", err2)
		}
	}

	if primaryErr != nil {
		return fmt.Errorf("vectorstore: upsert observation: %w", primaryErr)
	}
	return nil
}

// SearchObservations queries the observations collection for relevant observations.
// For private chats: returns all private observations for this user.
// For group/supergroup chats: returns only observations from this specific chat.
// Results are re-ranked by score * recencyWeight * importanceWeight.
// Observations with importance < 3 are filtered out entirely.
func (vs *VectorStore) SearchObservations(ctx context.Context, query string, userID, chatID int64, chatType string, limit int, scoreThreshold float32) ([]ObservationResult, error) {
	// Ensure collection exists (no-op if already created by IndexObservation).
	if err := vs.ensureObservationsCollection(ctx); err != nil {
		return nil, nil // Collection doesn't exist yet, return empty results.
	}

	vec, err := vs.activeEmbedder().Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: embed query: %w", err)
	}
	if limit <= 0 {
		limit = 5
	}
	// Fetch more than needed so we can filter by importance and re-rank.
	fetchLimit := uint64(limit * 3)
	if fetchLimit < 20 {
		fetchLimit = 20
	}

	var filter *qdrant.Filter
	if chatType == "private" || chatType == "" {
		// Private chats: search all private observations for this user.
		filter = &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchInt("user_id", userID),
			},
			MustNot: []*qdrant.Condition{
				qdrant.NewMatchKeyword("chat_type", "group"),
				qdrant.NewMatchKeyword("chat_type", "supergroup"),
			},
		}
	} else {
		// Group/supergroup: only this chat's observations.
		filter = &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchInt("user_id", userID),
				qdrant.NewMatchInt("chat_id", chatID),
			},
		}
	}

	scored, err := vs.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: observationsCollection,
		Query:          qdrant.NewQueryDense(vec),
		Filter:         filter,
		Limit:          &fetchLimit,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("vectorstore: query observations: %w", err)
	}

	now := time.Now()
	var results []ObservationResult
	for _, sp := range scored {
		if sp.Score < scoreThreshold {
			continue
		}

		var importance int
		if v, ok := sp.Payload["importance"]; ok {
			importance = int(v.GetIntegerValue())
		}
		// Filter out low-importance observations entirely.
		if importance < 3 {
			continue
		}

		r := ObservationResult{
			Importance: importance,
			Score:      sp.Score,
		}
		if v, ok := sp.Payload["obs_id"]; ok {
			r.ID = v.GetStringValue()
		}
		if v, ok := sp.Payload["type"]; ok {
			r.Type = v.GetStringValue()
		}
		if v, ok := sp.Payload["title"]; ok {
			r.Title = v.GetStringValue()
		}
		if v, ok := sp.Payload["summary"]; ok {
			r.Summary = v.GetStringValue()
		}
		if v, ok := sp.Payload["created_at"]; ok {
			r.CreatedAt = v.GetStringValue()
		}

		// Re-rank: score * recencyWeight * importanceWeight
		var daysAgo float64
		if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
			daysAgo = math.Max(0, now.Sub(t).Hours()/24.0)
		}
		recencyWeight := 1.0 / (1.0 + daysAgo*0.05)
		importanceWeight := 0.5 + (float64(importance) / 20.0)
		rankScore := float64(sp.Score) * recencyWeight * importanceWeight
		r.Score = float32(rankScore)

		results = append(results, r)
	}

	// Sort by re-ranked score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// DeleteObservationVector deletes an observation's vector from the observations collection.
func (vs *VectorStore) DeleteObservationVector(ctx context.Context, obsID string) error {
	pointID := qdrant.NewID(ToUUID(obsID))
	_, err := vs.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: observationsCollection,
		Points:         qdrant.NewPointsSelector(pointID),
	})
	if err != nil {
		return fmt.Errorf("vectorstore: delete observation: %w", err)
	}
	return nil
}

// DeleteCollection deletes a Qdrant collection by name.
// Returns nil if the collection does not exist.
func (vs *VectorStore) DeleteCollection(ctx context.Context, name string) error {
	exists, err := vs.client.CollectionExists(ctx, name)
	if err != nil {
		return fmt.Errorf("vectorstore: check collection %s: %w", name, err)
	}
	if !exists {
		return nil
	}
	err = vs.client.DeleteCollection(ctx, name)
	if err != nil {
		return fmt.Errorf("vectorstore: delete collection %s: %w", name, err)
	}
	return nil
}

// CreateCollection creates a Qdrant collection with the given name and vector dimension.
func (vs *VectorStore) CreateCollection(ctx context.Context, name string, dim uint64) error {
	return vs.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: name,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     dim,
			Distance: qdrant.Distance_Cosine,
		}),
	})
}

// BatchUpsert upserts multiple points into a collection.
func (vs *VectorStore) BatchUpsert(ctx context.Context, collection string, points []*qdrant.PointStruct) error {
	_, err := vs.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: collection,
		Points:         points,
	})
	if err != nil {
		return fmt.Errorf("vectorstore: batch upsert: %w", err)
	}
	return nil
}

// CollectionNames returns the names of the three standard collections.
func CollectionNames() [3]string {
	return [3]string{collectionMessages, collectionNotes, collectionSummaries}
}

// VersionedNames returns versioned collection names for the given version.
func VersionedNames(version int) [3]string {
	base := CollectionNames()
	return [3]string{
		fmt.Sprintf("%s_v%d", base[0], version),
		fmt.Sprintf("%s_v%d", base[1], version),
		fmt.Sprintf("%s_v%d", base[2], version),
	}
}

// collectionIndex maps a source type to its index in the collection triplet.
func collectionIndex(source string) int {
	switch source {
	case "note":
		return 1
	case "summary":
		return 2
	default:
		return 0
	}
}

// EnableDualWrite activates dual-writing to new versioned collections.
func (vs *VectorStore) EnableDualWrite(newEmb Embedder, newCollections [3]string) {
	vs.dw.Store(&dualWriteState{embedder: newEmb, collections: newCollections})
	slog.Info("dual-write enabled", "collections", newCollections)
}

// DisableDualWrite stops dual-writing.
func (vs *VectorStore) DisableDualWrite() {
	vs.dw.Store(nil)
	slog.Info("dual-write disabled")
}

// DeleteCollections deletes the three named collections (best-effort, logs errors).
func (vs *VectorStore) DeleteCollections(ctx context.Context, names [3]string) {
	for _, name := range names {
		if err := vs.DeleteCollection(ctx, name); err != nil {
			slog.Warn("failed to delete collection", "name", name, "err", err)
		}
	}
}

// SwapAliases atomically repoints the three standard aliases to new versioned collections.
// Used for vN→vN+1 transitions where aliases already exist.
func (vs *VectorStore) SwapAliases(ctx context.Context, newCollections, oldCollections [3]string) error {
	aliasNames := CollectionNames()
	var actions []*qdrant.AliasOperations
	for i := range 3 {
		actions = append(actions,
			&qdrant.AliasOperations{
				Action: &qdrant.AliasOperations_DeleteAlias{
					DeleteAlias: &qdrant.DeleteAlias{AliasName: aliasNames[i]},
				},
			},
			&qdrant.AliasOperations{
				Action: &qdrant.AliasOperations_CreateAlias{
					CreateAlias: &qdrant.CreateAlias{
						AliasName:      aliasNames[i],
						CollectionName: newCollections[i],
					},
				},
			},
		)
	}
	if err := vs.client.UpdateAliases(ctx, actions); err != nil {
		return fmt.Errorf("vectorstore: swap aliases: %w", err)
	}
	slog.Info("aliases swapped", "new", newCollections)
	return nil
}

// BootstrapAliases converts raw v0 collections to the versioned+alias scheme.
// This deletes the raw collections and creates aliases pointing to the new versioned collections.
// There is a brief gap during this one-time operation.
func (vs *VectorStore) BootstrapAliases(ctx context.Context, newCollections [3]string) error {
	aliasNames := CollectionNames()
	slog.Warn("converting to versioned collections (one-time operation, brief search gap)")

	// Delete raw collections (they can't coexist with same-name aliases).
	for _, name := range aliasNames {
		if err := vs.DeleteCollection(ctx, name); err != nil {
			return fmt.Errorf("vectorstore: bootstrap delete %s: %w", name, err)
		}
	}

	// Create aliases pointing to the new versioned collections.
	var actions []*qdrant.AliasOperations
	for i := range 3 {
		actions = append(actions, &qdrant.AliasOperations{
			Action: &qdrant.AliasOperations_CreateAlias{
				CreateAlias: &qdrant.CreateAlias{
					AliasName:      aliasNames[i],
					CollectionName: newCollections[i],
				},
			},
		})
	}
	if err := vs.client.UpdateAliases(ctx, actions); err != nil {
		return fmt.Errorf("vectorstore: bootstrap aliases: %w", err)
	}
	slog.Info("versioned collection aliases created", "collections", newCollections)
	return nil
}

// ListAliasTargets returns a map of alias→collection for all curlycatclaw aliases.
func (vs *VectorStore) ListAliasTargets(ctx context.Context) (map[string]string, error) {
	aliases, err := vs.client.ListAliases(ctx)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: list aliases: %w", err)
	}
	result := make(map[string]string)
	for _, a := range aliases {
		if strings.HasPrefix(a.AliasName, "curlycatclaw_") {
			result[a.AliasName] = a.CollectionName
		}
	}
	return result, nil
}

// HasAliases checks whether the standard collection names are aliases (not raw collections).
func (vs *VectorStore) HasAliases(ctx context.Context) (bool, error) {
	targets, err := vs.ListAliasTargets(ctx)
	if err != nil {
		return false, err
	}
	// If any of the standard names appear as aliases, we're in versioned mode.
	for _, name := range CollectionNames() {
		if _, ok := targets[name]; ok {
			return true, nil
		}
	}
	return false, nil
}

// Close tears down the gRPC connection.
// It is safe to call Close on a zero-value VectorStore or multiple times.
func (vs *VectorStore) Close() error {
	if vs.client == nil {
		return nil
	}
	err := vs.client.Close()
	vs.client = nil
	return err
}

// ensureObservationsCollection lazily creates the observations collection.
// Unlike sync.Once, this retries on transient failures (e.g., Qdrant
// temporarily unreachable) so a single network blip doesn't permanently
// disable observations for the process lifetime.
func (vs *VectorStore) ensureObservationsCollection(ctx context.Context) error {
	vs.obsCollMu.Lock()
	defer vs.obsCollMu.Unlock()
	if vs.obsCollDone {
		return nil
	}
	if err := vs.ensureCollection(ctx, observationsCollection); err != nil {
		return err
	}
	vs.obsCollDone = true
	return nil
}

// ensureCollection creates a collection if it does not already exist.
func (vs *VectorStore) ensureCollection(ctx context.Context, name string) error {
	exists, err := vs.client.CollectionExists(ctx, name)
	if err != nil {
		return fmt.Errorf("vectorstore: check collection %s: %w", name, err)
	}
	if exists {
		return nil
	}

	err = vs.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: name,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     vs.activeEmbedder().Dimension(),
			Distance: qdrant.Distance_Cosine,
		}),
	})
	if err != nil {
		return fmt.Errorf("vectorstore: create collection %s: %w", name, err)
	}
	return nil
}

// collectionForSource returns the collection name for a given source type.
func collectionForSource(source string) string {
	switch source {
	case "note":
		return collectionNotes
	case "summary":
		return collectionSummaries
	default:
		return collectionMessages
	}
}

// ToUUID converts an arbitrary string ID to a valid UUID format using FNV-128 hashing.
func ToUUID(id string) string {
	h := fnv.New128a()
	h.Write([]byte(id))
	sum := h.Sum(nil)
	// Set version (4) and variant (RFC 4122).
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// parseAddr splits "host:port" into host and port.
func parseAddr(addr string) (string, int, error) {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid address: %s", addr)
	}
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
		return "", 0, fmt.Errorf("invalid port in address %s: %w", addr, err)
	}
	return parts[0], port, nil
}
