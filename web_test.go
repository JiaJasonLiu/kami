package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	in := `<html><head><style>.a{color:red}</style><script>alert(1)</script></head>` +
		`<body><h1>Title</h1><p>Hello&nbsp;&amp; welcome</p></body></html>`
	got := htmlToText(in)
	if strings.Contains(got, "alert") || strings.Contains(got, "color:red") {
		t.Errorf("script/style leaked into text: %q", got)
	}
	if !strings.Contains(got, "Title") || !strings.Contains(got, "Hello & welcome") {
		t.Errorf("expected readable text, got %q", got)
	}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>up to date</p></body></html>"))
	}))
	defer srv.Close()

	got, err := webFetch(srv.URL)
	if err != nil {
		t.Fatalf("webFetch: %v", err)
	}
	if !strings.Contains(got, "up to date") {
		t.Errorf("unexpected page text: %q", got)
	}

	// Non-http schemes are rejected.
	if _, err := webFetch("file:///etc/passwd"); err == nil {
		t.Error("expected file:// scheme to be rejected")
	}
}

func TestWebSearchNeedsKey(t *testing.T) {
	cfg = Config{}
	if _, err := webSearch("anything"); err == nil {
		t.Error("expected error when brave_api_key is unset")
	}
}

func TestWebSearchBrave(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"Go <strong>1.23</strong>","url":"https://go.dev","description":"Latest &amp; greatest"}
		]}}`))
	}))
	defer srv.Close()

	cfg = Config{BraveAPIKey: "secret"}
	braveSearchURL = srv.URL

	out, err := webSearch("golang release")
	if err != nil {
		t.Fatalf("webSearch: %v", err)
	}
	if gotKey != "secret" {
		t.Errorf("expected key forwarded, got %q", gotKey)
	}
	if gotQuery != "golang release" {
		t.Errorf("expected query forwarded, got %q", gotQuery)
	}
	if !strings.Contains(out, "Go 1.23") || !strings.Contains(out, "https://go.dev") || !strings.Contains(out, "Latest & greatest") {
		t.Errorf("unexpected search output: %q", out)
	}
	if strings.Contains(out, "<strong>") {
		t.Errorf("html not stripped from results: %q", out)
	}
}
