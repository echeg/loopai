package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/jessevdk/go-flags"

	"github.com/umputun/ralphex/pkg/config"
)

// skillContract is the version of the binary surface the Claude and Codex skills depend on: CLI
// flags, config keys, and the completion-report headings. every skill embeds the number it was
// written for and warns when `loopai --skill-contract` answers differently. bump it, together with
// `expected=` in every skill, whenever a surface change affects what a skill tells the model to do.
// TestSkillContract fingerprints that surface against testdata/skill-contract.golden, so a change
// cannot land without either a bump or a deliberate `-update`.
const skillContract = 3

// configKeyLine matches a key assignment in the embedded defaults config, commented or not: the
// file documents most keys as `# key =` lines left at their built-in default.
var configKeyLine = regexp.MustCompile(`^#?\s*([a-z][a-z0-9_]*)\s*=`)

// skillContractRequested reports whether the raw arguments ask for the skill contract. main needs
// the answer before flag parsing, because the version banner is printed first and stdout must carry
// the integer alone.
func skillContractRequested(args []string) bool {
	return rawFlagRequested(args, "--skill-contract")
}

// versionBannerWriter is where main prints the version banner: nowhere under --skill-contract,
// whose stdout carries the integer alone, stderr under --acp, whose stdout carries JSON-RPC only,
// and stdout otherwise.
func versionBannerWriter(args []string) io.Writer {
	if skillContractRequested(args) {
		return io.Discard
	}
	if acpRequested(args) {
		return os.Stderr
	}
	return os.Stdout
}

// rawFlagRequested reports whether flag appears in the raw arguments before a `--` terminator.
func rawFlagRequested(args []string, flag string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == flag {
			return true
		}
	}
	return false
}

// printSkillContract writes the contract number alone on one line, the form the skills parse.
func printSkillContract(w io.Writer) {
	fmt.Fprintln(w, skillContract)
}

// skillContractSurface renders the binary surface the skills depend on as readable text: visible
// long flags, config keys of the embedded defaults, and the level-two headings of the embedded
// report prompt. flags and keys are sorted; headings keep file order, since their order is part of
// the report contract.
func skillContractSurface() (string, error) {
	var b strings.Builder

	b.WriteString("flags:\n")
	for _, name := range visibleLongFlags() {
		b.WriteString("--" + name + "\n")
	}

	cfg, err := config.EmbeddedDefault("config")
	if err != nil {
		return "", fmt.Errorf("skill contract surface: %w", err)
	}
	keys := []string{}
	for line := range strings.Lines(string(cfg)) {
		if m := configKeyLine.FindStringSubmatch(line); len(m) == 2 {
			keys = append(keys, m[1])
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	b.WriteString("\nconfig keys:\n")
	for _, k := range keys {
		b.WriteString(k + "\n")
	}

	report, err := config.EmbeddedDefault("prompts/report.txt")
	if err != nil {
		return "", fmt.Errorf("skill contract surface: %w", err)
	}
	b.WriteString("\nreport headings:\n")
	for line := range strings.Lines(string(report)) {
		if heading, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "## "); ok {
			b.WriteString(strings.TrimSpace(heading) + "\n")
		}
	}
	return b.String(), nil
}

// visibleLongFlags returns the sorted long names of every non-hidden option in opts, each followed
// by its sorted choice values when it restricts them, since a skill may pass a value such as
// `--finalize=pr`. hidden options are the removed spellings kept only to print a migration error and
// --detached-run, which only the --detach launcher passes; no skill uses either.
// opts declares no groups, so every option sits in the parser's top-level group.
func visibleLongFlags() []string {
	var o opts
	var names []string
	for _, g := range flags.NewParser(&o, flags.None).Groups() {
		for _, opt := range g.Options() {
			if opt.Hidden {
				continue
			}
			name := opt.LongName
			if len(opt.Choices) > 0 {
				choices := slices.Sorted(slices.Values(opt.Choices))
				name += " [" + strings.Join(choices, "|") + "]"
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
