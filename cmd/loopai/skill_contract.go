package main

import (
	"fmt"
	"io"
)

// skillContract is the version of the binary surface the Claude and Codex skills depend on: CLI
// flags, config keys, and the completion-report headings. every skill embeds the number it was
// written for and warns when `loopai --skill-contract` answers differently. bump it, together with
// `expected=` in every skill, whenever a surface change affects what a skill tells the model to do.
const skillContract = 1

// skillContractRequested reports whether the raw arguments ask for the skill contract. main needs
// the answer before flag parsing, because the version banner is printed first and stdout must carry
// the integer alone.
func skillContractRequested(args []string) bool {
	return rawFlagRequested(args, "--skill-contract")
}

// printSkillContract writes the contract number alone on one line, the form the skills parse.
func printSkillContract(w io.Writer) {
	fmt.Fprintln(w, skillContract)
}
