package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestJIRAFetcherSeparatesHeadingsFromBodyText covers blocks that hold their
// text directly: without a break a heading runs into the next paragraph.
func TestJIRAFetcherSeparatesHeadingsFromBodyText(t *testing.T) {
	adf := `{"fields":{"summary":"T","description":{"type":"doc","content":[
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Steps"}]},
		{"type":"paragraph","content":[{"type":"text","text":"do the thing"}]}]}}}`
	srv := jiraServer(t, adf)
	defer srv.Close()

	f := &JIRAFetcher{BaseURL: srv.URL, Token: "tok"}
	got, err := f.Fetch(context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if strings.Contains(got.Body, "Stepsdo the thing") {
		t.Errorf("heading ran into the body text: %q", got.Body)
	}
	if !strings.Contains(got.Body, "Steps") || !strings.Contains(got.Body, "do the thing") {
		t.Errorf("Body = %q, want both the heading and the paragraph", got.Body)
	}
}

// TestJIRAFetcherRejectsAnOversizedResponse pins the reason a huge response is
// refused. Reading exactly the cap would truncate the body and surface as
// "unexpected end of JSON input", which blames the server for malformed output
// instead of naming the size.
func TestJIRAFetcherRejectsAnOversizedResponse(t *testing.T) {
	huge := strings.Repeat("x", 2<<20)
	body := fmt.Sprintf(`{"fields":{"summary":"T","description":%q}}`, huge)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	_, err := (&JIRAFetcher{BaseURL: srv.URL, Token: "tok"}).Fetch(
		context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err == nil {
		t.Fatal("Fetch() on an oversized response = nil, want an error")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxJIRABody)) {
		t.Errorf("error = %q, want it to name the limit (%d bytes)", err, maxJIRABody)
	}
	if strings.Contains(err.Error(), "JSON") {
		t.Errorf("error = %q, want the size named rather than a parse failure", err)
	}
}

// TestJIRAFetcherAcceptsAResponseAtTheLimit checks the extra byte read does not
// reject a response that merely reaches the cap.
func TestJIRAFetcherAcceptsAResponseAtTheLimit(t *testing.T) {
	prefix := `{"fields":{"summary":"T","description":"`
	suffix := `"}}`
	fill := strings.Repeat("x", maxJIRABody-len(prefix)-len(suffix))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, prefix+fill+suffix)
	}))
	defer srv.Close()

	got, err := (&JIRAFetcher{BaseURL: srv.URL, Token: "tok"}).Fetch(
		context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err != nil {
		t.Fatalf("Fetch() on a response exactly at the cap = %v, want nil", err)
	}
	if got.Title != "T" {
		t.Errorf("Title = %q, want T", got.Title)
	}
}

// TestJIRAFetcherReportsTheStatusOfALargeErrorPage keeps the size guard from
// masking an HTTP failure. An SSO portal answering 401 with a big HTML page is
// a routine way for this to break, and the status is what explains it.
func TestJIRAFetcherReportsTheStatusOfALargeErrorPage(t *testing.T) {
	page := "<html><body>" + strings.Repeat("Sign in to continue. ", 100000) + "</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	_, err := (&JIRAFetcher{BaseURL: srv.URL, Token: "tok"}).Fetch(
		context.Background(), Ref{Kind: KindJIRA, Key: "PROJ-1"})
	if err == nil {
		t.Fatal("Fetch() on a 401 = nil, want an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want it to report the status", err)
	}
	if strings.Contains(err.Error(), "more than") {
		t.Errorf("error = %q, want the status rather than a complaint about size", err)
	}
	// The page itself must not be dumped into the message.
	if len(err.Error()) > 600 {
		t.Errorf("error is %d chars; the body should be trimmed", len(err.Error()))
	}
}
