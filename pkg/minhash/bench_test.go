package minhash

import (
	"fmt"
	"testing"
)

// BenchmarkCompute measures the cost of computing MinHash for traces of various sizes.
// This simulates the per-trace overhead at block flush time.
func BenchmarkCompute(b *testing.B) {
	// Realistic trace sizes: small (5 sigs), medium (15), large (50), very large (100)
	for _, n := range []int{5, 15, 30, 50, 100} {
		sigs := make([]string, n)
		for i := range sigs {
			sigs[i] = fmt.Sprintf("service-%d|operation-%d", i%10, i)
		}

		b.Run(fmt.Sprintf("sigs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Compute(sigs)
			}
		})
	}
}

// BenchmarkComputeAndBands measures Compute + ToBands together (full flush-time work).
func BenchmarkComputeAndBands(b *testing.B) {
	for _, n := range []int{5, 15, 30, 50, 100} {
		sigs := make([]string, n)
		for i := range sigs {
			sigs[i] = fmt.Sprintf("service-%d|operation-%d", i%10, i)
		}

		b.Run(fmt.Sprintf("sigs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sig := Compute(sigs)
				_ = sig.ToBands()
			}
		})
	}
}

// BenchmarkAnyBandMatches measures the filter check (query-time hot path).
func BenchmarkAnyBandMatches(b *testing.B) {
	sigsA := make([]string, 15)
	sigsB := make([]string, 15)
	for i := range sigsA {
		sigsA[i] = fmt.Sprintf("service-%d|operation-%d", i%10, i)
		sigsB[i] = fmt.Sprintf("service-%d|operation-%d", i%10, i)
	}
	bandsA := Compute(sigsA).ToBands()
	bandsB := Compute(sigsB).ToBands()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = AnyBandMatches(bandsA, bandsB)
	}
}
