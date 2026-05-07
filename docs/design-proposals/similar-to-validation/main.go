// similar-to-validation validates the MinHash design against real tercios scenario files.
//
// It extracts operation signature sets from scenario JSON, computes exact Jaccard,
// computes MinHash with configurable K/B/R, estimates Jaccard from MinHash,
// and reports band match results.
//
// Usage:
//
//	go run ./docs/design-proposals/similar-to-validation/
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// --- MinHash parameters ---
// Default configuration; configSearch tests alternatives.
const (
	K = 8 // total hash functions
	B = 2 // bands
	R = 4 // rows per band (K/B)
)

type minhashConfig struct {
	K int
	B int
	R int
}

// --- Tercios scenario model (minimal) ---

type scenario struct {
	Name     string                       `json:"name"`
	Services map[string]scenarioService   `json:"services"`
	Nodes    map[string]scenarioNode      `json:"nodes"`
	Edges    []scenarioEdge               `json:"edges"`
}

type scenarioService struct {
	Resource map[string]scenarioAttrValue `json:"resource"`
}

type scenarioNode struct {
	Service  string `json:"service"`
	SpanName string `json:"span_name"`
}

type scenarioEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type scenarioAttrValue struct {
	Value interface{} `json:"value"`
}

// --- Core logic ---

// extractSignatures returns the set of "service.name|span.name" strings from a scenario.
func extractSignatures(s scenario) map[string]struct{} {
	sigs := make(map[string]struct{})
	for nodeID, node := range s.Nodes {
		svc := s.Services[node.Service]
		svcName := ""
		if v, ok := svc.Resource["service.name"]; ok {
			svcName = fmt.Sprintf("%v", v.Value)
		}
		sig := svcName + "|" + node.SpanName
		sigs[sig] = struct{}{}
		_ = nodeID
	}
	return sigs
}

// jaccard computes exact Jaccard similarity between two sets.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	intersection := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 1.0
	}
	return float64(intersection) / float64(union)
}

// maxK is the largest K we test.
const maxK = 32

// seeds for MinHash. Fixed for reproducibility.
var seeds [maxK]uint64

func init() {
	rng := rand.New(rand.NewSource(42))
	for i := range seeds {
		seeds[i] = rng.Uint64()
	}
}

// computeMinHashN computes k minimum hash values for a signature set.
func computeMinHashN(sigs map[string]struct{}, k int) []uint64 {
	mh := make([]uint64, k)
	for i := range mh {
		mh[i] = math.MaxUint64
	}

	for sig := range sigs {
		b := []byte(sig)
		for i := 0; i < k; i++ {
			h := xxhash.New()
			var seedBytes [8]byte
			seedBytes[0] = byte(seeds[i])
			seedBytes[1] = byte(seeds[i] >> 8)
			seedBytes[2] = byte(seeds[i] >> 16)
			seedBytes[3] = byte(seeds[i] >> 24)
			seedBytes[4] = byte(seeds[i] >> 32)
			seedBytes[5] = byte(seeds[i] >> 40)
			seedBytes[6] = byte(seeds[i] >> 48)
			seedBytes[7] = byte(seeds[i] >> 56)
			_, _ = h.Write(seedBytes[:])
			_, _ = h.Write(b)
			v := h.Sum64()
			if v < mh[i] {
				mh[i] = v
			}
		}
	}
	return mh
}

func computeMinHash(sigs map[string]struct{}) [K]uint64 {
	slice := computeMinHashN(sigs, K)
	var arr [K]uint64
	copy(arr[:], slice)
	return arr
}

// estimateJaccard estimates Jaccard similarity from two MinHash signatures.
func estimateJaccard(a, b [K]uint64) float64 {
	matches := 0
	for i := range a {
		if a[i] == b[i] {
			matches++
		}
	}
	return float64(matches) / float64(K)
}

func estimateJaccardN(a, b []uint64) float64 {
	matches := 0
	for i := range a {
		if a[i] == b[i] {
			matches++
		}
	}
	return float64(matches) / float64(len(a))
}

