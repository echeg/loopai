package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		opts  Options
		body  string
	}{
		{"no frontmatter", "just a prompt", Options{}, "just a prompt"},
		{"model only", "---\nmodel: haiku\n---\nthe prompt", Options{Model: "haiku"}, "the prompt"},
		{"agent only", "---\nagent: code-reviewer\n---\nthe prompt", Options{AgentType: "code-reviewer"}, "the prompt"},
		{"both fields", "---\nmodel: sonnet\nagent: code-reviewer\n---\nthe prompt", Options{Model: "sonnet", AgentType: "code-reviewer"}, "the prompt"},
		{"unclosed frontmatter", "---\nmodel: haiku\nno closing", Options{}, "---\nmodel: haiku\nno closing"},
		{"empty body after frontmatter", "---\nmodel: haiku\n---\n", Options{Model: "haiku"}, ""},
		{"unknown keys ignored", "---\nmodel: opus\nfoo: bar\n---\nbody", Options{Model: "opus"}, "body"},
		{"whitespace in values", "---\nmodel:  haiku  \nagent:  code-reviewer  \n---\nbody", Options{Model: "haiku", AgentType: "code-reviewer"}, "body"},
		{"malformed yaml", "---\n: :\n  bad:\n---\nbody", Options{}, "---\n: :\n  bad:\n---\nbody"},

		// closing delimiter must be on its own line
		{"closing delimiter not on own line", "---\nmodel: haiku\n---extra\nbody", Options{}, "---\nmodel: haiku\n---extra\nbody"},
		{"closing delimiter with trailing text", "---\nmodel: haiku\n--- body", Options{}, "---\nmodel: haiku\n--- body"},

		// empty and minimal frontmatter
		{"empty frontmatter block", "---\n---\nbody", Options{}, "---\n---\nbody"},
		{"frontmatter only no trailing newline", "---\nmodel: haiku\n---", Options{Model: "haiku"}, ""},

		// yaml edge cases
		// model normalization
		{"full model id normalized", "---\nmodel: claude-sonnet-4-5-20250929\n---\nbody", Options{Model: "sonnet"}, "body"},
		{"full model id haiku normalized", "---\nmodel: claude-haiku-4-5-20251001\n---\nbody", Options{Model: "haiku"}, "body"},
		{"full model id opus normalized", "---\nmodel: claude-opus-4-6\n---\nbody", Options{Model: "opus"}, "body"},
		{"full model id fable normalized", "---\nmodel: claude-fable-5\n---\nbody", Options{Model: "fable"}, "body"},
		{"model keyword preserved", "---\nmodel: sonnet\n---\nbody", Options{Model: "sonnet"}, "body"},
		{"unknown model kept as-is", "---\nmodel: gpt-5\n---\nbody", Options{Model: "gpt-5"}, "body"},

		{"yaml type mismatch model number", "---\nmodel: 123\n---\nbody", Options{Model: "123"}, "body"},
		{"yaml null value", "---\nmodel: null\n---\nbody", Options{}, "body"},
		{"duplicate keys rejected", "---\nmodel: haiku\nmodel: opus\n---\nbody", Options{}, "---\nmodel: haiku\nmodel: opus\n---\nbody"},

		// body with dashes
		{"body contains triple dashes", "---\nmodel: haiku\n---\nsome text\n---\nmore text", Options{Model: "haiku"}, "some text\n---\nmore text"},

		// description marks an agent as dynamic (project-specific)
		{"description only", "---\ndescription: reviews sql migrations\n---\nbody", Options{Description: "reviews sql migrations"}, "body"},
		{"description absent", "---\nmodel: haiku\n---\nbody", Options{Model: "haiku"}, "body"},
		{"description empty", "---\ndescription: \n---\nbody", Options{}, "body"},
		{"description quoted empty", `---` + "\n" + `description: ""` + "\n---\nbody", Options{}, "body"},
		{"description multi-word with punctuation", "---\ndescription: checks HTTP handlers for auth, logging & errors\n---\nbody",
			Options{Description: "checks HTTP handlers for auth, logging & errors"}, "body"},
		{"description with all fields", "---\nmodel: opus\nagent: code-reviewer\ndescription: reviews migrations\n---\nbody",
			Options{Model: "opus", AgentType: "code-reviewer", Description: "reviews migrations"}, "body"},
		{"description whitespace trimmed by yaml", "---\ndescription:   spaced out   \n---\nbody", Options{Description: "spaced out"}, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, body := parseOptions(tt.input)
			assert.Equal(t, tt.opts, opts)
			assert.Equal(t, tt.body, body)
		})
	}
}

