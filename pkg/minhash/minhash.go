// Package minhash computes MinHash signatures for trace structural similarity.
//
// A trace's structure is defined by its operation signature set: the set of
// "service.name|span.name" pairs across all spans. MinHash approximates the
// Jaccard similarity between two such sets using a compact fixed-size signature.
//
// The signature is split into B bands of R rows each. Two traces are considered
// "similar" if at least one band matches. This enables storage-level filtering
// via parquet column predicates.
package minhash

import (
	"encoding/binary"
	"math"
	"math/rand"

	"github.com/cespare/xxhash/v2"
)

const (
	// K is the total number of hash functions.
	K = 8

	// B is the number of bands.
	B = 4

	// R is the number of rows per band (K/B).
	R = 2
)

// seeds are fixed random values used to create K independent hash functions.
// These must never change once blocks are written — they are part of the format.
var seeds [K]uint64

func init() {
	rng := rand.New(rand.NewSource(0x4d696e48617368)) // "MinHash" as hex
	for i := range seeds {
		seeds[i] = rng.Uint64()
	}
}

// Signature holds the K minimum hash values for a signature set.
type Signature [K]uint64

// Bands holds the B band hashes derived from a Signature.
type Bands [B]uint64

// Compute returns a MinHash signature for the given set of operation signatures.
// Each signature should be a "service.name|span.name" string.
func Compute(signatures []string) Signature {
	var sig Signature
	for i := range sig {
		sig[i] = math.MaxUint64
	}

	for _, s := range signatures {
		b := []byte(s)
		for i := 0; i < K; i++ {
			h := xxhash.New()
			var seedBuf [8]byte
			binary.LittleEndian.PutUint64(seedBuf[:], seeds[i])
			_, _ = h.Write(seedBuf[:])
			_, _ = h.Write(b)
			v := h.Sum64()
			if v < sig[i] {
				sig[i] = v
			}
		}
	}

	return sig
}

// ComputeFromSet is like Compute but accepts a map for callers that already have a set.
func ComputeFromSet(signatures map[string]struct{}) Signature {
	var sig Signature
	for i := range sig {
		sig[i] = math.MaxUint64
	}

	for s := range signatures {
		b := []byte(s)
		for i := 0; i < K; i++ {
			h := xxhash.New()
			var seedBuf [8]byte
			binary.LittleEndian.PutUint64(seedBuf[:], seeds[i])
			_, _ = h.Write(seedBuf[:])
			_, _ = h.Write(b)
			v := h.Sum64()
			if v < sig[i] {
				sig[i] = v
			}
		}
	}

	return sig
}

// ToBands computes B band hashes from a MinHash signature.
// Each band hashes R consecutive values into a single uint64.
func (s Signature) ToBands() Bands {
	var bands Bands
	for b := 0; b < B; b++ {
		start := b * R
		h := xxhash.New()
		var buf [8]byte
		for j := start; j < start+R; j++ {
			binary.LittleEndian.PutUint64(buf[:], s[j])
			_, _ = h.Write(buf[:])
		}
		bands[b] = h.Sum64()
	}
	return bands
}

// EstimateJaccard estimates the Jaccard similarity between two signatures
// by counting the fraction of matching hash slots.
func EstimateJaccard(a, b Signature) float64 {
	matches := 0
	for i := range a {
		if a[i] == b[i] {
			matches++
		}
	}
	return float64(matches) / float64(K)
}

// AnyBandMatches returns true if at least one band hash matches between a and b.
// This is the filter predicate used at the storage level.
func AnyBandMatches(a, b Bands) bool {
	for i := range a {
		if a[i] == b[i] {
			return true
		}
	}
	return false
}

// MatchingBands returns the number of bands that match between a and b.
func MatchingBands(a, b Bands) int {
	count := 0
	for i := range a {
		if a[i] == b[i] {
			count++
		}
	}
	return count
}
