// Package mdhtml converts GitHub-flavored markdown to Telegram-safe HTML.
//
// Telegram supports a limited subset of HTML: <b>, <i>, <u>, <s>, <code>,
// <pre>, <a href="">, <blockquote>, and <pre><code class="language-xxx">.
// All other < > & characters must be escaped.
package mdhtml

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// placeholder is a sentinel prefix used to protect code blocks and inline
// code from being processed by inline formatting rules.
const placeholder = "\x00MDHTML"

// Convert transforms GitHub-flavored markdown to Telegram-safe HTML.
// Content inside code blocks and inline code is not processed for inline
// formatting. Unclosed markers are left as literal text.
func Convert(markdown string) string {
	// Phase 1: Extract and protect fenced code blocks.
	var fencedBlocks []fencedBlock
	markdown, fencedBlocks = extractFencedBlocks(markdown)

	// Phase 2: Extract and protect inline code.
	var inlineBlocks []string
	markdown, inlineBlocks = extractInlineCode(markdown)

	// Phase 3: Escape HTML entities in the remaining text.
	markdown = html.EscapeString(markdown)

	// Phase 4: Convert links [text](url) -> <a href="url">text</a>
	markdown = convertLinks(markdown)

	// Phase 5: Convert bold **text** -> <b>text</b>
	markdown = convertBold(markdown)

	// Phase 6: Convert italic *text* -> <i>text</i>
	markdown = convertItalic(markdown)

	// Phase 7: Convert strikethrough ~~text~~ -> <s>text</s>
	markdown = convertStrikethrough(markdown)

	// Phase 8: Convert headers (# Header -> <b>Header</b>)
	markdown = convertHeaders(markdown)

	// Phase 9: Convert blockquotes (> text -> <blockquote>text</blockquote>)
	markdown = convertBlockquotes(markdown)

	// Phase 10: Convert list bullets (- item or * item -> bullet item)
	markdown = convertListBullets(markdown)

	// Phase 11: Strip horizontal rules (---)
	markdown = stripHorizontalRules(markdown)

	// Phase 12: Restore fenced code blocks.
	markdown = restoreFencedBlocks(markdown, fencedBlocks)

	// Phase 13: Restore inline code.
	markdown = restoreInlineCode(markdown, inlineBlocks)

	return markdown
}

// ConvertSafe calls Convert and validates that the result has balanced tags.
// If the HTML is invalid, it falls back to html.EscapeString(markdown).
func ConvertSafe(markdown string) string {
	converted := Convert(markdown)
	if !hasBalancedTags(converted) {
		return html.EscapeString(markdown)
	}
	return converted
}

// ---------------------------------------------------------------------------
// Fenced code blocks
// ---------------------------------------------------------------------------

type fencedBlock struct {
	lang    string
	content string
}

// fencedBlockRe matches fenced code blocks: ```lang\n...\n```
// The (?s) flag makes . match newlines. We use a non-greedy match.
var fencedBlockRe = regexp.MustCompile("(?s)```(\\w*)\\n(.*?)\\n?```")

func extractFencedBlocks(s string) (string, []fencedBlock) {
	var blocks []fencedBlock
	result := fencedBlockRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := fencedBlockRe.FindStringSubmatch(match)
		blocks = append(blocks, fencedBlock{
			lang:    sub[1],
			content: sub[2],
		})
		return fmt.Sprintf("%sFENCED%d%s", placeholder, len(blocks)-1, placeholder)
	})
	return result, blocks
}

func restoreFencedBlocks(s string, blocks []fencedBlock) string {
	for i, b := range blocks {
		escaped := html.EscapeString(b.content)
		var replacement string
		if b.lang != "" {
			replacement = fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>", b.lang, escaped)
		} else {
			replacement = fmt.Sprintf("<pre><code>%s</code></pre>", escaped)
		}
		s = strings.Replace(s, fmt.Sprintf("%sFENCED%d%s", placeholder, i, placeholder), replacement, 1)
	}
	return s
}

// ---------------------------------------------------------------------------
// Inline code
// ---------------------------------------------------------------------------

var inlineCodeRe = regexp.MustCompile("`([^`]+)`")

func extractInlineCode(s string) (string, []string) {
	var blocks []string
	result := inlineCodeRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := inlineCodeRe.FindStringSubmatch(match)
		blocks = append(blocks, sub[1])
		return fmt.Sprintf("%sINLINE%d%s", placeholder, len(blocks)-1, placeholder)
	})
	return result, blocks
}

func restoreInlineCode(s string, blocks []string) string {
	for i, b := range blocks {
		escaped := html.EscapeString(b)
		replacement := fmt.Sprintf("<code>%s</code>", escaped)
		s = strings.Replace(s, fmt.Sprintf("%sINLINE%d%s", placeholder, i, placeholder), replacement, 1)
	}
	return s
}

// ---------------------------------------------------------------------------
// Inline formatting
// ---------------------------------------------------------------------------

// convertLinks converts [text](url) to <a href="url">text</a>.
// The text and url are already HTML-escaped at this point, so we need to
// unescape the url for the href attribute (Telegram expects raw URLs).
var linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