func TestParseAgentOptions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		opts  Options
		body  string
	}{
		{"frontmatter parsed", "---\ndescription: reviews migrations\nmodel: opus\n---\nthe body", Options{Model: "opus", Description: "reviews migrations"}, "the body"},
		{"no frontmatter", "plain body", Options{}, "plain body"},
		{"malformed frontmatter falls back to body", "---\n: :\n  bad:\n---\nbody", Options{}, "---\n: :\n  bad:\n---\nbody"},
		// the loader normalizes before parsing; the exported parser must match it or the
		// --gen-agents report contradicts what the review phase actually loads
		{"leading blank line", "\n---\ndescription: reviews races\n---\nbody", Options{Description: "reviews races"}, "body"},
		{"leading comment line", "# written by loopai\n---\ndescription: reviews sql\n---\nbody", Options{Description: "reviews sql"}, "body"},
		{"crlf endings", "---\r\ndescription: reviews signals\r\n---\r\nbody\r\n", Options{Description: "reviews signals"}, "body"},
		{"trailing whitespace", "---\ndescription: reviews docs\n---\nbody\n\n", Options{Description: "reviews docs"}, "body"},
		{"comment only, no frontmatter", "# just a comment\nbody", Options{}, "# just a comment\nbody"},
		// a well-formed block carrying only foreign keys must be consumed behind a comment
		// header exactly as it is without one, or its raw "---" lines land in the agent body
		{"foreign keys only", "---\nname: api\ntools: read\n---\nbody", Options{}, "body"},
		{"foreign keys only behind comment", "# hdr\n---\nname: api\ntools: read\n---\nbody", Options{}, "body"},
		{"empty description behind comment", "# hdr\n---\ndescription: \"\"\n---\nbody", Options{}, "body"},
		// the catalog renders the description as a single markdown list item; a block
		// scalar must not push unindented continuation lines into that structure
		{"block scalar description collapsed", "---\ndescription: |\n  reviews sql migrations\n  and schema changes\n---\nbody", Options{Description: "reviews sql migrations and schema changes"}, "body"},
		{"folded scalar description collapsed", "---\ndescription: >\n  reviews goroutine\n  lifetimes\n---\nbody", Options{Description: "reviews goroutine lifetimes"}, "body"},
		{"whitespace-only description is not dynamic", "---\ndescription: \"   \"\n---\nbody", Options{}, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, body := ParseAgentOptions(tt.input)
			assert.Equal(t, tt.opts, opts)
			assert.Equal(t, tt.body, body)
		})
	}
}