// hashBand hashes R consecutive MinHash values into a single band hash.
func hashBand(values []uint64) uint64 {
	h := xxhash.New()
	var buf [8]byte
	for _, v := range values {
		buf[0] = byte(v)
		buf[1] = byte(v >> 8)
		buf[2] = byte(v >> 16)
		buf[3] = byte(v >> 24)
		buf[4] = byte(v >> 32)
		buf[5] = byte(v >> 40)
		buf[6] = byte(v >> 48)
		buf[7] = byte(v >> 56)
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}

// computeBands returns B band hashes from a MinHash signature.
func computeBands(mh [K]uint64) [B]uint64 {
	var bands [B]uint64
	for b := 0; b < B; b++ {
		start := b * R
		bands[b] = hashBand(mh[start : start+R])
	}
	return bands
}

func computeBandsN(mh []uint64, cfg minhashConfig) []uint64 {
	bands := make([]uint64, cfg.B)
	for b := 0; b < cfg.B; b++ {
		start := b * cfg.R
		end := start + cfg.R
		if end > len(mh) {
			end = len(mh)
		}
		bands[b] = hashBand(mh[start:end])
	}
	return bands
}

// anyBandMatches returns true if at least one band matches between two signatures.
func anyBandMatches(a, b [B]uint64) bool {
	for i := range a {
		if a[i] == b[i] {
			return true
		}
	}
	return false
}

func anyBandMatchesN(a, b []uint64) bool {
	for i := range a {
		if a[i] == b[i] {
			return true
		}
	}
	return false
}

// bandMatchCount returns how many bands match.
func bandMatchCount(a, b [B]uint64) int {
	count := 0
	for i := range a {
		if a[i] == b[i] {
			count++
		}
	}
	return count
}

// --- Partial trace simulation ---

// removeRandomSignatures simulates trace fragmentation by removing a fraction of signatures.
func removeRandomSignatures(sigs map[string]struct{}, removeFraction float64, seed int64) map[string]struct{} {
	rng := rand.New(rand.NewSource(seed))
	sorted := make([]string, 0, len(sigs))
	for k := range sigs {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	partial := make(map[string]struct{})
	for _, k := range sorted {
		if rng.Float64() >= removeFraction {
			partial[k] = struct{}{}
		}
	}
	// Ensure at least one signature remains
	if len(partial) == 0 && len(sorted) > 0 {
		partial[sorted[0]] = struct{}{}
	}
	return partial
}

// --- Main ---

func loadScenario(path string) (scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scenario{}, fmt.Errorf("read %s: %w", path, err)
	}
	var s scenario
	if err := json.Unmarshal(data, &s); err != nil {
		return scenario{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

func printSignatures(name string, sigs map[string]struct{}) {
	sorted := make([]string, 0, len(sigs))
	for k := range sigs {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	fmt.Printf("\n%s (%d unique signatures):\n", name, len(sorted))
	for _, s := range sorted {
		fmt.Printf("  %s\n", s)
	}
}

type comparison struct {
	nameA            string
	nameB            string
	exactJaccard     float64
	estimatedJaccard float64
	bandsMatched     int
	anyBandMatch     bool
}

func compareTraces(nameA string, sigsA map[string]struct{}, nameB string, sigsB map[string]struct{}) comparison {
	mhA := computeMinHash(sigsA)
	mhB := computeMinHash(sigsB)
	bandsA := computeBands(mhA)
	bandsB := computeBands(mhB)

	return comparison{
		nameA:            nameA,
		nameB:            nameB,
		exactJaccard:     jaccard(sigsA, sigsB),
		estimatedJaccard: estimateJaccard(mhA, mhB),
		bandsMatched:     bandMatchCount(bandsA, bandsB),
		anyBandMatch:     anyBandMatches(bandsA, bandsB),
	}
}

func printComparison(c comparison) {
	matchSymbol := "✗ FILTERED OUT"
	if c.anyBandMatch {
		matchSymbol = "✓ PASSES FILTER"
	}
	fmt.Printf("\n── %s vs %s ──\n", c.nameA, c.nameB)
	fmt.Printf("  Exact Jaccard:     %.3f\n", c.exactJaccard)
	fmt.Printf("  Estimated Jaccard: %.3f  (K=%d)\n", c.estimatedJaccard, K)
	fmt.Printf("  Bands matched:     %d/%d\n", c.bandsMatched, B)
	fmt.Printf("  Filter result:     %s\n", matchSymbol)
}

func main() {
	scenarioDir := os.Getenv("SCENARIO_DIR")
	if scenarioDir == "" {
		fmt.Fprintf(os.Stderr, "Set SCENARIO_DIR to the directory containing tercios scenario JSON files.\n")
		fmt.Fprintf(os.Stderr, "Example: SCENARIO_DIR=path/to/scenarios go run .\n")
		fmt.Fprintf(os.Stderr, "See https://github.com/javiermolinar/tercios for scenario file format.\n")
		os.Exit(1)
	}

	slowPath := scenarioDir + "/scenario-booking-distributed-slow-cache-miss.json"
	fastPath := scenarioDir + "/scenario-booking-distributed-fast-cache-hit.json"

	slow, err := loadScenario(slowPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fast, err := loadScenario(fastPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	slowSigs := extractSignatures(slow)
	fastSigs := extractSignatures(fast)

	// --- Print signatures ---
	printSignatures(slow.Name, slowSigs)
	printSignatures(fast.Name, fastSigs)

	// --- Set analysis ---
	shared := make([]string, 0)
	onlySlow := make([]string, 0)
	onlyFast := make([]string, 0)
	for k := range slowSigs {
		if _, ok := fastSigs[k]; ok {
			shared = append(shared, k)
		} else {
			onlySlow = append(onlySlow, k)
		}
	}
	for k := range fastSigs {
		if _, ok := slowSigs[k]; !ok {
			onlyFast = append(onlyFast, k)
		}
	}
	sort.Strings(shared)
	sort.Strings(onlySlow)
	sort.Strings(onlyFast)

	fmt.Printf("\n── Set Analysis ──\n")
	fmt.Printf("  Shared:    %d  %v\n", len(shared), shared)
	fmt.Printf("  Only slow: %d  %v\n", len(onlySlow), onlySlow)
	fmt.Printf("  Only fast: %d  %v\n", len(onlyFast), onlyFast)

	fmt.Printf("\n══════════════════════════════════════════\n")
	fmt.Printf("  TEST 1: Full traces\n")
	fmt.Printf("══════════════════════════════════════════\n")

	// --- Test 1: Slow vs Fast (real scenario) ---
	printComparison(compareTraces(slow.Name, slowSigs, fast.Name, fastSigs))

	// --- Test 2: Identical traces (same scenario, should be Jaccard=1.0) ---
	printComparison(compareTraces(slow.Name, slowSigs, slow.Name+" (copy)", slowSigs))

	// --- Test 3: Completely different trace ---
	unrelatedSigs := map[string]struct{}{
		"auth-service|POST /login":          {},
		"session-service|GET /session":      {},
		"user-db|SQL user_lookup":           {},
		"audit-service|Produce audit.event": {},
	}
	printComparison(compareTraces(slow.Name, slowSigs, "unrelated-auth-flow", unrelatedSigs))

	// --- Test 4: Fragmentation simulation ---
	fmt.Printf("\n══════════════════════════════════════════\n")
	fmt.Printf("  TEST 2: Fragmented traces (partial signature sets)\n")
	fmt.Printf("══════════════════════════════════════════\n")

	for _, removeFrac := range []float64{0.1, 0.2, 0.3, 0.5} {
		partialSlow := removeRandomSignatures(slowSigs, removeFrac, 123)
		fmt.Printf("\n  --- %.0f%% of slow trace signatures removed (%d/%d remaining) ---\n",
			removeFrac*100, len(partialSlow), len(slowSigs))

		// Partial slow vs full slow (same trace, fragmented)
		c := compareTraces("slow (partial)", partialSlow, slow.Name+" (full)", slowSigs)
		printComparison(c)

		// Partial slow vs full fast (cross-scenario, fragmented)
		c = compareTraces("slow (partial)", partialSlow, fast.Name+" (full)", fastSigs)
		printComparison(c)
	}

	// --- Test 5: Monte Carlo — compare configurations ---
	fmt.Printf("\n══════════════════════════════════════════\n")
	fmt.Printf("  TEST 3: Configuration comparison\n")
	fmt.Printf("          (10000 trials per Jaccard level)\n")
	fmt.Printf("══════════════════════════════════════════\n")

	configs := []minhashConfig{
		{K: 8, B: 2, R: 4},   // Original proposal: 16 bytes
		{K: 8, B: 4, R: 2},   // More bands, fewer rows: 32 bytes
		{K: 16, B: 4, R: 4},  // Double K: 32 bytes
		{K: 16, B: 8, R: 2},  // Many bands: 64 bytes
		{K: 12, B: 4, R: 3},  // Compromise: 32 bytes
		{K: 12, B: 6, R: 2},  // Max bands at K=12: 48 bytes
	}

	targetJaccards := []float64{1.0, 0.9, 0.83, 0.8, 0.7, 0.5, 0.3, 0.2}
	trials := 10000

	for _, cfg := range configs {
		storageBytesPerTrace := cfg.B * 8
		fmt.Printf("\n  Config: K=%d B=%d R=%d  (%d bytes/trace, %d parquet columns)\n",
			cfg.K, cfg.B, cfg.R, storageBytesPerTrace, cfg.B)
		fmt.Printf("  %-10s %-15s %-15s %-15s\n", "Jaccard", "Theory", "Observed", "FalseNeg%")
		fmt.Printf("  %s\n", strings.Repeat("─", 60))

		for _, targetJ := range targetJaccards {
			matches := 0
			for t := 0; t < trials; t++ {
				rng := rand.New(rand.NewSource(int64(t * 1000)))
				setA, setB := generateSetsWithJaccard(targetJ, 15, rng)
				mhA := computeMinHashN(setA, cfg.K)
				mhB := computeMinHashN(setB, cfg.K)
				bandsA := computeBandsN(mhA, cfg)
				bandsB := computeBandsN(mhB, cfg)
				if anyBandMatchesN(bandsA, bandsB) {
					matches++
				}
			}

			observed := float64(matches) / float64(trials)
			theoretical := theoreticalBandMatchProb(targetJ, cfg.B, cfg.R)
			falseNegPct := (1.0 - observed) * 100

			fmt.Printf("  %-10.2f %-15.3f %-15.3f %-15.1f\n", targetJ, theoretical, observed, falseNegPct)
		}
	}

	// --- Test 6: Real scenario across configs ---
	fmt.Printf("\n══════════════════════════════════════════\n")
	fmt.Printf("  TEST 4: Real scenarios across configs\n")
	fmt.Printf("══════════════════════════════════════════\n")

	fmt.Printf("\n  Slow vs Fast (J=0.833) — 10000 trials with random seed perturbation\n")
	fmt.Printf("  %-25s %-12s %-12s %-12s\n", "Config", "Bytes", "MatchRate", "FalseNeg%")
	fmt.Printf("  %s\n", strings.Repeat("─", 65))

	for _, cfg := range configs {
		matches := 0
		for t := 0; t < trials; t++ {
			// Perturb by removing 0-1 random signatures from slow set
			rng := rand.New(rand.NewSource(int64(t)))
			variantSlow := make(map[string]struct{})
			for k := range slowSigs {
				variantSlow[k] = struct{}{}
			}
			// Optionally add a random noise signature to simulate span variance
			if rng.Float64() < 0.3 {
				variantSlow[fmt.Sprintf("noise-svc-%d|noise-op-%d", rng.Intn(100), t)] = struct{}{}
			}

			mhA := computeMinHashN(variantSlow, cfg.K)
			mhB := computeMinHashN(fastSigs, cfg.K)
			bandsA := computeBandsN(mhA, cfg)
			bandsB := computeBandsN(mhB, cfg)
			if anyBandMatchesN(bandsA, bandsB) {
				matches++
			}
		}

		rate := float64(matches) / float64(trials)
		storage := cfg.B * 8
		fmt.Printf("  K=%d B=%d R=%d %13d %12.1f%% %11.1f%%\n",
			cfg.K, cfg.B, cfg.R, storage, rate*100, (1-rate)*100)
	}
}

// generateSetsWithJaccard creates two string sets with approximately the target Jaccard similarity.
func generateSetsWithJaccard(target float64, baseSize int, rng *rand.Rand) (map[string]struct{}, map[string]struct{}) {
	setA := make(map[string]struct{})
	setB := make(map[string]struct{})

	// Shared elements
	sharedCount := int(math.Round(float64(baseSize) * target))
	if sharedCount < 1 && target > 0 {
		sharedCount = 1
	}

	for i := 0; i < sharedCount; i++ {
		sig := fmt.Sprintf("svc-%d|op-%d", rng.Intn(100), rng.Intn(100))
		setA[sig] = struct{}{}
		setB[sig] = struct{}{}
	}

	// Unique elements to reach target Jaccard
	// J = shared / (shared + uniqueA + uniqueB)
	// For simplicity, add equal unique elements to each side
	if target < 1.0 && target > 0 {
		uniquePerSide := int(math.Round(float64(sharedCount) * (1.0 - target) / target / 2.0))
		if uniquePerSide < 1 {
			uniquePerSide = 1
		}
		for i := 0; i < uniquePerSide; i++ {
			setA[fmt.Sprintf("only-a-%d-%d", rng.Intn(10000), i)] = struct{}{}
			setB[fmt.Sprintf("only-b-%d-%d", rng.Intn(10000), i)] = struct{}{}
		}
	}

	return setA, setB
}

// theoreticalBandMatchProb computes P(at least one band matches) for given Jaccard, B bands, R rows.
func theoreticalBandMatchProb(j float64, b, r int) float64 {
	// P(band matches) = j^r
	// P(no band matches) = (1 - j^r)^b
	// P(at least one) = 1 - (1 - j^r)^b
	pBand := math.Pow(j, float64(r))
	return 1.0 - math.Pow(1.0-pBand, float64(b))
}
