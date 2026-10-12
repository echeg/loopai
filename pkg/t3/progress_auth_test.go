package t3

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveProgressEndpoint(t *testing.T) {
	t.Run("explicit token requires no CLI", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		ep, err := ResolveProgressEndpoint(t.Context(), envMap(map[string]string{
			EnvToken: " explicit-token ", EnvURL: "https://remote.example/", envT3Home: t.TempDir(),
		}))
		require.NoError(t, err)
		assert.Equal(t, Endpoint{BaseURL: "https://remote.example", Token: "explicit-token"}, ep)
	})

	for _, bin := range []string{"t3", "npx"} {
		t.Run(bin, func(t *testing.T) {
			dir := settleTestPath(t)
			home := t.TempDir()
			writeRuntime(t, home, `{"version":1,"origin":"http://127.0.0.1:3773"}`)
			writeSettleCLI(t, dir, bin, `printf '%s\n' "$@" > "$T3CODE_HOME/issued-args"
printf 'progress-test-token\n'
`)
			ep, err := ResolveProgressEndpoint(t.Context(), envMap(map[string]string{envT3Home: home}))
			require.NoError(t, err)
			assert.Equal(t, Endpoint{BaseURL: "http://127.0.0.1:3773", Token: "progress-test-token"}, ep)
			args, err := os.ReadFile(filepath.Join(home, "issued-args")) //nolint:gosec // test-owned directory
			require.NoError(t, err)
			want := []string{"auth", "session", "issue", "--token-only", "--ttl", "7d", "--label", "loopai-progress"}
			if bin == "npx" {
				want = append([]string{"--yes", "t3@latest"}, want...)
			}
			assert.Equal(t, want, strings.Split(strings.TrimSpace(string(args)), "\n"))
		})
	}

	t.Run("no token for a different server", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		home := t.TempDir()
		writeRuntime(t, home, `{"version":1,"origin":"http://127.0.0.1:3773"}`)
		_, err := ResolveProgressEndpoint(t.Context(), envMap(map[string]string{
			envT3Home: home, EnvURL: "https://remote.example",
		}))
		require.ErrorIs(t, err, ErrNoToken)
	})

	t.Run("credential failure does not expose output", func(t *testing.T) {
		dir := settleTestPath(t)
		home := t.TempDir()
		writeRuntime(t, home, `{"version":1,"origin":"http://127.0.0.1:3773"}`)
		writeSettleCLI(t, dir, "t3", "printf 'secret-token'; printf 'secret-error' >&2; exit 7\n")
		ep, err := ResolveProgressEndpoint(t.Context(), envMap(map[string]string{envT3Home: home}))
		require.ErrorContains(t, err, "issue progress token")
		assert.Empty(t, ep)
		assert.NotContains(t, err.Error(), "secret-token")
		assert.NotContains(t, err.Error(), "secret-error")
	})
}
