package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Internet access for the agent. Two tools:
//   - web_fetch(url): download a page and return it as plain text (no key).
//   - web_search(query): query the Brave Search API (needs brave_api_key).
//
// Both go through a single dedicated HTTP client with a modest timeout so a
// slow or hostile server can't stall the agent loop. Neither tool touches the
// filesystem or runs commands — they only make outbound GET requests.

// webClient is the HTTP client shared by web_fetch and web_search. Its timeout
// is deliberately short: a web lookup is an assist, not a long job.
var webClient = &http.Client{Timeout: 30 * time.Second}

// braveSearchURL is the Brave Search API endpoint. It is a var (not a const)
// only so tests can point it at an httptest server.
var braveSearchURL = "https://api.search.brave.com/res/v1/web/search"

const (
	// webFetchMaxBytes caps how much of a page we download before giving up,
	// so a huge response can't exhaust memory.
	webFetchMaxBytes = 2 << 20 // 2 MiB
	// webTextMaxChars caps the text handed back to the model, keeping a single
	// fetch from blowing the context/history budget.
	webTextMaxChars = 20000
)

var (
	scriptRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRe  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]+>`)
	wsRe     = regexp.MustCompile(`[ \t\r\f\v]*\n\s*`)
	spaceRe  = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText reduces an HTML document to readable plain text: it drops script
// and style blocks, strips the remaining tags, unescapes HTML entities, and
// collapses runs of whitespace. It is intentionally simple (RE2, no external
// parser) — good enough to let the model read a page, not to render it.
func htmlToText(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = styleRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ") // non-breaking space → normal space
	s = wsRe.ReplaceAllString(s, "\n")
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// webFetch downloads a single http(s) URL and returns it as plain text. Only
// http and https are accepted so the tool can't be pointed at file:// or other
// schemes.
func webFetch(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("only http and https urls are supported")
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	// A browser-ish UA and Accept help avoid trivial bot walls.
	req.Header.Set("User-Agent", "kami-gateway/1.0 (+https://github.com/JiaJasonLiu/kami)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.8")
	resp, err := webClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch failed: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes))
	if err != nil {
		return "", err
	}
	ct := resp.Header.Get("Content-Type")
	text := string(body)
	if strings.Contains(ct, "html") || strings.Contains(text, "<html") || strings.Contains(text, "<body") {
		text = htmlToText(text)
	} else {
		text = strings.TrimSpace(text)
	}
	if text == "" {
		return "(the page had no readable text)", nil
	}
	return truncate(text, webTextMaxChars), nil
}

// braveResult mirrors the fields of a Brave Search web result we care about.
type braveResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// braveResponse is the slice of the Brave Search JSON response we read.
type braveResponse struct {
	Web struct {
		Results []braveResult `json:"results"`
	} `json:"web"`
}

// webSearch queries the Brave Search API and returns a compact, numbered list
// of results (title, url, snippet). It requires cfg.BraveAPIKey; without one it
// returns an actionable error telling the owner how to configure it.
func webSearch(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("missing search query")
	}
	if cfg.BraveAPIKey == "" {
		return "", errors.New("web search is not configured: set brave_api_key with set_config (free key at https://brave.com/search/api/)")
	}
	u := fmt.Sprintf("%s?q=%s&count=5", braveSearchURL, url.QueryEscape(query))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", cfg.BraveAPIKey)
	resp, err := webClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("brave search error: %s", resp.Status)
	}
	var br braveResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, webFetchMaxBytes)).Decode(&br); err != nil {
		return "", fmt.Errorf("could not parse search response: %v", err)
	}
	if len(br.Web.Results) == 0 {
		return "no results", nil
	}
	var b strings.Builder
	for i, r := range br.Web.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, htmlToText(r.Title), r.URL)
		if d := htmlToText(r.Description); d != "" {
			fmt.Fprintf(&b, "   %s\n", d)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// tWebFetch is the tool handler for web_fetch.
func tWebFetch(args map[string]interface{}) (string, error) {
	rawURL, err := argStr(args, "url")
	if err != nil {
		return "", err
	}
	return webFetch(rawURL)
}

// tWebSearch is the tool handler for web_search.
func tWebSearch(args map[string]interface{}) (string, error) {
	query, err := argStr(args, "query")
	if err != nil {
		return "", err
	}
	return webSearch(query)
}
