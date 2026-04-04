package main

import (
	"bytes"
	"strings"
	"testing"
)

func newTestPrompter(input string) (*Prompter, *bytes.Buffer) {
	r := strings.NewReader(input)
	w := &bytes.Buffer{}
	return NewPrompterFromReader(r, w), w
}

func TestPrompterString(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		defaultVal string
		want       string
	}{
		{"user input", "hello\n", "default", "hello"},
		{"empty uses default", "\n", "default", "default"},
		{"whitespace uses default", "  \n", "default", "default"},
		{"no newline (EOF)", "", "fallback", "fallback"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPrompter(tc.input)
			got := p.String("Prompt", tc.defaultVal)
			if got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrompterYesNo(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		defaultYes bool
		want       bool
	}{
		{"yes", "y\n", false, true},
		{"YES", "YES\n", false, true},
		{"no", "n\n", true, false},
		{"other", "maybe\n", true, false},
		{"empty default yes", "\n", true, true},
		{"empty default no", "\n", false, false},
		{"eof default yes", "", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPrompter(tc.input)
			got := p.YesNo("Continue?", tc.defaultYes)
			if got != tc.want {
				t.Errorf("YesNo() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrompterChoice(t *testing.T) {
	options := []string{"Apple", "Banana", "Cherry"}

	cases := []struct {
		name       string
		input      string
		defaultIdx int
		want       int
	}{
		{"select first", "1\n", 0, 0},
		{"select third", "3\n", 0, 2},
		{"empty uses default", "\n", 1, 1},
		{"eof uses default", "", 2, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPrompter(tc.input)
			got := p.Choice("Pick fruit:", options, tc.defaultIdx)
			if got != tc.want {
				t.Errorf("Choice() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPrompterChoiceInvalidThenValid(t *testing.T) {
	// First try is out of range, second is valid
	p, _ := newTestPrompter("99\n2\n")
	got := p.Choice("Pick:", []string{"A", "B", "C"}, 0)
	if got != 1 {
		t.Errorf("Choice() = %d, want 1 (second attempt)", got)
	}
}

func TestPrompterMultiSelect(t *testing.T) {
	options := []string{"A", "B", "C", "D"}

	cases := []struct {
		name       string
		input      string
		defaultAll bool
		want       []int
	}{
		{"select specific", "1,3\n", false, []int{0, 2}},
		{"select all", "all\n", false, []int{0, 1, 2, 3}},
		{"select none", "none\n", true, []int{}},
		{"empty default all", "\n", true, []int{0, 1, 2, 3}},
		{"empty default none", "\n", false, []int{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPrompter(tc.input)
			got := p.MultiSelect("Pick items:", options, tc.defaultAll)
			if len(got) != len(tc.want) {
				t.Errorf("MultiSelect() len = %d, want %d", len(got), len(tc.want))
				return
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("MultiSelect()[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPrompterFloat(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		defaultVal float64
		want       float64
	}{
		{"valid float", "42.5\n", 10, 42.5},
		{"integer", "100\n", 10, 100},
		{"empty uses default", "\n", 10, 10},
		{"invalid uses default", "abc\n", 10, 10},
		{"eof uses default", "", 10, 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPrompter(tc.input)
			got := p.Float("Enter value:", tc.defaultVal)
			if got != tc.want {
				t.Errorf("Float() = %g, want %g", got, tc.want)
			}
		})
	}
}

func TestMultiSelectDefault(t *testing.T) {
	options := []string{"A", "B", "C"}

	all := multiSelectDefault(options, true)
	if len(all) != 3 {
		t.Errorf("defaultAll=true: len = %d, want 3", len(all))
	}
	for i, v := range all {
		if v != i {
			t.Errorf("defaultAll=true: [%d] = %d, want %d", i, v, i)
		}
	}

	none := multiSelectDefault(options, false)
	if len(none) != 0 {
		t.Errorf("defaultAll=false: len = %d, want 0", len(none))
	}
}
