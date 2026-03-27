package skills

import (
	"testing"
)

func TestParseDuckDuckGoHTML_RealisticResults(t *testing.T) {
	html := `
<html>
<body>
<div class="results">
  <div class="result">
    <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2F&rut=abc123">Go Programming Language</a>
    <a class="result__snippet">Go is an <b>open source</b> programming language supported by Google.</a>
  </div>
  <div class="result">
    <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fen.wikipedia.org%2Fwiki%2FGo_(programming_language)&rut=def456">Go (programming language) - Wikipedia</a>
    <a class="result__snippet">Go, also known as Golang, is a statically typed, compiled language.</a>
  </div>
</div>
</body>
</html>`

	results := parseDuckDuckGoHTML(html)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Title != "Go Programming Language" {
		t.Errorf("result[0] title = %q, want %q", results[0].Title, "Go Programming Language")
	}
	if results[0].URL != "https://golang.org/" {
		t.Errorf("result[0] URL = %q, want %q", results[0].URL, "https://golang.org/")
	}
	if results[0].Snippet != "Go is an open source programming language supported by Google." {
		t.Errorf("result[0] snippet = %q, want %q", results[0].Snippet, "Go is an open source programming language supported by Google.")
	}

	if results[1].Title != "Go (programming language) - Wikipedia" {
		t.Errorf("result[1] title = %q, want %q", results[1].Title, "Go (programming language) - Wikipedia")
	}
	if results[1].URL != "https://en.wikipedia.org/wiki/Go_(programming_language)" {
		t.Errorf("result[1] URL = %q, want %q", results[1].URL, "https://en.wikipedia.org/wiki/Go_(programming_language)")
	}
}

