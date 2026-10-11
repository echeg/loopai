package t3

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSettleEndpointExplicitToken(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ep, err := ResolveSettleEndpoint(t.Context(), envMap(map[string]string{
		EnvToken: " explicit-token ", EnvURL: "https://remote.example/", envT3Home: t.TempDir(),
	}))
	require.NoError(t, err)
	assert.Equal(t, Endpoint{BaseURL: "https://remote.example", Token: "explicit-token"}, ep)
}

func TestResolveSettleEndpointNoRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ResolveSettleEndpoint(t.Context(), envMap(map[string]string{envT3Home: t.TempDir()}))
	require.ErrorIs(t, err, ErrNoRuntime)
}

func TestResolveSettleEndpointRejectsOverride(t *testing.T) {
	home := t.TempDir()
	writeRuntime(t, home, `{"version":1,"origin":"http://127.0.0.1:3773"}`)
	// No executable can run, even when an override points to a different local server.
	t.Setenv("PATH", t.TempDir())
	for _, override := range []string{"https://remote.example", "http://127.0.0.1:9999", "http://127.0.0.1:3773/path"} {
		t.Run(override, func(t *testing.T) {
			_, err := ResolveSettleEndpoint(t.Context(), envMap(map[string]string{envT3Home: home, EnvURL: override}))
			require.ErrorIs(t, err, ErrNoToken)
			require.ErrorContains(t, err, "differs from the local runtime")
		})
	}
}

func TestResolveSettleEndpointIssuesToken(t *testing.T) {
	for _, bin := range []string{"t3", "npx"} {
		t.Run(bin, func(t *testing.T) {
			dir := settleTestPath(t)
			home := t.TempDir()
			writeRuntime(t, home, `{"version":1,"origin":"http://127.0.0.1:3773/"}`)
			t.Setenv(envT3Home, "/wrong-inherited-home")
			t.Setenv(EnvToken, "unchanged-process-token")
			writeSettleCLI(t, dir, bin, `printf '%s\n' "$@" > "$T3CODE_HOME/issued-args"
printf '%s\n' "$T3CODE_HOME" > "$T3CODE_HOME/issued-home"
pwd > "$T3CODE_HOME/issued-cwd"
printf '  issued-test-token\n'
`)
			ep, err := ResolveSettleEndpoint(t.Context(), envMap(map[string]string{
				envT3Home: "  " + home + "  ", EnvURL: " http://127.0.0.1:3773/ ",
			}))
			require.NoError(t, err)
			assert.Equal(t, Endpoint{BaseURL: "http://127.0.0.1:3773", Token: "issued-test-token"}, ep)
			args, err := os.ReadFile(filepath.Join(home, "issued-args")) //nolint:gosec // test-owned temporary directory
			require.NoError(t, err)
			want := []string{"auth", "session", "issue", "--token-only", "--ttl", "1h", "--label", "loopai-settle"}
			if bin == "npx" {
				want = append([]string{"--yes", "t3@latest"}, want...)
			}
			assert.Equal(t, want, strings.Split(strings.TrimSpace(string(args)), "\n"))
			for _, file := range []string{"issued-home", "issued-cwd"} {
				got, readErr := os.ReadFile(filepath.Join(home, file)) //nolint:gosec // test-owned temporary directory
				require.NoError(t, readErr)
				wantHome := home
				if file == "issued-cwd" {
					wantHome, err = filepath.EvalSymlinks(home)
					require.NoError(t, err)
				}
				assert.Equal(t, wantHome, strings.TrimSpace(string(got)))
			}
			assert.Equal(t, "/wrong-inherited-home", os.Getenv(envT3Home))
			assert.Equal(t, "unchanged-process-token", os.Getenv(EnvToken))
		})
	}
}

func TestResolveSettleEndpointDefaultHome(t *testing.T) {
	dir := settleTestPath(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	t.Setenv(envT3Home, "/wrong-inherited-home")
	home := filepath.Join(userHome, ".t3")
	writeRuntime(t, home, `{"version":1,"origin":"http://localhost:3773"}`)
	writeSettleCLI(t, dir, "t3", `printf '%s\n' "$T3CODE_HOME" > "$T3CODE_HOME/issued-home"
printf 'default-home-token\n'
`)
	ep, err := ResolveSettleEndpoint(t.Context(), envMap(nil))
	require.NoError(t, err)
	assert.Equal(t, "default-home-token", ep.Token)
	issuedHome, err := os.ReadFile(filepath.Join(home, "issued-home")) //nolint:gosec // test-owned temporary directory
	require.NoError(t, err)
	assert.Equal(t, home, strings.TrimSpace(string(issuedHome)))
}

func TestResolveSettleEndpointTokenErrors(t *testing.T) {
	for _, tc := range []struct {
		name, script, want string
	}{
		{"failed", "printf 'secret-stdout-token\\n'\nprintf 'secret-stderr-token\\n' >&2\nexit 7\n", "issue settlement token"},
		{"empty", "printf ' \\n'\n", "empty or invalid token"},
		{"multiple lines", "printf 'secret-stdout-token\\nsecret-stderr-token\\n'\n", "empty or invalid token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := settleTestPath(t)
			home := t.TempDir()
			writeRuntime(t, home, `{"version":1,"origin":"http://localhost:3773"}`)
			writeSettleCLI(t, dir, "t3", tc.script)
			writeSettleCLI(t, dir, "npx", `printf 'unexpected fallback' > "$T3CODE_HOME/fallback-ran"`)
			ep, err := ResolveSettleEndpoint(t.Context(), envMap(map[string]string{envT3Home: home}))
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, ep)
			assert.NotContains(t, err.Error(), "secret-stdout-token")
			assert.NotContains(t, err.Error(), "secret-stderr-token")
			_, statErr := os.Stat(filepath.Join(home, "fallback-ran"))
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestResolveSettleEndpointMissingCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	writeRuntime(t, home, `{"version":1,"origin":"http://localhost:3773"}`)
	_, err := ResolveSettleEndpoint(t.Context(), envMap(map[string]string{envT3Home: home}))
	require.ErrorContains(t, err, "install t3 or npx")
}

func TestResolveSettleEndpointCancellation(t *testing.T) {
	dir := settleTestPath(t)
	home := t.TempDir()
	writeRuntime(t, home, `{"version":1,"origin":"http://localhost:3773"}`)
	writeSettleCLI(t, dir, "t3", "exec /bin/sleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	ep, err := ResolveSettleEndpoint(ctx, envMap(map[string]string{envT3Home: home}))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, ep)
	assert.Less(t, time.Since(started), 2*time.Second)
}

// An isolated PATH is essential: tests must never issue real credentials through the installed CLI.
func settleTestPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses a POSIX shell")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	return dir
}

func writeSettleCLI(t *testing.T, dir, name, script string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+script), 0o700)) //nolint:gosec // test fixture must be executable
}
