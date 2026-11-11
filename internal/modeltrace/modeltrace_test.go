package modeltrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// bankPathForTest resolves the fingerprint bank from the repo data dir.
func bankPathForTest(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "data", "unified_bank.json")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("fingerprint bank not present at %s", p)
	}
	return p
}

func loadTestBank(t *testing.T) *Bank {
	t.Helper()
	SetBankPath(bankPathForTest(t))
	b, err := Load()
	if err != nil {
		t.Fatalf("load bank: %v", err)
	}
	return b
}

func TestBankLoads(t *testing.T) {
	b := loadTestBank(t)
	if len(b.Models) != 18 {
		t.Fatalf("models = %d, want 18", len(b.Models))
	}
	if b.orderedWeight <= 0 {
		t.Fatal("ordered-block weight missing")
	}
	if b.MinNumbers() != 80 {
		t.Fatalf("min numbers = %d", b.MinNumbers())
	}
	if beta := b.betaFor(3); beta <= 0 {
		t.Fatalf("beta(3) = %v", beta)
	}
}

// TestAttributeMatchesReferenceScores pins the Go port against numbers produced
// by the Python reference implementation. Regenerating the fixtures means
// re-running scripts/modeltrace_golden.py.
func TestAttributeMatchesReferenceScores(t *testing.T) {
	b := loadTestBank(t)
	fixture := filepath.Join("testdata", "golden.json")
	if _, err := os.Stat(fixture); err != nil {
		t.Skip("golden fixture not generated")
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string   `json:"name"`
		Requested string  `json:"requested"`
		Answers  []Answer `json:"answers"`
		WantTop  string   `json:"want_top"`
		WantProbs map[string]float64 `json:"want_probs"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		res, err := b.Attribute(c.Requested, c.Answers)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if c.WantTop != "" && res.TopModel != c.WantTop {
			t.Fatalf("%s: top = %s, want %s", c.Name, res.TopModel, c.WantTop)
		}
		for model, want := range c.WantProbs {
			got := 0.0
			for _, r := range res.Ranking {
				if r.Model == model {
					got = r.Prob
					break
				}
			}
			// 1e-6 tolerance absorbs float64 summation order differences.
			if diff := got - want; diff > 1e-6 || diff < -1e-6 {
				t.Fatalf("%s: p(%s) = %.8f, want %.8f", c.Name, model, got, want)
			}
		}
	}
}
