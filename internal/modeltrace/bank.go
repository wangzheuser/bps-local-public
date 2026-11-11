// Package modeltrace identifies which model actually produced a set of numbers.
//
// Upstream plans silently route several model names onto one backend, so the
// name in the response proves nothing. Different models have stable,
// measurable biases in the integers they pick, and comparing those biases
// against an enrolled fingerprint bank reveals the real model.
//
// The algorithm mirrors the reference implementation: a nuisance-projected
// Hellinger similarity over the 1..355 histogram fused with an ordered-block
// feature, then a temperature-scaled softmax across the enrolled models.
package modeltrace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ValueMin and ValueMax bound the integers a challenge asks for.
const (
	ValueMin = 1
	ValueMax = 355
	// Dimension is the histogram width over [ValueMin, ValueMax].
	Dimension = ValueMax - ValueMin + 1
	alpha     = 0.5
)

// Model is one enrolled fingerprint.
type Model struct {
	ID         string `json:"id"`
	DisplayName string `json:"display_name"`
	Family     string `json:"family"`
}

// Bank is the loaded fingerprint bank.
type Bank struct {
	BuiltAt     string
	Models      []Model
	modelIndex  map[string]int
	calibration map[int]float64 // query count -> beta

	hellinger hellingerArtifact
	ordered   orderedArtifact
	orderedWeight float64
	minNumbers    int
}

type hellingerArtifact struct {
	featureMean []float64
	featureScale []float64
	nuisanceBasis [][]float64
	centroids     [][]float64
}

type orderedArtifact struct {
	featureMean          []float64
	featureScale         []float64
	nuisanceBasis        [][]float64
	centroids            [][]float64
	// environmentCentroids is [environments][models][dims].
	environmentCentroids [][][]float64
}

var (
	bankOnce   sync.Once
	bankCached *Bank
	bankErr    error
	bankPath   string
)

// SetBankPath points the loader at a bank file. An empty value falls back to the
// well-known locations, so container images need no configuration.
func SetBankPath(path string) {
	path = strings.TrimSpace(path)
	if path != bankPath {
		bankPath = path
		bankOnce = sync.Once{}
	}
}

// Load reads and validates the fingerprint bank once per path, resolving the
// path from the configured value or the search locations.
func Load() (*Bank, error) {
	bankOnce.Do(func() {
		resolved, err := ResolveBankPath(bankPath)
		if err != nil {
			bankErr = err
			return
		}
		bankCached, bankErr = loadBank(resolved)
	})
	return bankCached, bankErr
}

// bankSearchPaths are tried in order when no explicit path is configured, so a
// container image that ships the bank works with zero setup.
func bankSearchPaths() []string {
	paths := []string{
		// Shipped inside the image by the Dockerfile.
		"/usr/local/share/modeltrace/unified_bank.json",
		// Alongside the data volume, which operators can mount.
		"/data/unified_bank.json",
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths,
			filepath.Join(dir, "unified_bank.json"),
			filepath.Join(dir, "..", "share", "modeltrace", "unified_bank.json"),
		)
	}
	return paths
}

// ResolveBankPath returns the first readable bank path. An explicit path wins;
// otherwise the well-known locations are probed so container deployments need
// no configuration.
func ResolveBankPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("modeltrace: configured bank is unreadable: %w", err)
		}
		return explicit, nil
	}
	tried := make([]string, 0, 4)
	for _, p := range bankSearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		tried = append(tried, p)
	}
	return "", fmt.Errorf("modeltrace: no fingerprint bank found (tried %v); "+
		"set BPS_MODELTRACE_BANK or place unified_bank.json in one of those paths",
		strings.Join(tried, ", "))
}

