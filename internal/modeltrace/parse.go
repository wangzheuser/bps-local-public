package modeltrace

import (
	"regexp"
	"strconv"
	"strings"
)

var digitRun = regexp.MustCompile(`\d+`)

// ParseNumbers extracts the longest run of in-range integers from an answer.
//
// The reference parser treats an alphabetic separator as a boundary between
// independent runs, keeps only values inside [1, 355], and returns the longest
// run. That keeps prose or a stray count from polluting the histogram.
func ParseNumbers(text string) []int {
	type span struct{ start, end int }
	matches := digitRun.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}

	var runs [][]int
	var current []int
	prevEnd := 0
	for _, m := range matches {
		sep := text[prevEnd:m[0]]
		if len(current) > 0 && hasLetter(sep) {
			runs = append(runs, current)
			current = nil
		}
		v, err := strconv.Atoi(text[m[0]:m[1]])
		if err == nil && v >= ValueMin && v <= ValueMax {
			current = append(current, v)
		}
		prevEnd = m[1]
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	if len(runs) == 0 {
		return nil
	}
	longest := runs[0]
	for _, r := range runs[1:] {
		if len(r) > len(longest) {
			longest = r
		}
	}
	return longest
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// CountNumbers builds the histogram over [ValueMin, ValueMax].
func CountNumbers(numbers []int) []int {
	counts := make([]int, Dimension)
	for _, n := range numbers {
		if n >= ValueMin && n <= ValueMax {
			counts[n-ValueMin]++
		}
	}
	return counts
}

// normalizeSpace trims and collapses whitespace, used when echoing prompts.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
