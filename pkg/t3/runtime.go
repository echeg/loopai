package t3

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables that locate the T3 Code server.
const (
	// EnvHome names T3 Code's home directory; it defaults to ~/.t3.
	EnvHome = "T3CODE_HOME"
	// EnvURL overrides the server origin read from server-runtime.json.
	EnvURL = "LOOPAI_T3_URL"
)

// protocolQuery is the WebSocket query parameter carrying the orchestration protocol version; the
// server answers 426 without it, before authentication.
const (
	protocolQueryParam = "orchestrationProtocol"
	protocolVersion    = "2"
)

// ErrNotRunning reports that no T3 Code server runtime file exists, which means the server is not
// running (it removes the file on shutdown).
var ErrNotRunning = errors.New("t3: T3 Code is not running")

// Runtime is the server identity a connection was made against. Origin is the effective HTTP origin,
// LOOPAI_T3_URL included; PID and StartedAt change when the server restarts and are zero when only
// LOOPAI_T3_URL named the server.
type Runtime struct {
	Origin        string `json:"origin"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	ServerVersion string `json:"serverVersion"`
}

// Home returns T3 Code's home directory: the trimmed T3CODE_HOME, else ~/.t3.
func Home() (string, error) {
	if home := strings.TrimSpace(os.Getenv(EnvHome)); home != "" {
		return home, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("t3: resolve home directory: %w", err)
	}
	return filepath.Join(userHome, ".t3"), nil
}

// RuntimePath returns the server runtime file below a T3 home directory.
func RuntimePath(home string) string {
	return filepath.Join(home, "userdata", "server-runtime.json")
}

// ReadRuntime reads the server runtime file below home. A missing file is ErrNotRunning. The file is
// only ever read: the client never writes below the T3 home.
func ReadRuntime(home string) (Runtime, error) {
	path := RuntimePath(home)
	data, err := os.ReadFile(path) //nolint:gosec // the path is T3 Code's own runtime file
	if errors.Is(err, fs.ErrNotExist) {
		return Runtime{}, fmt.Errorf("%w: %s does not exist", ErrNotRunning, path)
	}
	if err != nil {
		return Runtime{}, fmt.Errorf("t3: read %s: %w", path, err)
	}
	var rt Runtime
	if err := json.Unmarshal(data, &rt); err != nil {
		return Runtime{}, fmt.Errorf("t3: decode %s: %w", path, err)
	}
	return rt, nil
}

// ResolveRuntime locates the server: it reads the runtime file below Home on every call and applies
// LOOPAI_T3_URL, which overrides the origin. With LOOPAI_T3_URL set the runtime file only supplies
// the PID and start time, and a missing or unreadable one leaves them zero.
func ResolveRuntime() (Runtime, error) {
	override := strings.TrimSpace(os.Getenv(EnvURL))
	home, err := Home()
	if err != nil {
		if override != "" {
			return Runtime{Origin: override}, nil
		}
		return Runtime{}, err
	}
	rt, err := ReadRuntime(home)
	switch {
	case override != "":
		if err != nil {
			rt = Runtime{}
		}
		rt.Origin = override
		return rt, nil
	case err != nil:
		return Runtime{}, err
	case strings.TrimSpace(rt.Origin) == "":
		return Runtime{}, fmt.Errorf("t3: %s has no origin", RuntimePath(home))
	}
	return rt, nil
}

// WebSocketURL turns an HTTP origin into the protocol-2 WebSocket endpoint: http becomes ws, https
// becomes wss, the path is /ws, and the query carries orchestrationProtocol=2.
func WebSocketURL(origin string) (string, error) {
	u, secure, err := parseOrigin(origin)
	if err != nil {
		return "", err
	}
	u.Scheme = "ws"
	if secure {
		u.Scheme = "wss"
	}
	u.Path = "/ws"
	u.RawQuery = url.Values{protocolQueryParam: {protocolVersion}}.Encode()
	return u.String(), nil
}

// httpOrigin normalizes an origin for the HTTP API: ws becomes http, wss becomes https, and any path
// or query is dropped.
func httpOrigin(origin string) (string, error) {
	u, secure, err := parseOrigin(origin)
	if err != nil {
		return "", err
	}
	u.Scheme = "http"
	if secure {
		u.Scheme = "https"
	}
	return u.String(), nil
}

// parseOrigin parses an http, https, ws, or wss origin, reporting whether it is secure, and strips
// everything but the scheme and host.
func parseOrigin(origin string) (u *url.URL, secure bool, err error) {
	u, err = url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return nil, false, fmt.Errorf("t3: parse origin %q: %w", origin, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
	case "https", "wss":
		secure = true
	default:
		return nil, false, fmt.Errorf("t3: origin %q is not an http or https URL", origin)
	}
	if u.Host == "" {
		return nil, false, fmt.Errorf("t3: origin %q has no host", origin)
	}
	return &url.URL{Host: u.Host}, secure, nil
}
