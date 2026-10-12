package t3

import "context"

// ResolveProgressEndpoint resolves credentials for an explicitly selected existing thread.
// Without an explicit token, the local T3 CLI issues one that lasts through a long plan run.
// Tokens stay in memory; missing runtime or credentials disable only cosmetic title updates.
func ResolveProgressEndpoint(ctx context.Context, getenv func(string) string) (Endpoint, error) {
	return resolveLocalEndpoint(ctx, getenv, "progress", "7d", "loopai-progress")
}
