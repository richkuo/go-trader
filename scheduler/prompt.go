package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Prompter wraps an input scanner and output writer for interactive prompts.
// Inject a custom reader/writer for tests.
type Prompter struct {
	scanner *bufio.Scanner
	out     io.Writer
}

// NewPrompter creates a Prompter using stdin/stdout.
func NewPrompter() *Prompter {
	return &Prompter{
		scanner: bufio.NewScanner(os.Stdin),
		out:     os.Stdout,
	}
}

// NewPrompterFromReader creates a Prompter with custom reader/writer (for tests).
func NewPrompterFromReader(r io.Reader, w io.Writer) *Prompter {
	return &Prompter{
		scanner: bufio.NewScanner(r),
		out:     w,
	}
}

// String prompts for a string value. Returns defaultVal on empty input.
func (p *Prompter) String(prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Fprintf(p.out, "%s: ", prompt)
	}
	if !p.scanner.Scan() {
		return defaultVal
	}
	input := strings.TrimSpace(p.scanner.Text())
	if input == "" {
		return defaultVal
	}
	return input
}

// YesNo prompts for a yes/no answer. Returns defaultYes on empty input.
func (p *Prompter) YesNo(prompt string, defaultYes bool) bool {
	def := "y/N"
	if defaultYes {
		def = "Y/n"
	}
	fmt.Fprintf(p.out, "%s [%s]: ", prompt, def)
	if !p.scanner.Scan() {
		return defaultYes
	}
	input := strings.TrimSpace(strings.ToLower(p.scanner.Text()))
	if input == "" {
		return defaultYes
	}
	return input == "y" || input == "yes"
}

// Choice prompts the user to pick one option from a numbered list.
// Returns the 0-based index of the selection.
func (p *Prompter) Choice(prompt string, options []string, defaultIdx int) int {
	fmt.Fprintln(p.out, prompt)
	for i, opt := range options {
		marker := " "
		if i == defaultIdx {
			marker = "*"
		}
		fmt.Fprintf(p.out, "  %s%d) %s\n", marker, i+1, opt)
	}
	for {
		fmt.Fprintf(p.out, "Enter choice [%d]: ", defaultIdx+1)
		if !p.scanner.Scan() {
			return defaultIdx
		}
		input := strings.TrimSpace(p.scanner.Text())
		if input == "" {
			return defaultIdx
		}
		n, err := strconv.Atoi(input)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
		fmt.Fprintln(p.out, "  Invalid choice, try again.")
	}
}

// MultiSelect prompts the user to pick multiple options from a numbered list.
// Accepts comma-separated numbers (e.g. "1,3"), "all", or "none".
// Returns a slice of 0-based indices.
func (p *Prompter) MultiSelect(prompt string, options []string, defaultAll bool) []int {
	return p.MultiSelectWithDefaults(prompt, options, multiSelectDefault(options, defaultAll))
}

// MultiSelectWithDefaults prompts the user to pick multiple options from a
// numbered list, using defaultIdxs when the input is empty.
func (p *Prompter) MultiSelectWithDefaults(prompt string, options []string, defaultIdxs []int) []int {
	fmt.Fprintln(p.out, prompt)
	for i, opt := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, opt)
	}
	defaultIdxs = normalizeMultiSelectIndices(defaultIdxs, len(options))
	def := multiSelectPromptDefault(options, defaultIdxs)
	for {
		fmt.Fprintf(p.out, "Enter comma-separated numbers, \"all\", or \"none\" [%s]: ", def)
		if !p.scanner.Scan() {
			return append([]int(nil), defaultIdxs...)
		}
		input := strings.TrimSpace(strings.ToLower(p.scanner.Text()))
		if input == "" {
			return append([]int(nil), defaultIdxs...)
		}
		if input == "all" {
			all := make([]int, len(options))
			for i := range options {
				all[i] = i
			}
			return all
		}
		if input == "none" {
			return []int{}
		}
		parts := strings.Split(input, ",")
		var result []int
		valid := true
		for _, part := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(options) {
				fmt.Fprintf(p.out, "  Invalid selection %q, try again.\n", strings.TrimSpace(part))
				valid = false
				break
			}
			result = append(result, n-1)
		}
		if valid && len(result) > 0 {
			return result
		}
		if valid {
			fmt.Fprintln(p.out, "  No items selected, try again.")
		}
	}
}

// Float prompts for a float64 value. Returns defaultVal on empty input or parse error.
func (p *Prompter) Float(prompt string, defaultVal float64) float64 {
	fmt.Fprintf(p.out, "%s [%.0f]: ", prompt, defaultVal)
	if !p.scanner.Scan() {
		return defaultVal
	}
	input := strings.TrimSpace(p.scanner.Text())
	if input == "" {
		return defaultVal
	}
	v, err := strconv.ParseFloat(input, 64)
	if err != nil {
		return defaultVal
	}
	return v
}

// FloatRange prompts for a float64 value in the half-open range (min, max].
// Re-prompts on out-of-range input so the wizard can only emit a value that
// passes downstream validation. Empty input returns defaultVal without
// re-prompting (the caller is responsible for picking a default inside range).
func (p *Prompter) FloatRange(prompt string, defaultVal, min, max float64) float64 {
	for {
		fmt.Fprintf(p.out, "%s [%.0f]: ", prompt, defaultVal)
		if !p.scanner.Scan() {
			return defaultVal
		}
		input := strings.TrimSpace(p.scanner.Text())
		if input == "" {
			return defaultVal
		}
		v, err := strconv.ParseFloat(input, 64)
		if err != nil {
			fmt.Fprintln(p.out, "  Invalid number, try again.")
			continue
		}
		if v <= min || v > max {
			fmt.Fprintf(p.out, "  Must be in (%.0f, %.0f], try again.\n", min, max)
			continue
		}
		return v
	}
}

func multiSelectDefault(options []string, defaultAll bool) []int {
	if defaultAll {
		all := make([]int, len(options))
		for i := range options {
			all[i] = i
		}
		return all
	}
	return []int{}
}

func normalizeMultiSelectIndices(indices []int, optionCount int) []int {
	seen := make(map[int]bool, len(indices))
	result := make([]int, 0, len(indices))
	for _, idx := range indices {
		if idx < 0 || idx >= optionCount || seen[idx] {
			continue
		}
		seen[idx] = true
		result = append(result, idx)
	}
	return result
}

func multiSelectPromptDefault(options []string, defaultIdxs []int) string {
	if len(defaultIdxs) == 0 {
		return "none"
	}
	if len(defaultIdxs) == len(options) {
		all := true
		for i, idx := range defaultIdxs {
			if idx != i {
				all = false
				break
			}
		}
		if all {
			return "all"
		}
	}
	parts := make([]string, len(defaultIdxs))
	for i, idx := range defaultIdxs {
		parts[i] = strconv.Itoa(idx + 1)
	}
	return strings.Join(parts, ",")
}
