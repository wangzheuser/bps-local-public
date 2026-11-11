package modeltrace

import "math"

// orderedDims is the ordered-block feature width: four 16-bin chunks plus the
// ten last-digit bins.
const (
	orderedChunks        = 4
	orderedBinsPerChunk  = 16
	orderedLastDigits    = 10
	orderedDims          = orderedChunks*orderedBinsPerChunk + orderedLastDigits
)

// hellingerFeature turns a histogram into an L2-normalized square-root vector.
// The 0.5 smoothing keeps zero bins from dominating the distance.
func hellingerFeature(counts []int) []float64 {
	values := make([]float64, len(counts))
	var sum float64
	for i, c := range counts {
		v := float64(c) + alpha
		values[i] = v
		sum += v
	}
	out := make([]float64, len(counts))
	for i, v := range values {
		out[i] = math.Sqrt(v / sum)
	}
	return out
}

// orderedBlockFeature captures sequence structure: how values distribute
// across the first/second/third/fourth quarter, plus the last-digit bias.
func orderedBlockFeature(numbers []int) []float64 {
	out := make([]float64, 0, orderedDims)
	n := len(numbers)
	if n == 0 {
		return make([]float64, orderedDims)
	}
	for c := 0; c < orderedChunks; c++ {
		// np.array_split hands the remainder to the leading chunks, so the
		// boundary is ceil-based rather than a plain n/4 division.
		start := splitStart(n, c)
		end := splitStart(n, c+1)
		if end > n {
			end = n
		}
		counts := make([]float64, orderedBinsPerChunk)
		for _, v := range numbers[start:end] {
			// np.histogram with range (1,356) over 16 bins: width 355/16.
			bin := int(float64(v-ValueMin) * orderedBinsPerChunk / (ValueMax - ValueMin + 1))
			if bin < 0 {
				bin = 0
			}
			if bin >= orderedBinsPerChunk {
				bin = orderedBinsPerChunk - 1
			}
			counts[bin]++
		}
		var sum float64
		for i := range counts {
			counts[i] += 0.5
			sum += counts[i]
		}
		for _, v := range counts {
			out = append(out, math.Sqrt(v/sum))
		}
	}
	// Last digits use bincount(minlength=10) with the same 0.5 smoothing.
	digits := make([]float64, orderedLastDigits)
	for _, v := range numbers {
		digits[v%10] += 1
	}
	var sum float64
	for i := range digits {
		digits[i] += 0.5
		sum += digits[i]
	}
	for _, v := range digits {
		out = append(out, math.Sqrt(v/sum))
	}
	return out
}

// standardize converts to z-scores. A tiny floor keeps constant vectors finite.
func standardize(values []float64) []float64 {
	n := len(values)
	if n == 0 {
		return nil
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)
	var variance float64
	for _, v := range values {
		d := v - mean
		variance += d * d
	}
	variance /= float64(n)
	scale := math.Sqrt(variance)
	if scale < 1e-12 {
		scale = 1e-12
	}
	out := make([]float64, n)
	for i, v := range values {
		out[i] = (v - mean) / scale
	}
	return out
}

// standardizeBy applies a stored mean/scale instead of recomputing.
func standardizeBy(values, mean, scale []float64) []float64 {
	out := make([]float64, len(values))
	for i := range values {
		s := scale[i]
		if s < 1e-12 {
			s = 1e-12
		}
		out[i] = (values[i] - mean[i]) / s
	}
	return out
}

