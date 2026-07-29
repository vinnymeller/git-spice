package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type scratchTokens struct{}

func (scratchTokens) Token(context.Context) (string, error) { return "test-token", nil }

// Proves the URL a stack request actually puts on the wire.
//
// The first real run against GitHub failed because the query string was passed
// inside the path, and url.JoinPath escaped "?" into "%3F". GitHub answered the
// resulting path with 404, which the forge layer reported as "stacks are not
// enabled for this repository".
func TestScratchRequestURLs(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	gw, err := NewGateway(srv.URL, srv.Client(), scratchTokens{})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	if _, err := gw.ListStacks(t.Context(), "camoag", "api"); err != nil {
		t.Fatalf("list stacks: %v", err)
	}
	if _, err := gw.FindStackForPullRequest(t.Context(), "camoag", "api", 10460); err != nil {
		t.Fatalf("find stack: %v", err)
	}

	for _, u := range got {
		t.Logf("request URI: %s", u)
	}

	want := []string{
		"/repos/camoag/api/stacks?page=1&per_page=100",
		"/repos/camoag/api/stacks?pull_request=10460",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d =\n  %q\nwant\n  %q", i, got[i], want[i])
		}
	}
}

// A 404 must reach the caller as a classifiable error, not a decode failure.
func TestScratchNotFoundClassifies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	gw, err := NewGateway(srv.URL, srv.Client(), scratchTokens{})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	_, err = gw.ListStacks(t.Context(), "camoag", "api")
	if err == nil {
		t.Fatal("expected an error")
	}
	t.Logf("error: %v", err)

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %T", err)
	}
	if httpErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", httpErr.StatusCode)
	}
	if httpErr.Message != "Not Found" {
		t.Errorf("message = %q", httpErr.Message)
	}
}

// A path carrying its own query string is a programming error, and must be
// rejected rather than silently producing a 404.
func TestScratchRejectsQueryInPath(t *testing.T) {
	gw, err := NewGateway("https://api.github.com", http.DefaultClient, scratchTokens{})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	_, err = gw.restRequest(t.Context(), http.MethodGet,
		"repos/o/r/stacks?per_page=100", nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error for a query string inside the path")
	}
	t.Logf("error: %v", err)
}
