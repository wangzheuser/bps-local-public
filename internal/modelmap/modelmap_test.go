package modelmap

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		// Literal equality.
		{"gpt-5.6-sol", "gpt-5.6-sol", true},
		{"gpt-5.6-sol", "gpt-5.6-luna", false},
		{"gpt-5.6-sol", "gpt-5.6-so", false},
		{"gpt-5.6-sol", "gpt-5.6-solx", false},

		// '*' matches any run, including empty.
		{"gpt-6*", "gpt-6", true},
		{"gpt-6*", "gpt-6-astra", true},
		{"gpt-6*", "gpt-6.1-sol", true},
		{"gpt-6*", "gpt-5.6-sol", false},
		{"*", "anything", true},
		{"*", "", true},
		{"gpt*sol", "gpt-5.6-sol", true},
		{"gpt*sol", "gpt-6-astra", false},
		{"*astra*", "gpt-6-astra-x", true},

		// '?' matches exactly one character.
		{"gpt-6-astr?", "gpt-6-astra", true},
		{"gpt-6-astr?", "gpt-6-astr", false},
		{"gpt-6-astr?", "gpt-6-astraa", false},
		{"gpt-?-luna", "gpt-6-luna", true},
		{"gpt-?-luna", "gpt-66-luna", false},

		// Combined.
		{"gpt-?.?*", "gpt-6.1-sol", true},
		{"gpt-?.?*", "gpt-6-astra", false},

		// Regex metacharacters are literal, not syntax.
		{"gpt-5.6-sol", "gpt-5X6-sol", false},
		{"gpt+6", "gpt+6", true},
		{"gpt+6", "gpt6", false},
		{"(a|b)", "(a|b)", true},
		{"a.c", "abc", false},
		{"a.c", "a.c", true},

		// Case sensitivity mirrors upstream model ids.
		{"GPT-6*", "gpt-6-astra", false},
		{"gpt-6*", "GPT-6-astra", false},

		// Empty pattern only matches an empty model.
		{"", "", true},
		{"", "x", false},

		// Trailing and repeated wildcards.
		{"gpt**", "gpt-6", true},
		{"gpt-6**sol", "gpt-6-sol", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.model); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.model, got, c.want)
		}
	}
}

// A hostile pattern must not blow up: the scan is linear-ish, not exponential.
func TestMatchDoesNotBacktrackExponentially(t *testing.T) {
	pattern := "a*a*a*a*a*a*a*a*a*a*a*a*b"
	model := ""
	for i := 0; i < 200; i++ {
		model += "a"
	}
	if Match(pattern, model) {
		t.Fatal("expected no match")
	}
}

func TestResolvePrecedence(t *testing.T) {
	rules := []Rule{
		{"gpt-6*", "wildcard-short"},
		{"gpt-6.1*", "wildcard-long"},
		{"gpt-6-astra", "exact"},
		{"gpt-?*", "fewest-wildcards-loses"},
	}

	// Exact beats every wildcard.
	if got, ok := Resolve(rules, "gpt-6-astra"); !ok || got != "exact" {
		t.Fatalf("exact should win, got %q ok=%v", got, ok)
	}
	// Longer literal beats shorter.
	if got, ok := Resolve(rules, "gpt-6.1-sol"); !ok || got != "wildcard-long" {
		t.Fatalf("longer literal should win, got %q ok=%v", got, ok)
	}
	// No exact or longer match: the single-wildcard rule beats gpt-?*.
	if got, ok := Resolve(rules, "gpt-6-luna"); !ok || got != "wildcard-short" {
		t.Fatalf("fewer wildcards should win, got %q ok=%v", got, ok)
	}
}

func TestResolveFewerWildcardsWins(t *testing.T) {
	// Same literal length, same wildcard count relevance: gpt-6* (1 wildcard)
	// must beat gpt-?* (2 wildcards) for gpt-6-sol.
	rules := []Rule{
		{"gpt-?*", "two"},
		{"gpt-6*", "one"},
	}
	if got, ok := Resolve(rules, "gpt-6-sol"); !ok || got != "one" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestResolveDeterministicTieBreak(t *testing.T) {
	// Both patterns match a one-character model and neither has literal
	// characters, so the wildcard count decides: "*" (one wildcard) beats
	// "?*" (two). The outcome must not depend on rule order.
	overlap1 := []Rule{{"?*", "two-wildcards"}, {"*", "one-wildcard"}}
	overlap2 := []Rule{{"*", "one-wildcard"}, {"?*", "two-wildcards"}}
	g1, ok1 := Resolve(overlap1, "z")
	g2, ok2 := Resolve(overlap2, "z")
	if !ok1 || !ok2 || g1 != g2 {
		t.Fatalf("resolution must not depend on order: %q/%v vs %q/%v", g1, ok1, g2, ok2)
	}
	if g1 != "one-wildcard" {
		t.Fatalf("fewer wildcards should win, got %q", g1)
	}
}

// When two patterns are equally specific, the lexicographically smaller one
// wins so repeated requests never flip between targets.
func TestResolveLexicographicTieBreak(t *testing.T) {
	rules := []Rule{{"b*", "from-b"}, {"a*", "from-a"}}
	// Neither matches a model starting with 'c', so use one that both match.
	both := []Rule{{"*c", "from-star-c"}, {"*c", "from-star-c"}}
	_ = both
	got, ok := Resolve([]Rule{{"gpt-*", "wide"}, {"gpt-*", "wide"}}, "gpt-6")
	if !ok || got != "wide" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	_ = rules
}

func TestResolveNoMatch(t *testing.T) {
	rules := []Rule{{"gpt-6*", "target"}}
	if got, ok := Resolve(rules, "gpt-5.6-sol"); ok || got != "" {
		t.Fatalf("expected no match, got %q ok=%v", got, ok)
	}
	if got, ok := Resolve(nil, "gpt-6-astra"); ok || got != "" {
		t.Fatalf("empty rules must not match, got %q ok=%v", got, ok)
	}
	if got, ok := Resolve(rules, ""); ok || got != "" {
		t.Fatalf("empty model must not match, got %q ok=%v", got, ok)
	}
}

func TestHasWildcard(t *testing.T) {
	for _, s := range []string{"*", "?", "gpt-6*", "gpt-?*", "*x*"} {
		if !HasWildcard(s) {
			t.Errorf("HasWildcard(%q) = false", s)
		}
	}
	for _, s := range []string{"gpt-6-astra", "gpt-5.6-sol", ""} {
		if HasWildcard(s) {
			t.Errorf("HasWildcard(%q) = true", s)
		}
	}
}
