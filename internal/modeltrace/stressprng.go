package modeltrace

// stressPRNG is a splitmix64 generator used only by tests to rebuild the
// fixture sequences from seeds. It lives in the non-test build so the Python
// generator script can be validated against the same arithmetic.
//
// Only integer operations are used: floating-point transcendental functions
// differ between Go and CPython in their last unit in the last place, which
// would make the regenerated sequences diverge.
type stressPRNG struct{ state uint64 }

func newStressPRNG(seed uint64) *stressPRNG { return &stressPRNG{state: seed} }

// next advances and returns the next 64-bit value.
func (p *stressPRNG) next() uint64 {
	p.state += 0x9E3779B97F4A7C15
	z := p.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// intn returns a value in [0, n).
func (p *stressPRNG) intn(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return p.next() % n
}

func clampValue(v int) int {
	if v < ValueMin {
		return ValueMin
	}
	if v > ValueMax {
		return ValueMax
	}
	return v
}

// StressSequence rebuilds one fixture sequence. name must match the generator
// names used by scripts/modeltrace_stress.py.
func StressSequence(name string, seed uint64, n int) []int {
	p := newStressPRNG(seed)
	out := make([]int, 0, n)
	switch name {
	case "uniform":
		for i := 0; i < n; i++ {
			out = append(out, 1+int(p.intn(355)))
		}
	case "skewed-low":
		// Triangular bias toward the low end: min of two draws.
		for i := 0; i < n; i++ {
			a, b := p.intn(355), p.intn(355)
			if b < a {
				a = b
			}
			out = append(out, 1+int(a))
		}
	case "skewed-high":
		for i := 0; i < n; i++ {
			a, b := p.intn(355), p.intn(355)
			if b < a {
				a = b
			}
			out = append(out, 355-int(a))
		}
	case "bimodal":
		for i := 0; i < n; i++ {
			if p.intn(2) == 0 {
				out = append(out, 1+int(p.intn(40)))
			} else {
				out = append(out, 300+int(p.intn(56)))
			}
		}
	case "digits":
		// Last digit biased to 0 and 5 so the ordered-block feature has signal.
		last := []int{0, 0, 5}
		for i := 0; i < n; i++ {
			v := (1+int(p.intn(35)))*10 + last[p.intn(3)]
			out = append(out, clampValue(v))
		}
	case "descending":
		// A trend across the sequence exercises the block features.
		for i := 0; i < n; i++ {
			center := 300
			if n > 1 {
				center = 300 - 240*i/(n-1)
			}
			out = append(out, clampValue(center+int(p.intn(41))-20))
		}
	case "boundary":
		choices := []int{1, 1, 355, 355, 2, 354}
		for i := 0; i < n; i++ {
			out = append(out, choices[p.intn(uint64(len(choices)))])
		}
	default:
		return nil
	}
	return out
}
