package t3

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// PullRequest identifies a pull request the way T3 Code thread links key it.
type PullRequest struct {
	Host       string
	Repository string
	Number     int
	URL        string
}

// ParseGitHubPullRequestURL parses https://github.com/<owner>/<repo>/pull/<n>, the only URL
// shape loopai's --pr produces.
func ParseGitHubPullRequestURL(raw string) (PullRequest, error) {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil {
		return PullRequest{}, fmt.Errorf("t3: parse pull request url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return PullRequest{}, fmt.Errorf("t3: unsupported pull request url %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" {
		return PullRequest{}, fmt.Errorf("t3: not a github.com pull request url %q", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return PullRequest{}, fmt.Errorf("t3: not a pull request url %q", raw)
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 {
		return PullRequest{}, fmt.Errorf("t3: invalid pull request number in %q", raw)
	}
	return PullRequest{
		Host:       host,
		Repository: strings.ToLower(parts[0] + "/" + parts[1]),
		Number:     number,
		URL:        trimmed,
	}, nil
}

// LinkPullRequest links the pull request at prURL to every non-archived thread on branch in the
// T3 Code project rooted at one of roots (the repository's registered worktrees). Threads that
// already carry the link are skipped because T3 Code rejects duplicates. It returns how many
// threads were linked.
func LinkPullRequest(ctx context.Context, api Dispatcher, roots []string, branch, prURL string) (int, error) {
	pr, err := ParseGitHubPullRequestURL(prURL)
	if err != nil {
		return 0, err
	}
	shell, err := api.Shell(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // Shell already names the failing call
	}
	projects := map[string]bool{}
	for _, root := range roots {
		if project, ok := shell.FindProject(root); ok {
			projects[project.ID] = true
		}
	}
	if len(projects) == 0 {
		return 0, fmt.Errorf("t3: no T3 Code project for %s", strings.Join(roots, ", "))
	}
	linked := 0
	for _, thread := range shell.Threads {
		if !projects[thread.ProjectID] || thread.ArchivedAt != nil || thread.Branch == nil ||
			*thread.Branch != branch || thread.HasPullRequest(pr) {
			continue
		}
		if _, err := api.Dispatch(ctx, NewThreadPullRequestLink(thread.ID, pr)); err != nil {
			return linked, err //nolint:wrapcheck // Dispatch already names the failing command
		}
		linked++
	}
	return linked, nil
}

// HasPullRequest reports whether the thread already links pr, which T3 Code rejects as a
// duplicate.
func (t Thread) HasPullRequest(pr PullRequest) bool {
	for _, link := range t.PullRequests {
		if strings.EqualFold(link.Host, pr.Host) && strings.EqualFold(link.Repository, pr.Repository) &&
			link.Number == pr.Number {
			return true
		}
	}
	return false
}
