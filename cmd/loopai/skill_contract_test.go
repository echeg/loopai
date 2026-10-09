package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestVersionBannerWriterSkillContract(t *testing.T) {
	assert.Equal(t, io.Discard, versionBannerWriter([]string{"--skill-contract"}))
	assert.Equal(t, io.Discard, versionBannerWriter([]string{"--acp", "--skill-contract"}),
		"the integer must stay alone on stdout whatever else is passed")
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
