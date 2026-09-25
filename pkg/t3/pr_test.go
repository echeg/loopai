package t3

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGitHubPullRequestURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    PullRequest
		wantErr string
	}{
		{
			name: "valid",
			in:   " https://github.com/Echeg/Loopai/pull/42\n",
			want: PullRequest{Host: "github.com", Repository: "echeg/loopai", Number: 42, URL: "https://github.com/Echeg/Loopai/pull/42"},
		},
		{
			name: "trailing slash",
			in:   "https://github.com/o/r/pull/7/",
			want: PullRequest{Host: "github.com", Repository: "o/r", Number: 7, URL: "https://github.com/o/r/pull/7/"},
		},
		{name: "other host", in: "https://gitlab.com/o/r/-/merge_requests/1", wantErr: "not a github.com"},
		{name: "issue url", in: "https://github.com/o/r/issues/1", wantErr: "not a pull request url"},
		{name: "extra path", in: "https://github.com/o/r/pull/1/files", wantErr: "not a pull request url"},
		{name: "bad number", in: "https://github.com/o/r/pull/abc", wantErr: "invalid pull request number"},
		{name: "zero", in: "https://github.com/o/r/pull/0", wantErr: "invalid pull request number"},
		{name: "not a url", in: "gh output", wantErr: "unsupported pull request url"},
		{name: "unparsable", in: "https://github.com/%zz", wantErr: "parse pull request url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGitHubPullRequestURL(tc.in)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLinkPullRequest(t *testing.T) {
	archived := "2026-09-01T00:00:00Z"
	shell := Shell{
		Projects: []Project{{ID: "p1", WorkspaceRoot: `C:\Repo`}, {ID: "p2", WorkspaceRoot: `C:\Other`}},
		Threads: []Thread{
			{ID: "match", ProjectID: "p1", Branch: new("feat")},
			{ID: "other-branch", ProjectID: "p1", Branch: new("main")},
			{ID: "no-branch", ProjectID: "p1"},
			{ID: "archived", ProjectID: "p1", Branch: new("feat"), ArchivedAt: &archived},
			{ID: "other-project", ProjectID: "p2", Branch: new("feat")},
			{ID: "already", ProjectID: "p1", Branch: new("feat"),
				PullRequests: []PullRequestLink{{Host: "github.com", Repository: "o/r", Number: 5}}},
		},
	}
	const url = "https://github.com/o/r/pull/5"

	t.Run("links matching threads", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell}
		n, err := LinkPullRequest(context.Background(), api, []string{"/elsewhere", "c:/repo"}, "feat", url)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		require.Len(t, api.commands, 1)
		link, ok := api.commands[0].(*ThreadPullRequestLink)
		require.True(t, ok)
		assert.Equal(t, "match", link.ThreadID)
		assert.Equal(t, "o/r", link.Repository)
		assert.Equal(t, 5, link.Number)
		assert.Equal(t, "manual", link.Source)
	})
	t.Run("invalid url", func(t *testing.T) {
		_, err := LinkPullRequest(context.Background(), &fakeDispatcher{shell: shell}, []string{`C:\Repo`}, "feat", "not a url")
		require.Error(t, err)
	})
	t.Run("no project", func(t *testing.T) {
		_, err := LinkPullRequest(context.Background(), &fakeDispatcher{shell: shell}, []string{"/nope"}, "feat", url)
		require.ErrorContains(t, err, "no T3 Code project for /nope")
	})
	t.Run("shell error", func(t *testing.T) {
		_, err := LinkPullRequest(context.Background(), &fakeDispatcher{shellErr: errors.New("down")}, []string{`C:\Repo`}, "feat", url)
		require.ErrorContains(t, err, "down")
	})
	t.Run("dispatch error", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell, dispErr: errors.New("rejected")}
		n, err := LinkPullRequest(context.Background(), api, []string{`C:\Repo`}, "feat", url)
		require.ErrorContains(t, err, "rejected")
		assert.Zero(t, n)
	})
}
