package modeltrace

import (
	"fmt"
	"sort"
)

// Answer is one collected challenge response.
type Answer struct {
	Expected int
	Text     string
}

// Result is the attribution for one model.
type Result struct {
	ModelID      string
	TopModel     string  // highest-probability model
	TopProb      float64 // its probability
	SelfProb     float64 // probability of the model the caller asked for
	Used         int     // answers accepted
	Match        bool    // TopModel equals the requested model
	Ranking      []Rank  // full ranking, descending
}

// Rank is one entry of the probability ranking.
type Rank struct {
	Model string
	Prob  float64
	Score float64
}

// Attribute compares collected answers against the bank and reports which
// model most likely produced them.
//
// requested is the name the caller asked for; it only affects SelfProb and
// Match, never the scoring itself.
func (b *Bank) Attribute(requested string, answers []Answer) (*Result, error) {
	if b == nil {
		return nil, fmt.Errorf("modeltrace: bank is not loaded")
	}
	var fused [][]float64
	used := 0
	var diagnostics []string
	for i, a := range answers {
		numbers := ParseNumbers(a.Text)
		minimum := b.minNumbers
		if a.Expected > 0 {
			if scaled := (a.Expected*55 + 99) / 100; scaled > minimum {
				minimum = scaled
			}
		}
		if len(numbers) < minimum {
			diagnostics = append(diagnostics,
				fmt.Sprintf("answer %d: %d numbers, need %d", i, len(numbers), minimum))
			continue
		}
		fused = append(fused, fuse(numbers, b))
		used++
	}
	if used == 0 {
		msg := "modeltrace: no usable answer"
		if len(diagnostics) > 0 {
			msg += " (" + diagnostics[0] + ")"
		}
		return nil, fmt.Errorf("%s", msg)
	}

	// Average the per-answer z-scores, then one global softmax.
	combined := make([]float64, len(b.Models))
	for _, scores := range fused {
		for i := range combined {
			if i < len(scores) {
				combined[i] += scores[i]
			}
		}
	}
	for i := range combined {
		combined[i] /= float64(used)
	}
	probs := softmax(combined, b.betaFor(used))

	ranking := make([]Rank, len(b.Models))
	for i, m := range b.Models {
		ranking[i] = Rank{Model: m.ID, Prob: probs[i], Score: combined[i]}
	}
	sort.SliceStable(ranking, func(i, j int) bool { return ranking[i].Prob > ranking[j].Prob })

	res := &Result{
		ModelID:  requested,
		TopModel: ranking[0].Model,
		TopProb:  ranking[0].Prob,
		Used:     used,
		Ranking:  ranking,
		Match:    ranking[0].Model == requested,
	}
	for _, r := range ranking {
		if r.Model == requested {
			res.SelfProb = r.Prob
			break
		}
	}
	return res, nil
}
