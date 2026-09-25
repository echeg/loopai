// Package t3 reports loopai runs to a running T3 Code server through its public HTTP and
// WebSocket APIs.
//
// The integration is best-effort: loopai only reads the server's runtime file and never writes
// below the T3 home directory, every change goes through the authenticated API, and failures
// never reach the run.
package t3

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// environment variables read by this package. The token and thread id are read here rather than
// through CLI options so a cmux hand-off never types the token into another shell.
const (
	EnvToken    = "LOOPAI_T3_TOKEN" //nolint:gosec // environment variable name, not a credential
	EnvURL      = "LOOPAI_T3_URL"
	EnvThreadID = "LOOPAI_T3_THREAD_ID"
	envT3Home   = "T3CODE_HOME"
)

// runtimeFileVersion is the only server-runtime.json schema version this package understands.
const runtimeFileVersion = 1

var (
	// ErrNoToken reports that LOOPAI_T3_TOKEN is unset.
	ErrNoToken = errors.New("t3: " + EnvToken + " is not set")
	// ErrNoRuntime reports that no running T3 Code server was found.
	ErrNoRuntime = errors.New("t3: no running T3 Code server found")
)

// Runtime is the subset of <T3 home>/userdata/server-runtime.json that loopai uses.
type Runtime struct {
	Version   int    `json:"version"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Origin    string `json:"origin"`
	StartedAt string `json:"startedAt"`
}

// Endpoint is the resolved server origin and bearer token.
type Endpoint struct {
	BaseURL string
	Token   string
}

// HomeDir resolves the T3 home directory the same way the server does: T3CODE_HOME after trimming
// and ~ expansion, otherwise ~/.t3.
func HomeDir(getenv func(string) string) (string, error) {
	if home := strings.TrimSpace(getenv(envT3Home)); home != "" {
		expanded, err := expandHome(home)
		if err != nil {
			return "", err
		}
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return "", fmt.Errorf("t3: resolve %s: %w", envT3Home, err)
		}
		return abs, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("t3: user home: %w", err)
	}
	return filepath.Join(userHome, ".t3"), nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("t3: user home: %w", err)
	}
	return filepath.Join(userHome, path[1:]), nil
}

// RuntimePath returns the server runtime file path for a T3 home directory.
func RuntimePath(home string) string {
	return filepath.Join(home, "userdata", "server-runtime.json")
}

// ReadRuntime reads the server runtime file. A missing file means no server is running.
func ReadRuntime(home string) (Runtime, error) {
	data, err := os.ReadFile(RuntimePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return Runtime{}, ErrNoRuntime
	}
	if err != nil {
		return Runtime{}, fmt.Errorf("t3: read runtime file: %w", err)
	}
	var rt Runtime
	if err := json.Unmarshal(data, &rt); err != nil {
		return Runtime{}, fmt.Errorf("t3: parse runtime file: %w", err)
	}
	if rt.Version != runtimeFileVersion {
		return Runtime{}, fmt.Errorf("t3: unsupported runtime file version %d", rt.Version)
	}
	if strings.TrimSpace(rt.Origin) == "" {
		return Runtime{}, errors.New("t3: runtime file has no origin")
	}
	return rt, nil
}

// ResolveEndpoint finds the server origin and bearer token. LOOPAI_T3_URL overrides the origin
// from the runtime file; the token comes only from LOOPAI_T3_TOKEN.
func ResolveEndpoint(getenv func(string) string) (Endpoint, error) {
	token := strings.TrimSpace(getenv(EnvToken))
	if token == "" {
		return Endpoint{}, ErrNoToken
	}
	if base := strings.TrimSpace(getenv(EnvURL)); base != "" {
		return Endpoint{BaseURL: strings.TrimRight(base, "/"), Token: token}, nil
	}
	home, err := HomeDir(getenv)
	if err != nil {
		return Endpoint{}, err
	}
	rt, err := ReadRuntime(home)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{BaseURL: strings.TrimRight(rt.Origin, "/"), Token: token}, nil
}

// NormalizePath mirrors T3 Code's normalizeProjectPathForComparison: trim, drop trailing
// separators except on a root, and for Windows drive or UNC paths use backslashes and lowercase.
func NormalizePath(path string) string {
	normalized := trimTrailingSeparators(strings.TrimSpace(path))
	if isWindowsDrivePath(normalized) || strings.HasPrefix(normalized, `\\`) {
		return strings.ToLower(strings.ReplaceAll(normalized, "/", `\`))
	}
	return normalized
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 2 || !isDriveLetter(path[0]) || path[1] != ':' {
		return false
	}
	return len(path) == 2 || path[2] == '/' || path[2] == '\\'
}

func isRootPath(path string) bool {
	if path == "/" || path == `\` {
		return true
	}
	return len(path) == 3 && isDriveLetter(path[0]) && path[1] == ':' && (path[2] == '/' || path[2] == '\\')
}

func trimTrailingSeparators(path string) string {
	if path == "" || isRootPath(path) {
		return path
	}
	var trimmed string
	if strings.HasPrefix(path, "/") {
		trimmed = strings.TrimRight(path, "/")
	} else {
		trimmed = strings.TrimRight(path, `/\`)
	}
	if trimmed == "" {
		return path
	}
	if len(trimmed) == 2 && isDriveLetter(trimmed[0]) && trimmed[1] == ':' {
		return trimmed + `\`
	}
	return trimmed
}
