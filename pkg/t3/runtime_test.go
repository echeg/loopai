package t3

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writeRuntime(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, "userdata")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server-runtime.json"), []byte(content), 0o600))
}

func TestHomeDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	t.Run("default", func(t *testing.T) {
		home, err := HomeDir(envMap(nil))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(fakeHome, ".t3"), home)
	})
	t.Run("explicit", func(t *testing.T) {
		dir := t.TempDir()
		home, err := HomeDir(envMap(map[string]string{envT3Home: "  " + dir + "  "}))
		require.NoError(t, err)
		assert.Equal(t, dir, home)
	})
	t.Run("tilde", func(t *testing.T) {
		home, err := HomeDir(envMap(map[string]string{envT3Home: "~/t3-home"}))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(fakeHome, "t3-home"), home)
	})
}

func TestReadRuntime(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Runtime
		wantErr string
	}{
		{
			name:    "valid",
			content: `{"version":1,"pid":42,"host":"0.0.0.0","port":3773,"origin":"http://127.0.0.1:3773","startedAt":"2026-09-24T18:17:29.078Z"}` + "\n",
			want:    Runtime{Version: 1, PID: 42, Port: 3773, Origin: "http://127.0.0.1:3773", StartedAt: "2026-09-24T18:17:29.078Z"},
		},
		{name: "bad json", content: "{", wantErr: "parse runtime file"},
		{name: "bad version", content: `{"version":2,"origin":"http://x"}`, wantErr: "unsupported runtime file version 2"},
		{name: "no origin", content: `{"version":1,"origin":"  "}`, wantErr: "no origin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeRuntime(t, home, tc.content)
			got, err := ReadRuntime(home)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("missing", func(t *testing.T) {
		_, err := ReadRuntime(t.TempDir())
		require.ErrorIs(t, err, ErrNoRuntime)
	})
}

func TestResolveEndpoint(t *testing.T) {
	home := t.TempDir()
	writeRuntime(t, home, `{"version":1,"port":3773,"origin":"http://127.0.0.1:3773/"}`)

	t.Run("runtime origin", func(t *testing.T) {
		ep, err := ResolveEndpoint(envMap(map[string]string{EnvToken: " tok ", envT3Home: home}))
		require.NoError(t, err)
		assert.Equal(t, Endpoint{BaseURL: "http://127.0.0.1:3773", Token: "tok"}, ep)
	})
	t.Run("url override skips runtime file", func(t *testing.T) {
		ep, err := ResolveEndpoint(envMap(map[string]string{
			EnvToken: "tok", EnvURL: "http://host:9/", envT3Home: t.TempDir(),
		}))
		require.NoError(t, err)
		assert.Equal(t, Endpoint{BaseURL: "http://host:9", Token: "tok"}, ep)
	})
	t.Run("missing token", func(t *testing.T) {
		_, err := ResolveEndpoint(envMap(map[string]string{envT3Home: home}))
		require.ErrorIs(t, err, ErrNoToken)
	})
	t.Run("no server", func(t *testing.T) {
		_, err := ResolveEndpoint(envMap(map[string]string{EnvToken: "tok", envT3Home: t.TempDir()}))
		require.ErrorIs(t, err, ErrNoRuntime)
	})
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`C:\Projects\AI\loopai`, `c:\projects\ai\loopai`},
		{`C:/Projects/AI/loopai/`, `c:\projects\ai\loopai`},
		{`  C:\Projects\\ `, `c:\projects`},
		{`C:\`, `c:\`},
		{`C:/`, `c:\`},
		{`C:\\`, `c:\`},
		{`C:`, `c:\`},
		{`\\server\share\dir\`, `\\server\share\dir`},
		{`/home/user/Repo/`, `/home/user/Repo`},
		{`/`, `/`},
		{`//`, `//`},
		{``, ``},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizePath(tc.in))
		})
	}
}
