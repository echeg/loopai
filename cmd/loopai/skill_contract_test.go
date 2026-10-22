package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateSkillContract rewrites testdata/skill-contract.golden from the current constant and surface.
var updateSkillContract = flag.Bool("update", false, "rewrite testdata/skill-contract.golden")

const skillContractGolden = "testdata/skill-contract.golden"

// skillContractMainEnv makes the test binary run main() with the unit-separator-delimited arguments it holds.
const skillContractMainEnv = "LOOPAI_TEST_SKILL_CONTRACT_MAIN"

// TestSkillContractMainHelper is the re-exec target of runSkillContractMain, not a test of its own.
func TestSkillContractMainHelper(t *testing.T) {
	raw, ok := os.LookupEnv(skillContractMainEnv)
	if !ok {
		t.Skip("helper process only")
	}
	os.Args = append([]string{"loopai"}, strings.Split(raw, "\x1f")...)
	main()
	t.Fatal("main returned without exiting")
}

// runSkillContractMain runs main() in a child process with HOME, USERPROFILE, and the config
// directory redirected to an empty temp directory and the working directory outside any repository.
func runSkillContractMain(t *testing.T, args ...string) (stdout string, code int) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSkillContractMainHelper$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		skillContractMainEnv+"="+strings.Join(args, "\x1f"),
		"HOME="+home, "USERPROFILE="+home,
		"LOOPAI_CONFIG_DIR="+filepath.Join(home, "config"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code = 0
	if err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		code = exitErr.ExitCode()
	}
	// the config directory is never created, so main returned before config loading
	_, statErr := os.Stat(filepath.Join(home, "config"))
	assert.True(t, os.IsNotExist(statErr), "the config directory must not be created")
	return out.String(), code
}

func TestSkillContractFlag(t *testing.T) {
	want := strconv.Itoa(skillContract) + "\n"

	t.Run("prints the integer alone", func(t *testing.T) {
		stdout, code := runSkillContractMain(t, "--skill-contract")
		assert.Equal(t, 0, code)
		assert.Equal(t, want, stdout, "no version banner on stdout")
	})

	t.Run("wins over --version", func(t *testing.T) {
		stdout, code := runSkillContractMain(t, "--version", "--skill-contract")
		assert.Equal(t, 0, code)
		assert.Equal(t, want, stdout)
	})

	// like --version, the flag ignores a plan argument rather than rejecting it
	t.Run("ignores a plan argument", func(t *testing.T) {
		stdout, code := runSkillContractMain(t, "--skill-contract", "docs/plans/missing.md")
		assert.Equal(t, 0, code)
		assert.Equal(t, want, stdout)
	})

	t.Run("after the terminator it is a positional", func(t *testing.T) {
		stdout, _ := runSkillContractMain(t, "--version", "--", "--skill-contract")
		assert.True(t, strings.HasPrefix(stdout, "loopai "), "banner printed: %q", stdout)
		assert.NotEqual(t, want, stdout)
	})
}

func TestSkillContractRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, false},
		{"flag", []string{"--skill-contract"}, true},
		{"with other flags", []string{"--debug", "--skill-contract", "plan.md"}, true},
		{"plain run", []string{"docs/plans/x.md"}, false},
		{"after terminator", []string{"--", "--skill-contract"}, false},
		{"prefix only", []string{"--skill-contract-x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, skillContractRequested(tc.args))
		})
	}
}

func TestVersionBannerWriter(t *testing.T) {
	assert.Equal(t, io.Discard, versionBannerWriter([]string{"--skill-contract"}))
	assert.Equal(t, io.Discard, versionBannerWriter([]string{"--debug", "--skill-contract", "plan.md"}),
		"the integer must stay alone on stdout whatever else is passed")
	assert.Same(t, os.Stdout, versionBannerWriter([]string{"plan.md"}))
	assert.Same(t, os.Stdout, versionBannerWriter(nil))
	assert.Same(t, os.Stdout, versionBannerWriter([]string{"--", "--skill-contract"}))
}

