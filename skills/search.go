package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewWebSearchSkill returns a web search skill using DuckDuckGo HTML search.
func NewWebSearchSkill() *Skill {
	return &Skill{
		Name:        "web_search",
		Description: "Search the web for information. Returns top results with titles, URLs, and snippets.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query"}},"required":["query"]}`),
		Execute:     executeWebSearch,
	}
}

type webSearchInput struct {
	Query string `json:"query"`
}

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

func executeWebSearch(ctx context.Context, input json.RawMessage) (string, error) {
	var params webSearchInput
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if params.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(params.Query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "curlycatclaw/1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2MB max
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	results := parseDuckDuckGoHTML(string(body))
	if len(results) == 0 {
		return "No results found for: " + params.Query, nil
	}

	// Limit to top 5.
	if len(results) > 5 {
		results = results[:5]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %s\n\n", params.Query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return sb.String(), nil
}

// parseDuckDuckGoHTML extracts search results from DuckDuckGo's HTML response.
// DuckDuckGo HTML results use:
//
//	<a class="result__a" href="...">title</a>
//	<a class="result__snippet">snippet</a>
func parseDuckDuckGoHTML(html string) []searchResult {
	var results []searchResult

	remaining := html
	for {
		// Find the next result link.
		idx := strings.Index(remaining, `class="result__a"`)
		if idx == -1 {
			break
		}
		remaining = remaining[idx:]

		// Extract href from the result__a tag.
		resultURL := extractHref(remaining)

		// Extract title text from inside the <a> tag.
		title := extractTagText(remaining, "a")

		// Find the snippet for this result.
		snippet := ""
		snippetIdx := strings.Index(remaining, `class="result__snippet"`)
		if snippetIdx != -1 {
			snippet = extractTagText(remaining[snippetIdx:], "a")
		}

		if title != "" && resultURL != "" {
			// DuckDuckGo wraps URLs in a redirect; extract the actual URL.
			actualURL := extractDDGRedirectURL(resultURL)
			results = append(results, searchResult{
				Title:   cleanHTMLText(title),
				URL:     actualURL,
				Snippet: cleanHTMLText(snippet),
			})
		}

		// Advance past this result.
		remaining = remaining[1:]
	}

	return results
}

// extractHref finds href="..." in the string starting near the current position.
func extractHref(s string) string {
	hrefIdx := strings.Index(s, `href="`)
	if hrefIdx == -1 {
		return ""
	}
	start := hrefIdx + len(`href="`)
	end := strings.Index(s[start:], `"`)
	if end == -1 {
		return ""
	}
	return s[start : start+end]
}

// extractTagText extracts the text content between the first > and </tagName>.
func extractTagText(s string, tag string) string {
	openEnd := strings.Index(s, ">")
	if openEnd == -1 {
		return ""
	}
	closeTag := "</" + tag + ">"
	closeIdx := strings.Index(s[openEnd:], closeTag)
	if closeIdx == -1 {
		return ""
	}
	return s[openEnd+1 : openEnd+closeIdx]
}

// extractDDGRedirectURL extracts the actual URL from DuckDuckGo's redirect wrapper.
// DuckDuckGo redirects look like: //duckduckgo.com/l/?uddg=<encoded-url>&...
func extractDDGRedirectURL(rawURL string) string {
	if strings.Contains(rawURL, "uddg=") {
		parts := strings.SplitN(rawURL, "uddg=", 2)
		if len(parts) == 2 {
			encoded := parts[1]
			// Trim any trailing query parameters.
			if ampIdx := strings.Index(encoded, "&"); ampIdx != -1 {
				encoded = encoded[:ampIdx]
			}
			decoded, err := url.QueryUnescape(encoded)
			if err == nil {
				return decoded
			}
		}
	}
	// If not a redirect or parsing fails, return as-is.
	if strings.HasPrefix(rawURL, "//") {
		return "https:" + rawURL
	}
	return rawURL
}

// cleanHTMLText strips HTML tags and cleans up whitespace from text content.
func cleanHTMLText(s string) string {
	// Remove HTML tags.
	var result strings.Builder
	inTag := false
	for _, c := range s {
		switch {
		case c == '<':
			inTag = true
		case c == '>':
			inTag = false
		case !inTag:
			result.WriteRune(c)
		}
	}
	// Normalize whitespace.
	text := result.String()
	text = strings.Join(strings.Fields(text), " ")
	return strings.TrimSpace(text)
}
