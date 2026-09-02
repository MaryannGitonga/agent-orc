package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubFetcher returns a fixed issue or error.
type stubFetcher struct {
	issue Issue
	err   error
}

func (s stubFetcher) Fetch(context.Context, Ref) (Issue, error) { return s.issue, s.err }

func TestResolveReturnsFreeTextUnfetched(t *testing.T) {
	r := &Resolver{}
	got, err := r.Resolve(context.Background(), Ref{Kind: KindText, Raw: "do the thing"})
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if got != "do the thing" {
		t.Errorf("Resolve() = %q, want the text unchanged", got)
	}
}

func TestResolveDispatchesToTheRightFetcher(t *testing.T) {
	r := &Resolver{
		GitHub: stubFetcher{issue: Issue{Title: "gh title", Body: "gh body"}},
		JIRA:   stubFetcher{issue: Issue{Title: "jira title"}},
	}
	got, err := r.Resolve(context.Background(), Ref{Kind: KindGitHub})
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if got != "gh title\n\ngh body" {
		t.Errorf("Resolve() = %q, want the GitHub issue", got)
	}
	if got, _ = r.Resolve(context.Background(), Ref{Kind: KindJIRA}); got != "jira title" {
		t.Errorf("Resolve() = %q, want the JIRA issue", got)
	}
}

func TestResolveFailsLoudlyOnAFetchError(t *testing.T) {
	r := &Resolver{GitHub: stubFetcher{err: context.DeadlineExceeded}}
	if _, err := r.Resolve(context.Background(), Ref{Kind: KindGitHub, Raw: "github://o/r#1"}); err == nil {
		t.Error("Resolve() = nil, want the fetch error surfaced rather than a fallback prompt")
	}
}

func TestResolveRejectsAnEmptyIssue(t *testing.T) {
	r := &Resolver{GitHub: stubFetcher{issue: Issue{}}}
	_, err := r.Resolve(context.Background(), Ref{Kind: KindGitHub, Raw: "github://o/r#1"})
	if err == nil || !strings.Contains(err.Error(), "no title or description") {
		t.Errorf("Resolve() = %v, want an error about the empty issue", err)
	}
}

// jiraServer stands in for a JIRA instance, asserting on the request it gets.
func jiraServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/rest/api/2/issue/PROJ-1"; got != want {
			t.Errorf("request path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want a bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestJIRAFetcherReadsSummaryAndPlainDescription(t *testing.T) {
	srv := jiraServer(t, `{"fields":{"summary":"Retry loops","description":"It spins forever."}}`)
	defer srv.Close()

	f := &JIRAFetcher{BaseURL: srv.URL, Token: "tok"}
	got, err := f.Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if got.Title != "Retry loops" || got.Body != "It spins forever." {
		t.Errorf("Fetch() = %+v, want the summary and description", got)
	}
}

func TestJIRAFetcherFlattensADocumentFormatDescription(t *testing.T) {
	// Cloud's v3 API returns Atlassian Document Format rather than a string.
	adf := `{"fields":{"summary":"T","description":{"type":"doc","content":[
		{"type":"paragraph","content":[{"type":"text","text":"first line"}]},
		{"type":"paragraph","content":[{"type":"text","text":"second line"}]}]}}}`
	srv := jiraServer(t, adf)
	defer srv.Close()

	f := &JIRAFetcher{BaseURL: srv.URL, Token: "tok"}
	got, err := f.Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	for _, want := range []string{"first line", "second line"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("Body = %q, want it to contain %q", got.Body, want)
		}
	}
}

func TestJIRAFetcherUsesBasicAuthWhenAnEmailIsSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "me@example.com" || pass != "tok" {
			t.Errorf("basic auth = %q/%q (ok=%v), want me@example.com/tok", user, pass, ok)
		}
		_, _ = w.Write([]byte(`{"fields":{"summary":"T"}}`))
	}))
	defer srv.Close()

	f := &JIRAFetcher{BaseURL: srv.URL, Token: "tok", User: "me@example.com"}
	if _, err := f.Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"}); err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
}

func TestJIRAFetcherSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist"]}`))
	}))
	defer srv.Close()

	f := &JIRAFetcher{BaseURL: srv.URL, Token: "tok"}
	_, err := f.Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err == nil {
		t.Fatal("Fetch() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want the status and the server's message", err)
	}
}

func TestJIRAFetcherRequiresConfiguration(t *testing.T) {
	t.Setenv(EnvJIRABaseURL, "")
	t.Setenv(EnvJIRAToken, "")

	_, err := (&JIRAFetcher{}).Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err == nil || !strings.Contains(err.Error(), EnvJIRABaseURL) {
		t.Errorf("Fetch() = %v, want an error naming %s", err, EnvJIRABaseURL)
	}

	_, err = (&JIRAFetcher{BaseURL: "https://jira.example.com"}).
		Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err == nil || !strings.Contains(err.Error(), EnvJIRAToken) {
		t.Errorf("Fetch() = %v, want an error naming %s", err, EnvJIRAToken)
	}
}

func TestJIRAFetcherHonoursTheAPIVersionOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/rest/api/3/") {
			t.Errorf("path = %q, want the v3 API", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"fields":{"summary":"T"}}`))
	}))
	defer srv.Close()

	f := &JIRAFetcher{BaseURL: srv.URL, Token: "tok", APIVersion: "3"}
	if _, err := f.Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"}); err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
}
