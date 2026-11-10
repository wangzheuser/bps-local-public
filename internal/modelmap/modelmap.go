// Package modelmap resolves a requested model to the model actually sent
// upstream. Patterns support only two wildcards: '*' for any run of characters
// (including none) and '?' for exactly one character.
package modelmap

import "strings"

// Rule aliases one model pattern onto a concrete target model.
type Rule struct {
	Pattern string
	Target  string
}

// HasWildcard reports whether s contains a wildcard character.
func HasWildcard(s string) bool {
	return strings.ContainsAny(s, "*?")
}

// Match reports whether model matches pattern. Only '*' and '?' are special;
// every other character, including regex metacharacters, compares literally.
//
// The scan is the classic two-pointer glob algorithm: no regex, so a hostile
// pattern cannot trigger catastrophic backtracking, and characters like '.'
// or '+' keep their literal meaning.
func Match(pattern, model string) bool {
	var p, m int
	// starP marks the last '*' seen; starM is the model offset to resume from.
	starP, starM := -1, 0
	for m < len(model) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == model[m]):
			p++
			m++
		case p < len(pattern) && pattern[p] == '*':
			starP = p
			starM = m
			p++
		case starP >= 0:
			// Backtrack: let the previous '*' absorb one more character.
			p = starP + 1
			starM++
			m = starM
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// literalLen is the number of non-wildcard characters in a pattern. Longer
// literals are more specific, which drives precedence.
func literalLen(pattern string) int {
	n := 0
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '*' && pattern[i] != '?' {
			n++
		}
	}
	return n
}

func wildcardCount(pattern string) int {
	n := 0
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '*' || pattern[i] == '?' {
			n++
		}
	}
	return n
}

// moreSpecific reports whether a should win over b. The order is:
//  1. an exact pattern (no wildcard) beats any wildcard pattern
//  2. a longer literal wins (gpt-6.1* beats gpt-6*)
//  3. fewer wildcards wins (gpt-6* beats gpt-?*)
//  4. lexicographically smaller pattern wins, so the choice is deterministic
func moreSpecific(a, b string) bool {
	aWild, bWild := HasWildcard(a), HasWildcard(b)
	if aWild != bWild {
		return !aWild
	}
	if la, lb := literalLen(a), literalLen(b); la != lb {
		return la > lb
	}
	if wa, wb := wildcardCount(a), wildcardCount(b); wa != wb {
		return wa < wb
	}
	return a < b
}

// Resolve returns the target of the single best matching rule. When nothing
// matches, ok is false and the caller passes the model through unchanged.
func Resolve(rules []Rule, model string) (target string, ok bool) {
	if model == "" {
		return "", false
	}
	best := -1
	for i, r := range rules {
		if !Match(r.Pattern, model) {
			continue
		}
		if best < 0 || moreSpecific(r.Pattern, rules[best].Pattern) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return rules[best].Target, true
}
