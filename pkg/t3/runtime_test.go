package t3

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useHome points T3CODE_HOME and the user home at fresh temp directories and clears LOOPAI_T3_URL,
// so no test reads a real T3 Code installation.
func useHome(t *testing.T) string {
	t.Helper()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvURL, "")
	return home
}

func writeRuntimeFile(t *testing.T, home, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "userdata"), 0o750))
	require.NoError(t, os.WriteFile(RuntimePath(home), []byte(content), 0o600))
}

func TestHome(t *testing.T) {
	useHome(t)
	t.Setenv(EnvHome, "  /custom/t3  ")
	home, err := Home()
	require.NoError(t, err)
	assert.Equal(t, "/custom/t3", home, "T3CODE_HOME is trimmed")

	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	t.Setenv(EnvHome, "   ")
	home, err = Home()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(userHome, ".t3"), home, "a blank T3CODE_HOME falls back to ~/.t3")
}

func TestRuntimePath(t *testing.T) {
	home := t.TempDir()
	assert.Equal(t, filepath.Join(home, "userdata", "server-runtime.json"), RuntimePath(home))
}

func TestReadRuntime(t *testing.T) {
	home := useHome(t)
	writeRuntimeFile(t, home, `{"version":1,"pid":4242,"ownerId":"o","host":"127.0.0.1","port":3773,`+
		`"origin":"http://127.0.0.1:3773","startedAt":"2026-10-22T10:00:00.000Z","serverVersion":"0.1.1",`+
		`"serviceManaged":false}`)
	rt, err := ReadRuntime(home)
	require.NoError(t, err)
	assert.Equal(t, Runtime{Origin: "http://127.0.0.1:3773", PID: 4242, StartedAt: "2026-10-22T10:00:00.000Z",
		ServerVersion: "0.1.1"}, rt)
}

func TestReadRuntimeErrors(t *testing.T) {
	home := useHome(t)
	_, err := ReadRuntime(home)
	require.ErrorIs(t, err, ErrNotRunning, "a missing runtime file means T3 Code is not running")
	assert.Contains(t, err.Error(), RuntimePath(home))

	writeRuntimeFile(t, home, `{not json`)
	_, err = ReadRuntime(home)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotRunning)
	assert.Contains(t, err.Error(), "decode")
}

func TestResolveRuntime(t *testing.T) {
	const runtimeJSON = `{"version":1,"pid":7,"port":1,"origin":"http://127.0.0.1:1111","startedAt":"s1"}`
	tests := []struct {
		name     string
		runtime  string
		override string
		want     Runtime
		wantErr  error
		errText  string
	}{
		{name: "runtime file", runtime: runtimeJSON,
			want: Runtime{Origin: "http://127.0.0.1:1111", PID: 7, StartedAt: "s1"}},
		{name: "LOOPAI_T3_URL wins over the runtime origin", runtime: runtimeJSON, override: " http://127.0.0.1:2222 ",
			want: Runtime{Origin: "http://127.0.0.1:2222", PID: 7, StartedAt: "s1"}},
		{name: "LOOPAI_T3_URL without a runtime file", override: "http://127.0.0.1:2222",
			want: Runtime{Origin: "http://127.0.0.1:2222"}},
		{name: "no runtime file", wantErr: ErrNotRunning},
		{name: "runtime without origin", runtime: `{"version":1,"pid":7}`, errText: "has no origin"},
		{name: "LOOPAI_T3_URL ignores a broken runtime file", runtime: `{`, override: "http://x:1",
			want: Runtime{Origin: "http://x:1"}},
		{name: "broken runtime file", runtime: `{`, errText: "decode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := useHome(t)
			if tc.runtime != "" {
				writeRuntimeFile(t, home, tc.runtime)
			}
			t.Setenv(EnvURL, tc.override)
			rt, err := ResolveRuntime()
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			case tc.errText != "":
				require.ErrorContains(t, err, tc.errText)
			default:
				require.NoError(t, err)
				assert.Equal(t, tc.want, rt)
			}
		})
	}
}

func TestWebSocketURL(t *testing.T) {
	tests := []struct {
		origin, want, errText string
	}{
		{origin: "http://127.0.0.1:3773", want: "ws://127.0.0.1:3773/ws?orchestrationProtocol=2"},
		{origin: "https://t3.example:8443/", want: "wss://t3.example:8443/ws?orchestrationProtocol=2"},
		{origin: "HTTP://localhost:1/some/path?x=1#f", want: "ws://localhost:1/ws?orchestrationProtocol=2"},
		{origin: "ws://127.0.0.1:1", want: "ws://127.0.0.1:1/ws?orchestrationProtocol=2"},
		{origin: "wss://h", want: "wss://h/ws?orchestrationProtocol=2"},
		{origin: "http://someone@h:1", want: "ws://h:1/ws?orchestrationProtocol=2"},
		{origin: "ftp://h", errText: "not an http or https URL"},
		{origin: "localhost:3773", errText: "not an http or https URL"},
		{origin: "127.0.0.1:3773", errText: "parse origin"},
		{origin: "http://", errText: "has no host"},
		{origin: "http://[::1", errText: "parse origin"},
	}
	for _, tc := range tests {
		t.Run(tc.origin, func(t *testing.T) {
			got, err := WebSocketURL(tc.origin)
			if tc.errText != "" {
				require.ErrorContains(t, err, tc.errText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHTTPOrigin(t *testing.T) {
	tests := []struct {
		origin, want string
	}{
		{origin: "http://127.0.0.1:3773", want: "http://127.0.0.1:3773"},
		{origin: "https://h/x?y", want: "https://h"},
		{origin: "ws://h:1", want: "http://h:1"},
		{origin: "wss://h:1/ws", want: "https://h:1"},
	}
	for _, tc := range tests {
		got, err := httpOrigin(tc.origin)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	_, err := httpOrigin("file:///x")
	require.Error(t, err)
}