func TestPrintSkillContract(t *testing.T) {
	var buf bytes.Buffer
	printSkillContract(&buf)
	assert.Equal(t, strconv.Itoa(skillContract)+"\n", buf.String())
}

func TestIsStandaloneCommandSkillContract(t *testing.T) {
	assert.True(t, isStandaloneCommand(opts{SkillContract: true}))
}

func TestSkillContract(t *testing.T) {
	surface, err := skillContractSurface()
	require.NoError(t, err)
	current := fmt.Sprintf("contract: %d\n\n%s", skillContract, surface)

	if *updateSkillContract {
		require.NoError(t, os.WriteFile(skillContractGolden, []byte(current), 0o600))
		return
	}

	data, err := os.ReadFile(skillContractGolden)
	require.NoError(t, err, "golden missing; run go test ./cmd/loopai -run TestSkillContract -update")
	golden := string(data)

	header, goldenSurface, _ := strings.Cut(golden, "\n\n")
	require.Equal(t, fmt.Sprintf("contract: %d", skillContract), header,
		"skillContract changed; update the golden with go test ./cmd/loopai -run TestSkillContract -update")

	if goldenSurface != surface {
		diff, diffErr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(goldenSurface), B: difflib.SplitLines(surface),
			FromFile: skillContractGolden, ToFile: "current surface", Context: 2,
		})
		require.NoError(t, diffErr)
		t.Fatalf("contract surface changed; bump skillContract if skills depend on the change, "+
			"then run go test ./cmd/loopai -run TestSkillContract -update\n%s", diff)
	}
}

func TestSkillContractSurface(t *testing.T) {
	surface, err := skillContractSurface()
	require.NoError(t, err)

	section := func(name string) []string {
		_, rest, found := strings.Cut(surface, name+":\n")
		require.True(t, found, "section %s missing", name)
		body, _, _ := strings.Cut(rest, "\n\n")
		return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	}

	flagsSection := section("flags")
	assert.Contains(t, flagsSection, "--skill-contract")
	assert.Contains(t, flagsSection, "--task-model")
	assert.Contains(t, flagsSection, "--finalize [merge|none|pr|sync]", "choice values are part of the surface")
	for _, removed := range []string{"--codex", "--codex-only", "--external-review-tool", "--external-review-model",
		"--t3", "--no-t3", "--t3-launch", "--t3-progress-thread"} {
		assert.NotContains(t, flagsSection, removed, "hidden removed flag must be excluded")
	}
	assert.True(t, slices.IsSorted(flagsSection), "flags sorted")

	keys := section("config keys")
	assert.Contains(t, keys, "task_model", "a commented default key is included")
	assert.Contains(t, keys, "claude_command", "an uncommented key is included")
	assert.True(t, slices.IsSorted(keys), "keys sorted")
	assert.Len(t, slices.Compact(slices.Clone(keys)), len(keys), "keys deduplicated")

	headings := section("report headings")
	assert.Equal(t, []string{"Summary", "Change scope", "Evidence", "Risk", "Merge danger",
		"Migrations and operational steps", "Plan deviation", "Backlog", "External review", "Validation"}, headings,
		"headings keep file order")

	again, err := skillContractSurface()
	require.NoError(t, err)
	assert.Equal(t, surface, again, "deterministic across calls")
}

func TestConfigKeyLine(t *testing.T) {
	tests := []struct {
		line, want string
	}{
		{"claude_command = claude", "claude_command"},
		{"# task_model =", "task_model"},
		{"#   codex_args = -c windows.sandbox=\"elevated\"", "codex_args"},
		{"# example: codex_args = -c x", ""},
		{"# claude_command: the command to run", ""},
		{"# (task_model = opus:high must become claude:opus:high)", ""},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			m := configKeyLine.FindStringSubmatch(tc.line)
			if tc.want == "" {
				assert.Nil(t, m)
				return
			}
			require.NotNil(t, m)
			assert.Equal(t, tc.want, m[1])
		})
	}
}
