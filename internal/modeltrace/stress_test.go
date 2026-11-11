package modeltrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// stressCase is one generated reference case: the answers, and the full
// probability vector the Python reference produced for them.
type stressCase struct {
	Name      string              `json:"name"`
	Requested string              `json:"requested"`
	Used      int                 `json:"used"`
	Beta      float64             `json:"beta"`
	Answers   []Answer            `json:"answers"`
	WantTop   string              `json:"want_top"`
	WantProbs map[string]float64  `json:"want_probs"`
	WantAll   [][2]json.RawMessage `json:"want_all"`
}

// TestAttributeMatchesReferenceOnRandomData replays every generated case and
// compares the whole probability vector, not just the winner. Matching all 18
// probabilities rules out a coincidental top-1 agreement.
func TestAttributeMatchesReferenceOnRandomData(t *testing.T) {
	b := loadTestBank(t)
	path := filepath.Join("testdata", "stress.json")
	if _, err := os.Stat(path); err != nil {
		t.Skip("stress fixture not generated; run scripts/modeltrace_stress.py")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []stressCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no stress cases")
	}

	const tolerance = 1e-9
	for _, c := range cases {
		res, err := b.Attribute(c.Requested, c.Answers)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if res.Used != c.Used {
			t.Fatalf("%s: used = %d, want %d", c.Name, res.Used, c.Used)
		}
		if res.TopModel != c.WantTop {
			t.Fatalf("%s: top = %s, want %s", c.Name, res.TopModel, c.WantTop)
		}
		got := make(map[string]float64, len(res.Ranking))
		for _, r := range res.Ranking {
			got[r.Model] = r.Prob
		}
		for model, want := range c.WantProbs {
			diff := got[model] - want
			if diff < 0 {
				diff = -diff
			}
			if diff > tolerance {
				t.Fatalf("%s: p(%s) = %.12f, want %.12f (diff %.3g)",
					c.Name, model, got[model], want, diff)
			}
		}
		// The full vector must sum to 1 within float noise.
		var sum float64
		for _, p := range got {
			sum += p
		}
		if sum < 1-1e-6 || sum > 1+1e-6 {
			t.Fatalf("%s: probabilities sum to %v", c.Name, sum)
		}
	}
	t.Logf("verified %d random cases against the Python reference", len(cases))
}

// TestParseNumbersMatchesReference pins the parser on shapes that appear in
// real model output.
func TestParseNumbersMatchesReference(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []int
	}{
		{"json array", `[1, 22, 355]`, []int{1, 22, 355}},
		{"comma separated", `1, 22, 355`, []int{1, 22, 355}},
		{"space separated", `1 22 355`, []int{1, 22, 355}},
		{"out of range dropped", `0 1 355 356 400`, []int{1, 355}},
		{"letters split runs", `here are 1 2 3 values: 10 20 30 40 50`,
			[]int{10, 20, 30, 40, 50}},
		{"preamble count ignored", `218 values: 5 9 12 7`, []int{5, 9, 12, 7}},
		{"no digits", `nothing here`, nil},
		{"all out of range", `0 356 999`, nil},
		{"newlines", "1\n2\n3", []int{1, 2, 3}},
	}
	for _, c := range cases {
		got := ParseNumbers(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}

// TestAttributeRejectsShortAnswers checks the acceptance threshold, which is
// what keeps truncated replies out of the scoring.
func TestAttributeRejectsShortAnswers(t *testing.T) {
	b := loadTestBank(t)
	short := make([]int, 10)
	for i := range short {
		short[i] = i + 1
	}
	raw, _ := json.Marshal(short)
	if _, err := b.Attribute("gpt-5.6-sol", []Answer{{Expected: 218, Text: string(raw)}}); err == nil {
		t.Fatal("short answer must be rejected")
	}
}

// TestChallengesAreStable checks the probe prompts match the enrolled
// environment, since a different prompt invalidates the comparison.
func TestChallengesAreStable(t *testing.T) {
	cs := Challenges(MaxChallenges)
	if len(cs) != 3 {
		t.Fatalf("challenges = %d, want 3", len(cs))
	}
	wantCounts := []int{218, 233, 247}
	for i, c := range cs {
		if c.Expected != wantCounts[i] {
			t.Fatalf("challenge %d expected = %d, want %d", i, c.Expected, wantCounts[i])
		}
		if c.Prompt == "" {
			t.Fatalf("challenge %d has no prompt", i)
		}
	}
}

// Fewer probes take a prefix of the enrolled set, so the bank's calibration for
// that query count still applies.
func TestChallengesHonoursCount(t *testing.T) {
	for n := 1; n <= MaxChallenges; n++ {
		cs := Challenges(n)
		if len(cs) != n {
			t.Fatalf("Challenges(%d) returned %d", n, len(cs))
		}
		full := Challenges(MaxChallenges)
		for i := range cs {
			if cs[i].Prompt != full[i].Prompt {
				t.Fatalf("Challenges(%d)[%d] is not a prefix of the full set", n, i)
			}
		}
	}
	// Out-of-range counts clamp instead of returning an empty or oversized set.
	if got := len(Challenges(0)); got != 1 {
		t.Fatalf("Challenges(0) = %d, want 1", got)
	}
	if got := len(Challenges(99)); got != MaxChallenges {
		t.Fatalf("Challenges(99) = %d, want %d", got, MaxChallenges)
	}
}
