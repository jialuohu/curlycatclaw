package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// LLMCaller abstracts the Claude API for read-only evaluation calls.
// This is separate from CronExecutor to ensure no tools are exposed during eval.
type LLMCaller interface {
	EvalCall(ctx context.Context, system string, messages []anthropic.MessageParam) (string, error)
}

// CandidateGenerator proposes memory fixes for failure clusters using Claude.
type CandidateGenerator struct {
	llm            LLMCaller
	maxPerRun      int
}

// NewCandidateGenerator creates a CandidateGenerator.
func NewCandidateGenerator(llm LLMCaller, maxPerRun int) *CandidateGenerator {
	return &CandidateGenerator{llm: llm, maxPerRun: maxPerRun}
}

const candidateSystemPrompt = `You are an AI assistant evaluator. You analyze failure patterns from past conversations and propose specific memory updates that would prevent similar failures.

For each failure pattern, propose a memory update in this JSON format:
{
  "type": "observation",
  "title": "concise title (max 100 chars)",
  "summary": "1-2 sentence description of what to remember",
  "facts": ["atomic fact 1", "atomic fact 2"],
  "confidence": 0.0-1.0,
  "predicted_impact": "how this memory would prevent the failure"
}

Rules:
- Only propose memories that would actually prevent future failures
- Be specific — vague memories are useless
- confidence should reflect how certain you are this memory would help
- If the failure was a one-off (user error, network glitch), set confidence < 0.3
- Output ONLY valid JSON, no markdown, no explanation`

// GenerateCandidates proposes memory updates for a list of failure clusters.
// It calls Claude once per cluster (read-only, no tools).
func (g *CandidateGenerator) GenerateCandidates(ctx context.Context, runID string, clusters []FailureCluster) []MemoryCandidate {
	var candidates []MemoryCandidate

	for i, cluster := range clusters {
		if len(candidates) >= g.maxPerRun {
			slog.Info("eval: candidate cap reached", "cap", g.maxPerRun)
			break
		}

		if ctx.Err() != nil {
			break
		}

		candidate, err := g.generateForCluster(ctx, runID, cluster)
		if err != nil {
			slog.Warn("eval: candidate generation failed", "cluster", cluster.ID, "err", err)
			continue
		}
		if candidate != nil {
			candidates = append(candidates, *candidate)
			slog.Info("eval: candidate generated", "index", i, "title", candidate.Title, "confidence", candidate.Confidence)
		}
	}

	return candidates
}

func (g *CandidateGenerator) generateForCluster(ctx context.Context, runID string, cluster FailureCluster) (*MemoryCandidate, error) {
	userPrompt := fmt.Sprintf(
		"Analyze this failure pattern and propose a memory update.\n\nFailure type: %s\nSeverity: %d/10\nFrequency: %d\nDescription: %s",
		cluster.ClusterType, cluster.Severity, cluster.Frequency, cluster.Description,
	)

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt)),
	}

	resp, err := g.llm.EvalCall(ctx, candidateSystemPrompt, messages)
	if err != nil {
		return nil, fmt.Errorf("llm call: %w", err)
	}

	return parseCandidateResponse(resp, runID, cluster)
}

// candidateJSON is the expected JSON structure from Claude.
type candidateJSON struct {
	Type            string   `json:"type"`
	Title           string   `json:"title"`
	Summary         string   `json:"summary"`
	Facts           []string `json:"facts"`
	Confidence      float64  `json:"confidence"`
	PredictedImpact string   `json:"predicted_impact"`
}

func parseCandidateResponse(resp string, runID string, cluster FailureCluster) (*MemoryCandidate, error) {
	// Strip markdown code fences if present.
	resp = strings.TrimSpace(resp)
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var cj candidateJSON
	if err := json.Unmarshal([]byte(resp), &cj); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if cj.Title == "" || cj.Summary == "" {
		return nil, fmt.Errorf("empty title or summary")
	}

	// Validate candidate type against allowed set.
	switch cj.Type {
	case "observation", "fact", "prompt_note":
		// valid
	default:
		cj.Type = "observation" // default to observation for unrecognized types
	}

	// Clamp confidence.
	if cj.Confidence < 0 {
		cj.Confidence = 0
	}
	if cj.Confidence > 1 {
		cj.Confidence = 1
	}

	content, _ := json.Marshal(map[string]any{
		"summary": cj.Summary,
		"facts":   cj.Facts,
	})
	evidence, _ := json.Marshal(map[string]any{
		"cluster_id":   cluster.ID,
		"cluster_type": cluster.ClusterType,
		"description":  cluster.Description,
	})

	return &MemoryCandidate{
		ID:               newID(),
		EvalRunID:        runID,
		FailureClusterID: cluster.ID,
		CandidateType:    cj.Type,
		Title:            cj.Title,
		Content:          string(content),
		Evidence:         string(evidence),
		Confidence:       cj.Confidence,
		PredictedImpact:  cj.PredictedImpact,
		Status:           "pending",
		CreatedAt:        time.Now().UTC(),
	}, nil
}
