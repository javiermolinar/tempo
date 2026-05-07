package minhash

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeFromTrace simulates what would happen at block flush time.
// It takes a trace-like structure (service name → span names) and computes MinHash bands.
// This validates the integration pattern without importing vparquet5.
func TestComputeFromTrace(t *testing.T) {
	tests := []struct {
		name      string
		spansA    []traceSpan
		spansB    []traceSpan
		wantMatch bool
		wantMinJ  float64 // minimum expected estimated Jaccard
	}{
		{
			name: "identical checkout traces",
			spansA: []traceSpan{
				{"checkout", "POST /cart"},
				{"checkout", "validateCart"},
				{"payment", "charge"},
				{"inventory", "checkStock"},
			},
			spansB: []traceSpan{
				{"checkout", "POST /cart"},
				{"checkout", "validateCart"},
				{"payment", "charge"},
				{"inventory", "checkStock"},
			},
			wantMatch: true,
			wantMinJ:  1.0,
		},
		{
			name: "completely different flows",
			spansA: []traceSpan{
				{"checkout", "POST /cart"},
				{"payment", "charge"},
			},
			spansB: []traceSpan{
				{"auth", "POST /login"},
				{"session", "GET /session"},
			},
			wantMatch: false,
			wantMinJ:  0.0,
		},
		{
			name: "duplicate spans within trace",
			spansA: []traceSpan{
				{"checkout", "POST /cart"},
				{"checkout", "POST /cart"}, // duplicate
				{"checkout", "POST /cart"}, // duplicate
				{"payment", "charge"},
			},
			spansB: []traceSpan{
				{"checkout", "POST /cart"},
				{"payment", "charge"},
			},
			wantMatch: true,
			wantMinJ:  1.0, // duplicates don't change the signature set
		},
		{
			name: "one additional service",
			spansA: []traceSpan{
				{"svc-a", "op-1"},
				{"svc-b", "op-2"},
				{"svc-c", "op-3"},
				{"svc-d", "op-4"},
				{"svc-e", "op-5"},
			},
			spansB: []traceSpan{
				{"svc-a", "op-1"},
				{"svc-b", "op-2"},
				{"svc-c", "op-3"},
				{"svc-d", "op-4"},
				{"svc-e", "op-5"},
				{"svc-f", "op-6"}, // one extra
			},
			wantMatch: true,
			wantMinJ:  0.5, // J=5/6=0.833, estimated should be above 0.5
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate extracting signatures from trace spans (same as flush-time code would do)
			sigsA := extractSignaturesFromSpans(tt.spansA)
			sigsB := extractSignaturesFromSpans(tt.spansB)

			sigA := Compute(sigsA)
			sigB := Compute(sigsB)

			bandsA := sigA.ToBands()
			bandsB := sigB.ToBands()

			estJ := EstimateJaccard(sigA, sigB)
			anyMatch := AnyBandMatches(bandsA, bandsB)

			t.Logf("Signatures A: %v", sigsA)
			t.Logf("Signatures B: %v", sigsB)
			t.Logf("Estimated Jaccard: %.3f, Bands: %v vs %v, AnyMatch: %v",
				estJ, bandsA, bandsB, anyMatch)

			if tt.wantMatch {
				assert.True(t, anyMatch, "expected at least one band to match")
			} else {
				assert.False(t, anyMatch, "expected no bands to match")
			}
			require.GreaterOrEqual(t, estJ, tt.wantMinJ,
				"estimated Jaccard should be >= %.2f", tt.wantMinJ)

			// Verify bands are stored as 4 uint64 values (simulates parquet write/read)
			var stored [B]uint64
			stored = bandsA
			var loaded Bands
			for i := range stored {
				loaded[i] = stored[i]
			}
			assert.Equal(t, bandsA, loaded, "round-trip through uint64 storage should be lossless")
		})
	}
}

// TestBandPredicateFilter simulates what TraceQL predicate pushdown would do.
// It creates a "block" of traces, computes bands, then filters by matching
// any band against a reference trace's bands.
func TestBandPredicateFilter(t *testing.T) {
	type traceRecord struct {
		name  string
		spans []traceSpan
	}

	// The reference trace (user's "bad" trace)
	reference := traceRecord{
		name: "slow-checkout",
		spans: []traceSpan{
			{"checkout", "POST /cart"},
			{"checkout", "validateCart"},
			{"payment", "charge"},
			{"inventory", "checkStock"},
			{"shipping", "estimateDelivery"},
		},
	}

	// A "block" of traces
	block := []traceRecord{
		{
			name: "identical-checkout",
			spans: []traceSpan{
				{"checkout", "POST /cart"},
				{"checkout", "validateCart"},
				{"payment", "charge"},
				{"inventory", "checkStock"},
				{"shipping", "estimateDelivery"},
			},
		},
		{
			name: "similar-checkout-extra-retry",
			spans: []traceSpan{
				{"checkout", "POST /cart"},
				{"checkout", "validateCart"},
				{"payment", "charge"},
				{"payment", "retryCharge"}, // extra
				{"inventory", "checkStock"},
				{"shipping", "estimateDelivery"},
			},
		},
		{
			name: "different-flow-auth",
			spans: []traceSpan{
				{"auth", "POST /login"},
				{"session", "createSession"},
				{"user-db", "SELECT user"},
			},
		},
		{
			name: "different-flow-search",
			spans: []traceSpan{
				{"search", "GET /search"},
				{"catalog", "queryProducts"},
				{"cache", "GET products"},
			},
		},
		{
			name: "partial-overlap-checkout",
			spans: []traceSpan{
				{"checkout", "POST /cart"},
				{"payment", "charge"},
				// missing: validateCart, checkStock, estimateDelivery
				// plus new operations:
				{"analytics", "trackPurchase"},
				{"notification", "sendEmail"},
			},
		},
	}

	// Compute reference bands
	refSigs := extractSignaturesFromSpans(reference.spans)
	refBands := Compute(refSigs).ToBands()

	// Simulate parquet predicate: "minHashBand0 = X || minHashBand1 = Y || ..."
	var passed []string
	var filtered []string

	for _, tr := range block {
		sigs := extractSignaturesFromSpans(tr.spans)
		bands := Compute(sigs).ToBands()

		if AnyBandMatches(refBands, bands) {
			passed = append(passed, tr.name)
		} else {
			filtered = append(filtered, tr.name)
		}
	}

	t.Logf("Reference: %s (sigs=%v)", reference.name, refSigs)
	t.Logf("Passed filter: %v", passed)
	t.Logf("Filtered out:  %v", filtered)

	// Identical must always pass
	assert.Contains(t, passed, "identical-checkout")

	// Different flows must always be filtered
	assert.Contains(t, filtered, "different-flow-auth")
	assert.Contains(t, filtered, "different-flow-search")

	// Similar with one extra operation should usually pass
	// (J=5/6=0.833, P(match)≈99% with B=4,R=2)
	assert.Contains(t, passed, "similar-checkout-extra-retry",
		"trace with one extra operation should pass the band filter")
}

type traceSpan struct {
	serviceName string
	spanName    string
}

func extractSignaturesFromSpans(spans []traceSpan) []string {
	seen := make(map[string]struct{})
	for _, s := range spans {
		sig := fmt.Sprintf("%s|%s", s.serviceName, s.spanName)
		seen[sig] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for sig := range seen {
		result = append(result, sig)
	}
	return result
}
