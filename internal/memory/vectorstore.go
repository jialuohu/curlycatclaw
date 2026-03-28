package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"
)

const (
	collectionMessages  = "curlycatclaw_messages"
	collectionNotes     = "curlycatclaw_notes"
	collectionSummaries = "curlycatclaw_summaries"
)

// SearchResult holds a single vector search result.
type SearchResult struct {
	ID        string
	Text      string
	Source    string
	Score     float32
	CreatedAt string
}

// VectorStore provides vector search backed by Qdrant.
type VectorStore struct {
	client   *qdrant.Client
	embedder Embedder
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

	return vs, nil
}

// Index upserts a text document into the appropriate collection.
// source must be "message" or "note".
func (vs *VectorStore) Index(ctx context.Context, id string, text string, userID int64, chatID int64, source string) error {
	collection := collectionForSource(source)
	vec, err := vs.embedder.Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("vectorstore: embed: %w", err)
	}

	payload := qdrant.NewValueMap(map[string]any{
		"user_id":    userID,
		"chat_id":    chatID,
		"text":       text,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})

	_, err = vs.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: collection,
		Points: []*qdrant.PointStruct{
			{
				Id:      qdrant.NewID(toUUID(id)),
				Vectors: qdrant.NewVectorsDense(vec),
				Payload: payload,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("vectorstore: upsert: %w", err)
	}
	return nil
}

// Search queries both collections for documents matching the query,
// filtered by userID, and returns the top limit results.
func (vs *VectorStore) Search(ctx context.Context, query string, userID int64, limit int) ([]SearchResult, error) {
	vec, err := vs.embedder.Embed(ctx, query)
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

// SearchSummaries queries the summaries collection for documents matching the query,
// filtered by (userID, chatID), returning up to limit results above the score threshold.
func (vs *VectorStore) SearchSummaries(ctx context.Context, query string, userID, chatID int64, limit int, scoreThreshold float32) ([]SearchResult, error) {
	vec, err := vs.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("vectorstore: embed query: %w", err)
	}
	if limit <= 0 {
		limit = 3
	}
	queryLimit := uint64(limit)

	filter := &qdrant.Filter{
		Must: []*qdrant.Condition{
			qdrant.NewMatchInt("user_id", userID),
			qdrant.NewMatchInt("chat_id", chatID),
		},
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
		results = append(results, r)
	}

	return results, nil
}

// Close tears down the gRPC connection.
func (vs *VectorStore) Close() error {
	return vs.client.Close()
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
			Size:     vs.embedder.Dimension(),
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

// toUUID converts an arbitrary string ID to a valid UUID format using FNV-128 hashing.
func toUUID(id string) string {
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
