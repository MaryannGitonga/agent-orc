package forge

import (
	"errors"
	"strings"
	"testing"
)

// fakeRunner records the commands it is asked to run and replays canned
// results, so the push-and-open sequence can be checked without a remote.
type fakeRunner struct {
	calls   [][]string
	results map[string]string
	errs    map[string]error
}

func (f *fakeRunner) Run(_ string, argv ...string) (string, error) {
	f.calls = append(f.calls, argv)
	key := strings.Join(argv[:min(3, len(argv))], " ")
	return f.results[key], f.errs[key]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (f *fakeRunner) called(sub string) []string {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return c
		}
	}
	return nil
}

func request() Request {
	return Request{
		Worktree: "/wt", Branch: "feat/x", BaseBranch: "main",
		Title: "PROJ-1: do the thing", Body: "body", Remote: "origin",
	}
}

func TestDetect(t *testing.T) {
	tests := map[string]Kind{
		"https://github.com/org/repo.git":     KindGitHub,
		"git@github.com:org/repo.git":         KindGitHub,
		"https://gitlab.com/org/repo.git":     KindGitLab,
		"git@gitlab.example.com:org/repo.git": KindGitLab,
		"https://git.sr.ht/~org/repo":         KindUnknown,
	}
	for url, want := range tests {
		if got := Detect(url); got != want {
			t.Errorf("Detect(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestOpenDraftUsesGhForGitHub(t *testing.T) {
	r := &fakeRunner{results: map[string]string{
		"gh pr create": "https://github.com/org/repo/pull/7",
	}}
	got, err := (&Opener{Runner: r}).OpenDraft(KindGitHub, request())
	if err != nil {
		t.Fatalf("OpenDraft() = %v", err)
	}
	if got != "https://github.com/org/repo/pull/7" {
		t.Errorf("OpenDraft() = %q, want the PR URL", got)
	}

	argv := strings.Join(r.called("gh pr create"), " ")
	for _, want := range []string{"--draft", "--base main", "--head feat/x"} {
		if !strings.Contains(argv, want) {
			t.Errorf("gh command %q is missing %q", argv, want)
		}
	}
}

func TestOpenDraftUsesGlabForGitLab(t *testing.T) {
	r := &fakeRunner{results: map[string]string{"glab mr create": "https://gitlab.com/org/repo/-/merge_requests/3"}}
	got, err := (&Opener{Runner: r}).OpenDraft(KindGitLab, request())
	if err != nil {
		t.Fatalf("OpenDraft() = %v", err)
	}
	if !strings.Contains(got, "merge_requests/3") {
		t.Errorf("OpenDraft() = %q, want the MR URL", got)
	}
	if argv := strings.Join(r.called("glab mr create"), " "); !strings.Contains(argv, "--draft") {
		t.Errorf("glab command %q is missing --draft", argv)
	}
}

func TestOpenDraftOnAnUnknownHost(t *testing.T) {
	_, err := (&Opener{Runner: &fakeRunner{}}).OpenDraft(KindUnknown, request())
	if err == nil {
		t.Fatal("OpenDraft() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "by hand") {
		t.Errorf("error = %q, want it to say what the user should do instead", err)
	}
}

func TestRemoteBranchExists(t *testing.T) {
	present := &fakeRunner{results: map[string]string{
		"git ls-remote --heads": "abc123\trefs/heads/feat/x",
	}}
	got, err := (&Opener{Runner: present}).RemoteBranchExists("/wt", "origin", "feat/x")
	if err != nil {
		t.Fatalf("RemoteBranchExists() = %v", err)
	}
	if !got {
		t.Error("RemoteBranchExists() = false, want true when the ref is listed")
	}

	absent := &fakeRunner{results: map[string]string{"git ls-remote --heads": ""}}
	got, err = (&Opener{Runner: absent}).RemoteBranchExists("/wt", "origin", "feat/x")
	if err != nil {
		t.Fatalf("RemoteBranchExists() = %v", err)
	}
	if got {
		t.Error("RemoteBranchExists() = true, want false for an empty listing")
	}
}

func TestPushReportsGitOutput(t *testing.T) {
	r := &fakeRunner{
		results: map[string]string{"git push -u": "remote rejected: protected branch"},
		errs:    map[string]error{"git push -u": errors.New("exit 1")},
	}
	err := (&Opener{Runner: r}).Push(request())
	if err == nil {
		t.Fatal("Push() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "protected branch") {
		t.Errorf("error = %q, want git's own message included", err)
	}
}

func TestLastURLPicksTheURLOutOfNoisyOutput(t *testing.T) {
	out := "Warning: 3 uncommitted changes\nhttps://github.com/org/repo/pull/7\n"
	if got := lastURL(out); got != "https://github.com/org/repo/pull/7" {
		t.Errorf("lastURL() = %q, want the URL", got)
	}
	if got := lastURL("no url here"); got != "no url here" {
		t.Errorf("lastURL() = %q, want the raw output when there is no URL", got)
	}
}

// TestHasRemote separates "there is no remote" from "git failed", because the
// publisher treats the first as legitimate local-only work and the second as a
// failure that must not be reported as success.
func TestHasRemote(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		r := &fakeRunner{results: map[string]string{"git remote": "origin\nupstream\n"}}
		got, err := (&Opener{Runner: r}).HasRemote("/wt", "origin")
		if err != nil || !got {
			t.Errorf("HasRemote() = %v, %v; want true, nil", got, err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		r := &fakeRunner{results: map[string]string{"git remote": "upstream\n"}}
		got, err := (&Opener{Runner: r}).HasRemote("/wt", "origin")
		if err != nil || got {
			t.Errorf("HasRemote() = %v, %v; want false, nil", got, err)
		}
	})
	t.Run("no remotes at all", func(t *testing.T) {
		r := &fakeRunner{results: map[string]string{"git remote": ""}}
		got, err := (&Opener{Runner: r}).HasRemote("/wt", "origin")
		if err != nil || got {
			t.Errorf("HasRemote() = %v, %v; want false, nil", got, err)
		}
	})
	t.Run("git failed", func(t *testing.T) {
		r := &fakeRunner{
			results: map[string]string{"git remote": "fatal: not a git repository"},
			errs:    map[string]error{"git remote": errors.New("exit status 128")},
		}
		got, err := (&Opener{Runner: r}).HasRemote("/wt", "origin")
		if err == nil {
			t.Error("HasRemote() = nil error on a git failure; it must not read as an absent remote")
		}
		if got {
			t.Error("HasRemote() = true on a git failure")
		}
	})
}
