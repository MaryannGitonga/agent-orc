package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Fetcher resolves a reference to the issue behind it.
type Fetcher interface {
	Fetch(ctx context.Context, ref Ref) (Issue, error)
}

// Resolver dispatches a reference to the right fetcher.
type Resolver struct {
	GitHub Fetcher
	JIRA   Fetcher
}

// NewResolver returns a resolver using the real GitHub and JIRA fetchers.
func NewResolver() *Resolver {
	return &Resolver{GitHub: &GitHubFetcher{}, JIRA: NewJIRAFetcher()}
}

// Resolve returns the base prompt text for a reference. Free text is returned
// as-is; a ticket reference is fetched.
//
// A failed fetch is a launch-time error, never a silent fallback: launching an
// agent on a guessed or empty prompt is worse than not launching it.
func (r *Resolver) Resolve(ctx context.Context, ref Ref) (string, error) {
	var f Fetcher
	switch ref.Kind {
	case KindText:
		return ref.Raw, nil
	case KindGitHub:
		f = r.GitHub
	case KindJIRA:
		f = r.JIRA
	default:
		return "", fmt.Errorf("unsupported source kind %q", ref.Kind)
	}
	if f == nil {
		return "", fmt.Errorf("no fetcher configured for %s sources", ref.Kind)
	}
	issue, err := f.Fetch(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", ref.Raw, err)
	}
	text := issue.Text()
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("fetching %s: the issue has no title or description", ref.Raw)
	}
	return text, nil
}

// GitHubFetcher reads an issue with the gh CLI, reusing the authentication
// that opening a PR already needs — no second credential to configure.
type GitHubFetcher struct {
	// Bin is the gh executable; empty means "gh" from PATH.
	Bin string
}

// Fetch returns the title and body of a GitHub issue.
func (g *GitHubFetcher) Fetch(ctx context.Context, ref Ref) (Issue, error) {
	bin := g.Bin
	if bin == "" {
		bin = "gh"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return Issue{}, fmt.Errorf("the gh CLI is required to fetch github sources: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin, "issue", "view", strconv.Itoa(ref.Number),
		"--repo", ref.Owner+"/"+ref.Repo, "--json", "title,body")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return Issue{}, fmt.Errorf("gh issue view: %w: %s", err, msg)
		}
		return Issue{}, fmt.Errorf("gh issue view: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(stdout.Bytes(), &issue); err != nil {
		return Issue{}, fmt.Errorf("parsing gh output: %w", err)
	}
	return issue, nil
}

// Environment variables the JIRA fetcher reads. These are ordinary JIRA API
// settings, not anything agent-orc invents.
const (
	EnvJIRABaseURL = "JIRA_BASE_URL"
	EnvJIRAToken   = "JIRA_API_TOKEN"
	EnvJIRAUser    = "JIRA_USER_EMAIL"
	// EnvJIRAAPIVersion overrides the REST API version. v2 is what Server and
	// Data Center use natively and Cloud still accepts; set this to 3 for an
	// instance that requires it.
	EnvJIRAAPIVersion = "JIRA_API_VERSION"
)

// JIRAFetcher reads an issue over the JIRA REST API.
type JIRAFetcher struct {
	Client *http.Client
	// BaseURL and Token override the environment when set; tests use this.
	BaseURL, Token, User, APIVersion string
}

// NewJIRAFetcher returns a fetcher configured from the environment.
func NewJIRAFetcher() *JIRAFetcher {
	return &JIRAFetcher{Client: &http.Client{Timeout: 30 * time.Second}}
}

// Fetch returns the summary and description of a JIRA issue.
func (j *JIRAFetcher) Fetch(ctx context.Context, ref Ref) (Issue, error) {
	base := firstNonEmpty(j.BaseURL, os.Getenv(EnvJIRABaseURL))
	token := firstNonEmpty(j.Token, os.Getenv(EnvJIRAToken))
	if base == "" {
		return Issue{}, fmt.Errorf("%s is not set", EnvJIRABaseURL)
	}
	if token == "" {
		return Issue{}, fmt.Errorf("%s is not set", EnvJIRAToken)
	}
	version := firstNonEmpty(j.APIVersion, os.Getenv(EnvJIRAAPIVersion), "2")

	url := fmt.Sprintf("%s/rest/api/%s/issue/%s", strings.TrimRight(base, "/"), version, ref.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Issue{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Cloud wants basic auth with the account email; Server and Data Center
	// want a bearer token. Which one applies is decided by whether an email
	// is configured, the same way the JIRA docs describe it.
	if user := firstNonEmpty(j.User, os.Getenv(EnvJIRAUser)); user != "" {
		req.SetBasicAuth(user, token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Issue{}, fmt.Errorf("requesting %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Issue{}, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Issue{}, fmt.Errorf("%s returned %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Fields struct {
			Summary     string          `json:"summary"`
			Description json.RawMessage `json:"description"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Issue{}, fmt.Errorf("parsing response: %w", err)
	}
	return Issue{Title: payload.Fields.Summary, Body: decodeDescription(payload.Fields.Description)}, nil
}

// decodeDescription flattens a JIRA description. v2 returns a plain string; v3
// returns an Atlassian Document Format tree, whose text nodes are gathered
// here so both API versions produce a usable prompt.
func decodeDescription(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	var b strings.Builder
	collectText(node, &b)
	return strings.TrimSpace(b.String())
}

// collectText walks an Atlassian Document Format tree gathering its text.
func collectText(node any, b *strings.Builder) {
	switch n := node.(type) {
	case map[string]any:
		if s, ok := n["text"].(string); ok {
			b.WriteString(s)
		}
		if kids, ok := n["content"].([]any); ok {
			collectText(kids, b)
		}
		if n["type"] == "paragraph" {
			b.WriteString("\n\n")
		}
	case []any:
		for _, kid := range n {
			collectText(kid, b)
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