func TestAgentFrontmatterUnparsable(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"unquoted description with colon", "---\ndescription: sql: migrations\n---\nbody", true},
		{"broken block behind leading comment", "# written by loopai\n---\ndescription: sql: migrations\n---\nbody", true},
		{"crlf broken block", "---\r\ndescription: sql: migrations\r\n---\r\nbody\r\n", true},
		{"parsed frontmatter", "---\ndescription: reviews sql\n---\nbody", false},
		// a working agent may open its body with a markdown rule; the block above it
		// parsed fine and the file must not be reported as broken
		{"body opens with markdown rule", "---\nmodel: opus\n---\n\n---\n\nchecklist", false},
		{"no description but parsed", "---\nmodel: opus\n---\nbody", false},
		{"unknown keys only", "---\nfoo: bar\n---\n---\nbody", false},
		// valid YAML that simply carries no loopai key is not broken frontmatter; reporting
		// it as such sends the user to fix quoting when the real gap is a missing description
		{"foreign keys only behind comment", "# hdr\n---\nname: api\ntools: read\n---\nbody", false},
		{"no frontmatter at all", "plain body", false},
		{"comment only, no frontmatter", "# just a comment\nbody", false},
		{"rule inside plain body", "intro\n\n---\n\nmore", false},
		// no frontmatter block at all, the body just opens with a markdown rule. the
		// gap is a missing description, not broken YAML, and saying otherwise sends the
		// user chasing a quoting bug that does not exist
		{"body opens with markdown rule, no frontmatter", "---\n\nchecklist\n", false},
		{"unterminated opening delimiter", "---\ndescription: reviews sql\nbody", false},
		{"closing delimiter not on its own line", "---\ndescription: sql: x\n--- trailing\nbody", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AgentFrontmatterUnparsable(tt.input))
		})
	}
}

func TestOptions_String(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"empty options", Options{}, "model=default, subagent=general-purpose"},
		{"model only", Options{Model: "haiku"}, "model=haiku, subagent=general-purpose"},
		{"agent only", Options{AgentType: "code-reviewer"}, "model=default, subagent=code-reviewer"},
		{"both fields", Options{Model: "opus", AgentType: "code-reviewer"}, "model=opus, subagent=code-reviewer"},
		// description is catalog metadata, not an execution option: String() stays unchanged
		{"description only", Options{Description: "reviews migrations"}, "model=default, subagent=general-purpose"},
		{"description with model", Options{Model: "haiku", Description: "reviews migrations"}, "model=haiku, subagent=general-purpose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.opts.String())
		})
	}
}

func TestOptions_Validate(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		warnings []string
	}{
		{"empty options", Options{}, nil},
		{"valid model haiku", Options{Model: "haiku"}, nil},
		{"valid model sonnet", Options{Model: "sonnet"}, nil},
		{"valid model opus", Options{Model: "opus"}, nil},
		{"valid model fable", Options{Model: "fable"}, nil},
		{"unknown model", Options{Model: "gpt-5"}, []string{`unknown model "gpt-5", must be one of: haiku, sonnet, opus, fable`}},
		{"agent type not validated", Options{AgentType: "anything-goes"}, nil},
		{"unknown model with agent", Options{Model: "bad", AgentType: "reviewer"}, []string{`unknown model "bad", must be one of: haiku, sonnet, opus, fable`}},
		{"description not validated", Options{Description: "anything goes"}, nil},
		{"unknown model with description still warns", Options{Model: "bad", Description: "reviews migrations"},
			[]string{`unknown model "bad", must be one of: haiku, sonnet, opus, fable`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.warnings, tt.opts.Validate())
		})
	}
}

// AgentFileHasBody must agree with the agent loader, which strips comments before
// deciding whether a file contributes a prompt at all
func TestAgentFileHasBody(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"plain body", "review the diff\n", true},
		{"frontmatter and body", "---\ndescription: review sql\n---\nchecklist\n", true},
		{"leading comment then frontmatter", "# header\n---\ndescription: review sql\n---\nchecklist\n", true},
		{"crlf body", "---\r\ndescription: review sql\r\n---\r\nchecklist\r\n", true},
		{"body is a markdown rule", "---\ndescription: review sql\n---\n\n---\n\nchecklist\n", true},
		{"unparsable frontmatter still a body", "---\ndescription: a: b\n---\nchecklist\n", true},
		{"empty", "", false},
		{"whitespace only", "   \n\n\t\n", false},
		{"comments only", "# agent\n# review things\n", false},
		{"frontmatter only", "---\ndescription: review sql\n---\n", false},
		{"frontmatter with comment-only body", "---\ndescription: review sql\n---\n# nothing yet\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AgentFileHasBody(tt.content))
		})
	}
}