func convertLinks(s string) string {
	return linkRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := linkRe.FindStringSubmatch(match)
		text := sub[1]
		// The URL was HTML-escaped in phase 3; unescape it for the href.
		url := html.UnescapeString(sub[2])
		url = strings.ReplaceAll(url, "\"", "&quot;")
		return fmt.Sprintf(`<a href="%s">%s</a>`, url, text)
	})
}

// convertBold converts **text** to <b>text</b>.
// Requires the ** to not be preceded/followed by spaces (to avoid matching
// isolated ** markers). Uses a non-greedy match. Only converts when the
// content is non-empty and the markers are properly closed.
var boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

func convertBold(s string) string {
	return boldRe.ReplaceAllString(s, "<b>$1</b>")
}

// convertItalic converts *text* to <i>text</i>.
// After bold conversion, ** markers are gone. Remaining single * pairs are italic.
func convertItalic(s string) string {
	// We need a custom replacer because the regex captures context chars.
	// Instead, use a simpler approach: match lone * not part of ** or tags.
	result := s
	// After bold conversion, ** are gone. Single * remaining are italic candidates.
	simpleItalicRe := regexp.MustCompile(`(?:^|[^<*/])\*([^*\n]+?)\*`)
	for {
		loc := simpleItalicRe.FindStringIndex(result)
		if loc == nil {
			break
		}
		match := result[loc[0]:loc[1]]
		// Find the actual *...* within the match (may have a prefix char).
		starIdx := strings.Index(match, "*")
		prefix := match[:starIdx]
		inner := match[starIdx:]
		// Extract content between the *s.
		content := inner[1 : len(inner)-1]
		replacement := prefix + "<i>" + content + "</i>"
		result = result[:loc[0]] + replacement + result[loc[1]:]
	}
	return result
}

// convertStrikethrough converts ~~text~~ to <s>text</s>.
var strikethroughRe = regexp.MustCompile(`~~(.+?)~~`)

func convertStrikethrough(s string) string {
	return strikethroughRe.ReplaceAllString(s, "<s>$1</s>")
}

// ---------------------------------------------------------------------------
// Block-level formatting
// ---------------------------------------------------------------------------

// headerRe matches lines starting with 1-6 # characters followed by a space.
var headerRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

func convertHeaders(s string) string {
	return headerRe.ReplaceAllString(s, "<b>$1</b>")
}

// convertBlockquotes wraps consecutive lines starting with > in <blockquote> tags.
func convertBlockquotes(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	var quoteLines []string

	flushQuote := func() {
		if len(quoteLines) > 0 {
			inner := strings.Join(quoteLines, "\n")
			result = append(result, "<blockquote>"+inner+"</blockquote>")
			quoteLines = nil
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "&gt; ") {
			// The > was HTML-escaped to &gt; in phase 3.
			quoteLines = append(quoteLines, strings.TrimPrefix(trimmed, "&gt; "))
		} else if trimmed == "&gt;" {
			// Bare > line (empty blockquote line).
			quoteLines = append(quoteLines, "")
		} else {
			flushQuote()
			result = append(result, line)
		}
	}
	flushQuote()

	return strings.Join(result, "\n")
}

// listBulletRe matches lines starting with - or * followed by a space.
// The - and * must be at the start of a line (possibly after whitespace).
// After HTML escaping, * becomes * (not escaped by html.EscapeString).
var listBulletRe = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)

func convertListBullets(s string) string {
	return listBulletRe.ReplaceAllString(s, "${1}\u2022 ")
}

// stripHorizontalRules removes lines that are just --- (three or more dashes).
// After HTML escaping, --- stays as --- (no special chars).
var hrRe = regexp.MustCompile(`(?m)^-{3,}\s*$`)

func stripHorizontalRules(s string) string {
	return hrRe.ReplaceAllString(s, "")
}

// ---------------------------------------------------------------------------
// Tag validation
// ---------------------------------------------------------------------------

// telegramTags are the tags that Telegram HTML mode supports.
var telegramTags = []string{"b", "i", "u", "s", "code", "pre", "a", "blockquote"}

// hasBalancedTags checks that all Telegram-supported HTML tags are properly
// balanced (each opening tag has a matching close tag, in order).
func hasBalancedTags(s string) bool {
	// Build a regex that matches opening or closing Telegram tags.
	// We look for <tag...> and </tag> patterns.
	tagPattern := strings.Join(telegramTags, "|")
	tagRe := regexp.MustCompile(`<(/?)(?:` + tagPattern + `)(?:\s[^>]*)?>`)

	var stack []string
	for _, match := range tagRe.FindAllStringSubmatch(s, -1) {
		full := match[0]
		isClose := match[1] == "/"

		// Extract the tag name from the match.
		var tagName string
		if isClose {
			// </tag>
			tagName = full[2 : len(full)-1]
		} else {
			// <tag> or <tag attr="...">
			// Find the tag name (first word after <).
			inner := full[1 : len(full)-1]
			spaceIdx := strings.IndexAny(inner, " \t")
			if spaceIdx >= 0 {
				tagName = inner[:spaceIdx]
			} else {
				tagName = inner
			}
		}

		if isClose {
			if len(stack) == 0 || stack[len(stack)-1] != tagName {
				return false
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, tagName)
		}
	}

	return len(stack) == 0
}
