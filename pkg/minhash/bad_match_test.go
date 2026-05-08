package minhash_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	pq "github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/pkg/minhash"
	vparquet4 "github.com/grafana/tempo/tempodb/encoding/vparquet4"
	vparquet5 "github.com/grafana/tempo/tempodb/encoding/vparquet5"
)

// TestBadMatches exhaustively searches for the worst MinHash band matches —
// traces that match on bands but have low Jaccard similarity.
//
// A "bad match" is one where similar_to() would pass a trace that shares
// very little structure with the reference. These are the cases that would
// make the feature return garbage.
//
// Run: TEMPO_BLOCK_PATH=/tmp/tempo-block/... go test ./pkg/minhash/ -run TestBadMatches -v -timeout 600s
func TestBadMatches(t *testing.T) {
	blockPath := os.Getenv("TEMPO_BLOCK_PATH")
	if blockPath == "" {
		blockPath = "/tmp/tempo-block/0005fda8-8756-47d5-827b-d5d24ad08cfc"
	}

	parquetPath := filepath.Join(blockPath, "data.parquet")
	fi, err := os.Stat(parquetPath)
	if err != nil {
		t.Skipf("Block not found at %s: %v", blockPath, err)
	}

	f, err := os.Open(parquetPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	pf, err := pq.OpenFile(f, fi.Size())
	if err != nil {
		t.Fatalf("open parquet: %v", err)
	}

	metaBytes, _ := os.ReadFile(filepath.Join(blockPath, "meta.json"))
	isV4 := strings.Contains(string(metaBytes), "vParquet4")

	var schema *pq.Schema
	if isV4 {
		schema = pq.SchemaOf(new(vparquet4.Trace))
	} else {
		schema = pq.SchemaOf(new(vparquet5.Trace))
	}

	// Read ALL traces (up to limit)
	type cohortInfo struct {
		sigSet   map[string]struct{}
		sigs     []string
		bands    minhash.Bands
		mhSig    minhash.Signature
		count    int
		rootSvc  string
		rootSpan string
	}

	cohortMap := make(map[string]*cohortInfo)
	traceCount := 0
	maxTraces := 50000

	for _, rg := range pf.RowGroups() {
		rows := rg.Rows()
		buf := make([]pq.Row, 64)
		for {
			n, readErr := rows.ReadRows(buf)
			if n == 0 || traceCount >= maxTraces {
				break
			}
			for _, row := range buf[:n] {
				if traceCount >= maxTraces {
					break
				}
				var td *traceData
				if isV4 {
					tr := new(vparquet4.Trace)
					if err := schema.Reconstruct(tr, row); err != nil {
						continue
					}
					td = extractFromV4(tr)
				} else {
					tr := new(vparquet5.Trace)
					if err := schema.Reconstruct(tr, row); err != nil {
						continue
					}
					td = extractFromV5(tr)
				}
				if len(td.sigs) == 0 {
					continue
				}
				traceCount++

				sorted := make([]string, len(td.sigs))
				copy(sorted, td.sigs)
				sort.Strings(sorted)
				key := strings.Join(sorted, "\n")

				if _, ok := cohortMap[key]; !ok {
					sig := minhash.Compute(td.sigs)
					cohortMap[key] = &cohortInfo{
						sigSet:   td.sigSet,
						sigs:     td.sigs,
						bands:    sig.ToBands(),
						mhSig:    sig,
						rootSvc:  td.rootSvc,
						rootSpan: td.rootSpan,
					}
				}
				cohortMap[key].count++
			}
			if readErr != nil {
				break
			}
		}
		rows.Close()
	}

	cohorts := make([]*cohortInfo, 0, len(cohortMap))
	for _, c := range cohortMap {
		cohorts = append(cohorts, c)
	}
	sort.Slice(cohorts, func(i, j int) bool { return cohorts[i].count > cohorts[j].count })

	t.Logf("Traces: %d, Unique shapes: %d", traceCount, len(cohorts))

	// Exhaustive pairwise comparison of ALL cohorts
	type badMatch struct {
		i, j    int
		exactJ  float64
		estJ    float64
		sigsI   int
		sigsJ   int
		shared  []string
		onlyI   []string
		onlyJ   []string
		svcI    string
		svcJ    string
		spanI   string
		spanJ   string
	}

	var badMatches []badMatch
	totalPairs := 0
	totalBandMatches := 0

	for i := 0; i < len(cohorts); i++ {
		for j := i + 1; j < len(cohorts); j++ {
			totalPairs++
			if !minhash.AnyBandMatches(cohorts[i].bands, cohorts[j].bands) {
				continue
			}
			totalBandMatches++

			// Compute exact Jaccard
			intersection := 0
			shared := make([]string, 0)
			for k := range cohorts[i].sigSet {
				if _, ok := cohorts[j].sigSet[k]; ok {
					intersection++
					shared = append(shared, k)
				}
			}
			union := len(cohorts[i].sigSet) + len(cohorts[j].sigSet) - intersection
			exactJ := 0.0
			if union > 0 {
				exactJ = float64(intersection) / float64(union)
			}

			onlyI := make([]string, 0)
			for k := range cohorts[i].sigSet {
				if _, ok := cohorts[j].sigSet[k]; !ok {
					onlyI = append(onlyI, k)
				}
			}
			onlyJ := make([]string, 0)
			for k := range cohorts[j].sigSet {
				if _, ok := cohorts[i].sigSet[k]; !ok {
					onlyJ = append(onlyJ, k)
				}
			}

			badMatches = append(badMatches, badMatch{
				i: i, j: j,
				exactJ: exactJ,
				estJ:   minhash.EstimateJaccard(cohorts[i].mhSig, cohorts[j].mhSig),
				sigsI:  len(cohorts[i].sigSet),
				sigsJ:  len(cohorts[j].sigSet),
				shared: shared,
				onlyI:  onlyI,
				onlyJ:  onlyJ,
				svcI:   cohorts[i].rootSvc,
				svcJ:   cohorts[j].rootSvc,
				spanI:  cohorts[i].rootSpan,
				spanJ:  cohorts[j].rootSpan,
			})
		}
	}

	// Sort by Jaccard ascending (worst matches first)
	sort.Slice(badMatches, func(i, j int) bool {
		return badMatches[i].exactJ < badMatches[j].exactJ
	})

	t.Logf("\nTotal cohort pairs: %d", totalPairs)
	t.Logf("Band matches: %d (%.2f%%)", totalBandMatches, float64(totalBandMatches)/float64(totalPairs)*100)

	// === Report: worst matches ===
	t.Log("\n=== Worst Band Matches (lowest Jaccard) ===")
	t.Log("These are cases where similar_to() would PASS but the traces share little structure.\n")

	shown := 0
	for _, bm := range badMatches {
		if shown >= 20 {
			break
		}
		shown++

		sort.Strings(bm.shared)
		sort.Strings(bm.onlyI)
		sort.Strings(bm.onlyJ)

		t.Logf("  #%d: Jaccard=%.3f (est=%.2f)", shown, bm.exactJ, bm.estJ)
		t.Logf("    A: [%d traces, %d sigs] %s|%s", cohorts[bm.i].count, bm.sigsI, bm.svcI, bm.spanI)
		t.Logf("    B: [%d traces, %d sigs] %s|%s", cohorts[bm.j].count, bm.sigsJ, bm.svcJ, bm.spanJ)
		if len(bm.shared) > 0 {
			t.Logf("    Shared: %v", bm.shared)
		} else {
			t.Logf("    Shared: NONE")
		}
		if len(bm.onlyI) <= 5 {
			t.Logf("    Only A: %v", bm.onlyI)
		} else {
			t.Logf("    Only A: %v ... +%d more", bm.onlyI[:3], len(bm.onlyI)-3)
		}
		if len(bm.onlyJ) <= 5 {
			t.Logf("    Only B: %v", bm.onlyJ)
		} else {
			t.Logf("    Only B: %v ... +%d more", bm.onlyJ[:3], len(bm.onlyJ)-3)
		}
		t.Log()
	}

	// === Distribution of matched Jaccards ===
	t.Log("=== Jaccard Distribution of ALL Band Matches ===")
	if len(badMatches) > 0 {
		buckets := [][2]float64{{0, 0.1}, {0.1, 0.2}, {0.2, 0.3}, {0.3, 0.4}, {0.4, 0.5},
			{0.5, 0.6}, {0.6, 0.7}, {0.7, 0.8}, {0.8, 0.9}, {0.9, 1.01}}
		for _, b := range buckets {
			count := 0
			for _, bm := range badMatches {
				if bm.exactJ >= b[0] && bm.exactJ < b[1] {
					count++
				}
			}
			bar := strings.Repeat("█", count)
			t.Logf("  [%.1f-%.1f) %3d %s", b[0], b[1], count, bar)
		}
	}

	// === Specific concern: single-sig collisions ===
	t.Log("\n=== Single-Signature Collisions ===")
	t.Log("  Cohorts with 1 signature that match on bands with a different single-sig cohort\n")

	singleCollisions := 0
	for _, bm := range badMatches {
		if bm.sigsI == 1 && bm.sigsJ == 1 && bm.exactJ == 0 {
			singleCollisions++
			if singleCollisions <= 10 {
				t.Logf("  COLLISION: %s|%s ↔ %s|%s",
					bm.svcI, bm.spanI, bm.svcJ, bm.spanJ)
			}
		}
	}
	t.Logf("  Total single-sig zero-Jaccard collisions: %d", singleCollisions)
}
