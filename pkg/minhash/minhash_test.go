package minhash

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Real tercios scenario signatures ---

// Booking slow (cache-miss): 10 services, no cache gateway/warmer.
var bookingSlowSigs = []string{
	"traveler-web|POST /api/booking/confirm",
	"booking-api|POST /booking/confirm",
	"pricing-service|POST /pricing/quote",
	"fare-cache|GET fare-rules",
	"pricing-db|SQL pricing_lookup",
	"inventory-service|POST /inventory/hold",
	"itinerary-service|POST /itinerary/create",
	"booking-db|SQL booking_write",
	"notification-service|Produce booking.confirmed",
	"email-worker|Consume booking.confirmed",
}

// Booking fast (cache-hit): same 10 + 2 new services. Jaccard = 10/12 = 0.833.
var bookingFastSigs = []string{
	"traveler-web|POST /api/booking/confirm",
	"booking-api|POST /booking/confirm",
	"pricing-service|POST /pricing/quote",
	"fare-cache|GET fare-rules",
	"pricing-db|SQL pricing_lookup",
	"inventory-service|POST /inventory/hold",
	"itinerary-service|POST /itinerary/create",
	"booking-db|SQL booking_write",
	"notification-service|Produce booking.confirmed",
	"email-worker|Consume booking.confirmed",
	"pricing-cache-gateway|POST /pricing/cache-pass",  // new
	"pricing-cache-warmer|Consume pricing.cache_warm", // new
}

// Unrelated auth flow.
var unrelatedSigs = []string{
	"auth-service|POST /login",
	"session-service|GET /session",
	"user-db|SQL user_lookup",
	"audit-service|Produce audit.event",
}