// projectOut removes the shared environment directions, then L2-normalizes.
func projectOut(vec []float64, basis [][]float64) []float64 {
	out := append([]float64(nil), vec...)
	for _, b := range basis {
		var dot float64
		for i := range out {
			dot += out[i] * b[i]
		}
		for i := range out {
			out[i] -= dot * b[i]
		}
	}
	var norm float64
	for _, v := range out {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm < 1e-12 {
		norm = 1e-12
	}
	for i := range out {
		out[i] /= norm
	}
	return out
}

// dot is the cosine-style similarity against one centroid (both unit length).
func dot(a, b []float64) float64 {
	var sum float64
	for i := range a {
		if i >= len(b) {
			break
		}
		sum += a[i] * b[i]
	}
	return sum
}

// hellingerScores reproduces robust_score_counts: standardize, project out the
// nuisance directions, normalize, then correlate with each model centroid.
func hellingerScores(counts []int, b *Bank) []float64 {
	feature := hellingerFeature(counts)
	scaled := standardizeBy(feature, b.hellinger.featureMean, b.hellinger.featureScale)
	projected := projectOut(scaled, b.hellinger.nuisanceBasis)
	scores := make([]float64, len(b.hellinger.centroids))
	for i, c := range b.hellinger.centroids {
		scores[i] = dot(projected, c)
	}
	return standardize(scores)
}

// orderedScores reproduces ordered_block_scores: match the best environment
// template, blend it with the nuisance-projected centroid correlation.
func orderedScores(numbers []int, b *Bank) []float64 {
	if b.orderedWeight == 0 {
		return nil
	}
	feature := orderedBlockFeature(numbers)
	scaled := standardizeBy(feature, b.ordered.featureMean, b.ordered.featureScale)

	// Template branch: normalize first, then for each model take the best match
	// across environments. environmentCentroids is [environments][models][dims],
	// matching the reference np.stack(...).max(axis=0).
	norm := l2norm(scaled)
	normalized := make([]float64, len(scaled))
	for i := range scaled {
		normalized[i] = scaled[i] / norm
	}
	template := make([]float64, len(b.Models))
	for i := range template {
		template[i] = math.Inf(-1)
	}
	for _, env := range b.ordered.environmentCentroids {
		for i := range template {
			if i >= len(env) {
				continue
			}
			if s := dot(normalized, env[i]); s > template[i] {
				template[i] = s
			}
		}
	}
	for i, v := range template {
		if math.IsInf(v, -1) {
			template[i] = 0
		}
	}
	templateStd := standardize(template)

	// Nuisance branch: project out, normalize, correlate with centroids.
	projected := projectOut(scaled, b.ordered.nuisanceBasis)
	nuisance := make([]float64, len(b.ordered.centroids))
	for i, c := range b.ordered.centroids {
		nuisance[i] = dot(projected, c)
	}
	nuisanceStd := standardize(nuisance)

	blended := make([]float64, len(templateStd))
	for i := range blended {
		blended[i] = 0.5*templateStd[i] + 0.5*nuisanceStd[i]
	}
	return standardize(blended)
}

// splitStart returns the first index of chunk i when n items are split into
// orderedChunks parts the way np.array_split does: leftover items go to the
// leading chunks.
func splitStart(n, i int) int {
	base := n / orderedChunks
	extra := n % orderedChunks
	if i <= extra {
		return i * (base + 1)
	}
	return extra*(base+1) + (i-extra)*base
}

func l2norm(v []float64) float64 {	var sum float64
	for _, x := range v {
		sum += x * x
	}
	n := math.Sqrt(sum)
	if n < 1e-12 {
		return 1e-12
	}
	return n
}

// fuse combines the marginal and ordered branches with the bank's weight.
func fuse(numbers []int, b *Bank) []float64 {
	marginal := hellingerScores(CountNumbers(numbers), b)
	if b.orderedWeight == 0 {
		return marginal
	}
	ordered := orderedScores(numbers, b)
	if ordered == nil {
		return marginal
	}
	out := make([]float64, len(marginal))
	for i := range out {
		out[i] = (1-b.orderedWeight)*marginal[i] + b.orderedWeight*ordered[i]
	}
	return out
}

// softmax turns scores into a distribution using the calibrated temperature.
func softmax(scores []float64, beta float64) []float64 {
	if len(scores) == 0 {
		return nil
	}
	scaled := make([]float64, len(scores))
	maximum := math.Inf(-1)
	for i, s := range scores {
		scaled[i] = beta * s
		if scaled[i] > maximum {
			maximum = scaled[i]
		}
	}
	var total float64
	for i := range scaled {
		scaled[i] = math.Exp(scaled[i] - maximum)
		total += scaled[i]
	}
	if total <= 0 {
		total = 1
	}
	for i := range scaled {
		scaled[i] /= total
	}
	return scaled
}