func loadBank(path string) (*Bank, error) {
	if path == "" {
		return nil, fmt.Errorf("modeltrace: fingerprint bank path is not configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("modeltrace: read bank: %w", err)
	}
	var doc struct {
		BuiltAt string `json:"built_at"`
		Models  []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Family      string `json:"family"`
		} `json:"models"`
		MinimumValidNumbers int `json:"minimum_valid_numbers"`
		Robust struct {
			ModelOrder string `json:"-"`
			Hellinger  struct {
				FeatureMean   []float64   `json:"feature_mean"`
				FeatureScale  []float64   `json:"feature_scale"`
				NuisanceBasis [][]float64 `json:"nuisance_basis"`
				Centroids     [][]float64 `json:"centroids"`
			} `json:"hellinger"`
			OrderedBlocks struct {
				Weight               float64     `json:"weight"`
				FeatureMean          []float64   `json:"feature_mean"`
				FeatureScale         []float64   `json:"feature_scale"`
				NuisanceBasis        [][]float64   `json:"nuisance_basis"`
				Centroids            [][]float64   `json:"centroids"`
				EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
			} `json:"ordered_blocks"`
		} `json:"robust"`
		Calibration map[string]struct {
			Beta float64 `json:"beta"`
		} `json:"calibration"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("modeltrace: decode bank: %w", err)
	}
	if len(doc.Models) == 0 {
		return nil, fmt.Errorf("modeltrace: bank has no models")
	}

	b := &Bank{
		BuiltAt:    doc.BuiltAt,
		modelIndex: make(map[string]int, len(doc.Models)),
		calibration: make(map[int]float64, len(doc.Calibration)),
		minNumbers: doc.MinimumValidNumbers,
	}
	for i, m := range doc.Models {
		b.Models = append(b.Models, Model{ID: m.ID, DisplayName: m.DisplayName, Family: m.Family})
		b.modelIndex[m.ID] = i
	}
	if b.minNumbers <= 0 {
		b.minNumbers = 80
	}
	for k, v := range doc.Calibration {
		var n int
		if _, err := fmt.Sscanf(k, "%d", &n); err == nil {
			b.calibration[n] = v.Beta
		}
	}

	h := doc.Robust.Hellinger
	b.hellinger = hellingerArtifact{
		featureMean: h.FeatureMean, featureScale: h.FeatureScale,
		nuisanceBasis: h.NuisanceBasis, centroids: h.Centroids,
	}
	o := doc.Robust.OrderedBlocks
	b.ordered = orderedArtifact{
		featureMean: o.FeatureMean, featureScale: o.FeatureScale,
		nuisanceBasis: o.NuisanceBasis, centroids: o.Centroids,
		environmentCentroids: o.EnvironmentCentroids,
	}
	b.orderedWeight = o.Weight

	// A malformed bank would silently produce nonsense scores, so fail loudly.
	if len(b.hellinger.featureMean) != Dimension || len(b.hellinger.featureScale) != Dimension {
		return nil, fmt.Errorf("modeltrace: bank hellinger dimensions are not %d", Dimension)
	}
	if len(b.hellinger.centroids) != len(b.Models) {
		return nil, fmt.Errorf("modeltrace: bank hellinger centroids do not match model count")
	}
	if b.orderedWeight > 0 {
		if len(b.ordered.featureMean) != orderedDims || len(b.ordered.centroids) != len(b.Models) {
			return nil, fmt.Errorf("modeltrace: bank ordered-block dimensions are inconsistent")
		}
	}
	return b, nil
}

// MinNumbers is the smallest accepted answer length.
func (b *Bank) MinNumbers() int { return b.minNumbers }

// betaFor picks the calibration temperature for a query count, clamping to the
// nearest available entry.
func (b *Bank) betaFor(queries int) float64 {
	if v, ok := b.calibration[queries]; ok {
		return v
	}
	best, bestKey := 12.0, -1
	for k, v := range b.calibration {
		d := k - queries
		if d < 0 {
			d = -d
		}
		if bestKey < 0 || d < bestKey {
			best, bestKey = v, d
		}
	}
	return best
}