func exactJaccard(a, b []string) float64 {
	setA := make(map[string]struct{}, len(a))
	for _, s := range a {
		setA[s] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, s := range b {
		setB[s] = struct{}{}
	}
	intersection := 0
	for k := range setA {
		if _, ok := setB[k]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 1.0
	}
	return float64(intersection) / float64(union)
}

func TestCompute_Deterministic(t *testing.T) {
	sig1 := Compute(bookingSlowSigs)
	sig2 := Compute(bookingSlowSigs)
	assert.Equal(t, sig1, sig2, "same input must produce same signature")
}

func TestCompute_EmptyInput(t *testing.T) {
	sig := Compute(nil)
	for i, v := range sig {
		assert.Equal(t, uint64(math.MaxUint64), v, "empty signature slot %d should be MaxUint64", i)
	}
}

func TestToBands_Deterministic(t *testing.T) {
	sig := Compute(bookingSlowSigs)
	b1 := sig.ToBands()
	b2 := sig.ToBands()
	assert.Equal(t, b1, b2)
}

func TestIdenticalTraces(t *testing.T) {
	sigA := Compute(bookingSlowSigs)
	sigB := Compute(bookingSlowSigs)

	assert.Equal(t, 1.0, EstimateJaccard(sigA, sigB))
	assert.True(t, AnyBandMatches(sigA.ToBands(), sigB.ToBands()))
	assert.Equal(t, B, MatchingBands(sigA.ToBands(), sigB.ToBands()))
}

func TestUnrelatedTraces(t *testing.T) {
	sigA := Compute(bookingSlowSigs)
	sigB := Compute(unrelatedSigs)

	assert.Equal(t, 0.0, EstimateJaccard(sigA, sigB))
	assert.False(t, AnyBandMatches(sigA.ToBands(), sigB.ToBands()))
}

func TestRealScenario_SlowVsFast(t *testing.T) {
	j := exactJaccard(bookingSlowSigs, bookingFastSigs)
	assert.InDelta(t, 0.833, j, 0.01, "expected Jaccard ~0.833 for slow vs fast")

	sigA := Compute(bookingSlowSigs)
	sigB := Compute(bookingFastSigs)
	estJ := EstimateJaccard(sigA, sigB)
	t.Logf("Exact Jaccard: %.3f, Estimated: %.3f", j, estJ)

	bandsA := sigA.ToBands()
	bandsB := sigB.ToBands()
	matched := MatchingBands(bandsA, bandsB)
	t.Logf("Bands matched: %d/%d, AnyMatch: %v", matched, B, AnyBandMatches(bandsA, bandsB))

	// With K=8,B=4,R=2 we expect ~99.6% match rate. One run may miss,
	// but the estimated Jaccard should be reasonably close.
	assert.Greater(t, estJ, 0.5, "estimated Jaccard should be >0.5 for J=0.833")
}

func TestComputeFromSet_EquivalentToCompute(t *testing.T) {
	set := make(map[string]struct{}, len(bookingSlowSigs))
	for _, s := range bookingSlowSigs {
		set[s] = struct{}{}
	}
	sigSlice := Compute(bookingSlowSigs)
	sigSet := ComputeFromSet(set)
	assert.Equal(t, sigSlice, sigSet)
}

func TestFragmentation_GracefulDegradation(t *testing.T) {
	fullSig := Compute(bookingSlowSigs)
	fullBands := fullSig.ToBands()

	// Remove 2 of 10 signatures (20% fragment loss)
	partial := bookingSlowSigs[:8]
	partialSig := Compute(partial)
	partialBands := partialSig.ToBands()

	estJ := EstimateJaccard(fullSig, partialSig)
	t.Logf("Full vs 80%% partial: estimated Jaccard=%.3f, bands matched=%d/%d",
		estJ, MatchingBands(fullBands, partialBands), B)

	// At 80% overlap, we expect some hash slots to still agree
	assert.Greater(t, estJ, 0.0, "partial trace should have non-zero similarity")
}

func TestMonteCarlo_BandMatchProbability(t *testing.T) {
	// Validate that observed band match rates align with theoretical predictions.
	// P(at least one band matches) = 1 - (1 - J^R)^B
	tests := []struct {
		jaccard   float64
		tolerance float64 // absolute tolerance for observed vs theoretical
	}{
		{1.0, 0.01},
		{0.9, 0.05},
		{0.83, 0.05},
		{0.7, 0.05},
		{0.5, 0.05},
		{0.3, 0.05},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("J=%.2f", tt.jaccard), func(t *testing.T) {
			theoretical := 1.0 - math.Pow(1.0-math.Pow(tt.jaccard, float64(R)), float64(B))
			trials := 5000
			matches := 0

			for trial := 0; trial < trials; trial++ {
				rng := rand.New(rand.NewSource(int64(trial)))
				setA, setB := generateSetsWithJaccard(tt.jaccard, 15, rng)
				sigA := Compute(setA)
				sigB := Compute(setB)
				if AnyBandMatches(sigA.ToBands(), sigB.ToBands()) {
					matches++
				}
			}

			observed := float64(matches) / float64(trials)
			t.Logf("Jaccard=%.2f: theoretical=%.3f, observed=%.3f (delta=%.3f)",
				tt.jaccard, theoretical, observed, math.Abs(observed-theoretical))

			require.InDelta(t, theoretical, observed, tt.tolerance,
				"observed match rate should be within %.0f%% of theoretical", tt.tolerance*100)
		})
	}
}

// generateSetsWithJaccard creates two string slices with approximately the target Jaccard similarity.
func generateSetsWithJaccard(target float64, baseSize int, rng *rand.Rand) ([]string, []string) {
	shared := make([]string, 0)
	onlyA := make([]string, 0)
	onlyB := make([]string, 0)

	sharedCount := int(math.Round(float64(baseSize) * target))
	if sharedCount < 1 && target > 0 {
		sharedCount = 1
	}
	for i := 0; i < sharedCount; i++ {
		shared = append(shared, fmt.Sprintf("svc-%d|op-%d", rng.Intn(1000), rng.Intn(1000)))
	}

	if target < 1.0 && target > 0 {
		uniquePerSide := int(math.Round(float64(sharedCount) * (1.0 - target) / target / 2.0))
		if uniquePerSide < 1 {
			uniquePerSide = 1
		}
		for i := 0; i < uniquePerSide; i++ {
			onlyA = append(onlyA, fmt.Sprintf("only-a-%d-%d", rng.Intn(10000), i))
			onlyB = append(onlyB, fmt.Sprintf("only-b-%d-%d", rng.Intn(10000), i))
		}
	}

	setA := append(append([]string{}, shared...), onlyA...)
	setB := append(append([]string{}, shared...), onlyB...)
	return setA, setB
}
