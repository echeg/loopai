package t3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

const localAuthTimeout = 15 * time.Second

// ResolveSettleEndpoint resolves credentials for a best-effort local merge close-out. An explicit
// token follows ResolveEndpoint's usual rules. Otherwise, a local runtime must exist and any URL
// override must match its origin before the T3 CLI may issue a short-lived settlement token.
// The issued bearer token stays in memory and the process environment is not changed.
func ResolveSettleEndpoint(ctx context.Context, getenv func(string) string) (Endpoint, error) {
	return resolveLocalEndpoint(ctx, getenv, "settlement", "1h", "loopai-settle")
}

// resolveLocalEndpoint keeps locally issued credentials in memory and rejects a different server.
func resolveLocalEndpoint(ctx context.Context, getenv func(string) string, purpose, ttl, label string) (Endpoint, error) {
	if strings.TrimSpace(getenv(EnvToken)) != "" {
		return ResolveEndpoint(getenv)
	}
	home, err := HomeDir(getenv)
	if err != nil {
		return Endpoint{}, err
	}
	rt, err := ReadRuntime(home)
	if err != nil {
		return Endpoint{}, err
	}
	base := strings.TrimRight(strings.TrimSpace(rt.Origin), "/")
	if override := strings.TrimRight(strings.TrimSpace(getenv(EnvURL)), "/"); override != "" && override != base {
		return Endpoint{}, fmt.Errorf("t3: URL override differs from the local runtime: %w", ErrNoToken)
	}

	ctx, cancel := context.WithTimeout(ctx, localAuthTimeout)
	defer cancel()
	cmd, err := localTokenCommand(ctx, home, ttl, label)
	if err != nil {
		return Endpoint{}, err
	}
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return Endpoint{}, fmt.Errorf("t3: issue %s token: %w", purpose, ctx.Err())
		}
		// CLI output can contain credentials, so only expose the process error, never its output.
		return Endpoint{}, fmt.Errorf("t3: issue %s token: %w", purpose, err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return Endpoint{}, errors.New("t3: token command returned an empty or invalid token")
	}
	return Endpoint{BaseURL: base, Token: token}, nil
}

// localTokenCommand uses the official CLI, including its documented npx fallback when the
// desktop installation has not installed t3 on PATH. Override an inherited T3CODE_HOME with the
// resolved home so injected environment readers and the CLI always use the same runtime.
func localTokenCommand(ctx context.Context, home, ttl, label string) (*exec.Cmd, error) {
	args := []string{"auth", "session", "issue", "--token-only", "--ttl", ttl, "--label", label}
	bin, err := exec.LookPath("t3")
	if errors.Is(err, exec.ErrNotFound) {
		bin, err = exec.LookPath("npx")
		args = append([]string{"--yes", "t3@latest"}, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("t3: find token command (install t3 or npx): %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// A merge may have just removed the worktree containing the caller's working directory.
	cmd.Dir = home
	cmd.Env = append(os.Environ(), envT3Home+"="+home)
	cmd.Stderr = io.Discard
	// npx can spawn a child that inherits the output pipe. Bound that wait after cancellation.
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd, nil
}
