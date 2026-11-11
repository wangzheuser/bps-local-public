package modeltrace

import "fmt"

// Challenge is one prompt the probe sends to a model.
type Challenge struct {
	ID       string
	Prompt   string
	Expected int
}

// directInstruction mirrors the reference wording: it forbids tools and
// rule-made sequences, which is what makes first-instinct choices comparable
// with the enrolled bank.
const directInstruction = " The current language model must complete this directly without tools, Python, code execution, " +
	"calculators, search, APIs, or external random generators. Choose every position separately. " +
	"Do not count upward or downward, including 1, 2, 3, and do not use an arithmetic progression, " +
	"repeating cycle, repeated block, or another rule-made pattern. Accidental repetitions are valid. " +
	"Once an item is written, do not sort, reorder, deduplicate, replace, or repair the list."

// environment01 counts come from the reference suite's first environment, which
// every enrolled model was sampled under.
var environment01 = []struct {
	count int
	tmpl  string
}{
	{218, "For each of %d positions, make one separate first-instinct choice of an integer from 1 to 355 inclusive."},
	{233, "Provide exactly %d whole-number choices between 1 and 355 inclusive, choosing each item separately as it comes to mind."},
	{247, "Fill exactly %d positions with integers from the closed interval [1, 355], making a separate immediate choice for every position."},
}

// Challenges returns the three probes used for attribution.
func Challenges() []Challenge {
	out := make([]Challenge, 0, len(environment01))
	for i, e := range environment01 {
		out = append(out, Challenge{
			ID:       fmt.Sprintf("probe-%d", i+1),
			Expected: e.count,
			Prompt: fmt.Sprintf(e.tmpl, e.count) + directInstruction +
				" Return one compact JSON array containing the complete sequence and no explanation.",
		})
	}
	return out
}