func TestParseDuckDuckGoHTML_NoResults(t *testing.T) {
	html := `<html><body><div class="no-results">No results</div></body></html>`

	results := parseDuckDuckGoHTML(html)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestParseDuckDuckGoHTML_EmptyString(t *testing.T) {
	results := parseDuckDuckGoHTML("")
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestParseDuckDuckGoHTML_MissingSnippet(t *testing.T) {
	html := `<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com">Example</a>`

	results := parseDuckDuckGoHTML(html)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Snippet != "" {
		t.Errorf("expected empty snippet, got %q", results[0].Snippet)
	}
}

func TestExtractHref_Present(t *testing.T) {
	s := `class="result__a" href="https://example.com/page?q=1">Link</a>`
	got := extractHref(s)
	if got != "https://example.com/page?q=1" {
		t.Errorf("extractHref = %q, want %q", got, "https://example.com/page?q=1")
	}
}

func TestExtractHref_Missing(t *testing.T) {
	s := `class="result__a">Link</a>`
	got := extractHref(s)
	if got != "" {
		t.Errorf("extractHref = %q, want empty string", got)
	}
}

func TestExtractHref_NoClosingQuote(t *testing.T) {
	s := `href="https://example.com`
	got := extractHref(s)
	if got != "" {
		t.Errorf("extractHref = %q, want empty string", got)
	}
}

func TestExtractTagText_MatchingTag(t *testing.T) {
	s := `class="result__a" href="...">Hello World</a>`
	got := extractTagText(s, "a")
	if got != "Hello World" {
		t.Errorf("extractTagText = %q, want %q", got, "Hello World")
	}
}

func TestExtractTagText_NestedHTML(t *testing.T) {
	s := `class="result__snippet">Go is an <b>open source</b> language</a>`
	got := extractTagText(s, "a")
	if got != "Go is an <b>open source</b> language" {
		t.Errorf("extractTagText = %q, want %q", got, "Go is an <b>open source</b> language")
	}
}

func TestExtractTagText_NoOpeningAngle(t *testing.T) {
	s := `just plain text without angle brackets`
	got := extractTagText(s, "a")
	if got != "" {
		t.Errorf("extractTagText = %q, want empty string", got)
	}
}

func TestExtractTagText_NoCloseTag(t *testing.T) {
	s := `class="result__a">Hello World`
	got := extractTagText(s, "a")
	if got != "" {
		t.Errorf("extractTagText = %q, want empty string", got)
	}
}

func TestExtractTagText_DivTag(t *testing.T) {
	s := `class="info">content here</div>`
	got := extractTagText(s, "div")
	if got != "content here" {
		t.Errorf("extractTagText = %q, want %q", got, "content here")
	}
}

func TestExtractDDGRedirectURL_WithRedirect(t *testing.T) {
	rawURL := "//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpath&rut=abc123"
	got := extractDDGRedirectURL(rawURL)
	if got != "https://example.com/path" {
		t.Errorf("extractDDGRedirectURL = %q, want %q", got, "https://example.com/path")
	}
}

func TestExtractDDGRedirectURL_WithRedirectNoTrailingParams(t *testing.T) {
	rawURL := "//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com"
	got := extractDDGRedirectURL(rawURL)
	if got != "https://example.com" {
		t.Errorf("extractDDGRedirectURL = %q, want %q", got, "https://example.com")
	}
}

func TestExtractDDGRedirectURL_PlainURL(t *testing.T) {
	rawURL := "https://example.com/page"
	got := extractDDGRedirectURL(rawURL)
	if got != "https://example.com/page" {
		t.Errorf("extractDDGRedirectURL = %q, want %q", got, "https://example.com/page")
	}
}

func TestExtractDDGRedirectURL_DoubleSlashPrefix(t *testing.T) {
	rawURL := "//example.com/path"
	got := extractDDGRedirectURL(rawURL)
	if got != "https://example.com/path" {
		t.Errorf("extractDDGRedirectURL = %q, want %q", got, "https://example.com/path")
	}
}

func TestExtractDDGRedirectURL_InvalidEncoding(t *testing.T) {
	// %ZZ is not a valid percent-encoding; should fall back to returning as-is.
	rawURL := "//duckduckgo.com/l/?uddg=%ZZ"
	got := extractDDGRedirectURL(rawURL)
	// The uddg value fails to decode, so it falls through to the // prefix branch.
	if got != "https://duckduckgo.com/l/?uddg=%ZZ" {
		t.Errorf("extractDDGRedirectURL = %q, want %q", got, "https://duckduckgo.com/l/?uddg=%ZZ")
	}
}

func TestCleanHTMLText_StripsTags(t *testing.T) {
	got := cleanHTMLText("Go is an <b>open source</b> language")
	if got != "Go is an open source language" {
		t.Errorf("cleanHTMLText = %q, want %q", got, "Go is an open source language")
	}
}

func TestCleanHTMLText_NormalizesWhitespace(t *testing.T) {
	got := cleanHTMLText("  hello   world  \n  foo  ")
	if got != "hello world foo" {
		t.Errorf("cleanHTMLText = %q, want %q", got, "hello world foo")
	}
}

func TestCleanHTMLText_EmptyString(t *testing.T) {
	got := cleanHTMLText("")
	if got != "" {
		t.Errorf("cleanHTMLText = %q, want empty string", got)
	}
}

func TestCleanHTMLText_OnlyTags(t *testing.T) {
	got := cleanHTMLText("<br/><hr/>")
	if got != "" {
		t.Errorf("cleanHTMLText = %q, want empty string", got)
	}
}

func TestCleanHTMLText_NestedTags(t *testing.T) {
	got := cleanHTMLText("<div><span>nested</span> text</div>")
	if got != "nested text" {
		t.Errorf("cleanHTMLText = %q, want %q", got, "nested text")
	}
}

func TestCleanHTMLText_PlainText(t *testing.T) {
	got := cleanHTMLText("no tags here")
	if got != "no tags here" {
		t.Errorf("cleanHTMLText = %q, want %q", got, "no tags here")
	}
}
