package config

import (
	"errors"
	"fmt"
	"strings"
)

// ProviderSpec is one parsed provider[:model[:effort]] value. Model and Effort
// are empty when the spec leaves them to the provider CLI's own default.
type ProviderSpec struct {
	Provider string
	Model    string
	Effort   string
}

// sentinel errors wrapped by ParseProviderSpec, so callers can reword a failure
// without re-parsing the value
var (
	ErrEmptyProviderSpec   = errors.New("empty spec; expected provider[:model[:effort]]")
	ErrTooManySpecSegments = errors.New("too many ':' separators")
	ErrMissingProvider     = errors.New("missing a provider prefix")
	ErrUnknownProvider     = errors.New("unknown provider")
)

// ModelSpec returns the provider-specific model[:effort] remainder. An effort
// without a model keeps its leading colon, matching the executors' own parser.
func (s ProviderSpec) ModelSpec() string {
	if s.Effort == "" {
		return s.Model
	}
	return s.Model + ":" + s.Effort
}

// ParseProviderSpec parses a provider[:model[:effort]] value. The provider is
// mandatory and matched case-insensitively against claude, codex, and custom;
// empty model and effort segments mean the provider's default. A value whose
// leading segment is a recognizable model name instead of a provider is
// rejected with the prefixed rewrite, since the provider is never inferred.
func ParseProviderSpec(value string) (ProviderSpec, error) {
	spec := strings.TrimSpace(value)
	if spec == "" {
		return ProviderSpec{}, ErrEmptyProviderSpec
	}

	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return ProviderSpec{}, fmt.Errorf("%q has %w; expected provider[:model[:effort]]", spec, ErrTooManySpecSegments)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	provider := strings.ToLower(parts[0])
	switch provider {
	case ExternalReviewToolClaude, ExternalReviewToolCodex, ExternalReviewToolCustom:
	case "":
		return ProviderSpec{}, fmt.Errorf("%q is %w; expected claude, codex, or custom before the first ':'",
			spec, ErrMissingProvider)
	default:
		// a two-segment bare spec is the old model[:effort] form; a third segment
		// would make the prefixed rewrite four segments long, so it gets no suggestion
		if inferred := ModelProvider(spec); inferred != "" && len(parts) < 3 {
			return ProviderSpec{}, fmt.Errorf("%q is %w; write %q", spec, ErrMissingProvider,
				inferred+":"+strings.Join(parts, ":"))
		}
		return ProviderSpec{}, fmt.Errorf("%q names %w %q; expected claude, codex, or custom",
			spec, ErrUnknownProvider, parts[0])
	}

	result := ProviderSpec{Provider: provider}
	if len(parts) > 1 {
		result.Model = parts[1]
	}
	if len(parts) > 2 {
		result.Effort = parts[2]
	}
	return result, nil
}
