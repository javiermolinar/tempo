package minhash_test

import (
	"encoding/json"
	"fmt"
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

type blockMeta struct {
	Format       string `json:"format"`
	TotalObjects int    `json:"totalObjects"`
	StartTime    string `json:"startTime"`
	EndTime      string `json:"endTime"`
}

type blockCohort struct {
	sigKey string
	sigSet map[string]struct{}
	count  int
	bands  minhash.Bands
	sig    minhash.Signature
	// representative trace info
	rootSvc  string
	rootSpan string
}

type blockResult struct {
	path    string
	meta    blockMeta
	cohorts []*blockCohort // sorted by count desc
}

func readBlock(t *testing.T, blockPath string, maxTraces int) *blockResult {
	t.Helper()

	parquetPath := filepath.Join(blockPath, "data.parquet")
	fi, err := os.Stat(parquetPath)
	if err != nil {
		return nil
	}

	f, err := os.Open(parquetPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	pf, err := pq.OpenFile(f, fi.Size())
	if err != nil {
		return nil
	}

	var meta blockMeta
	metaBytes, _ := os.ReadFile(filepath.Join(blockPath, "meta.json"))
	_ = json.Unmarshal(metaBytes, &meta)

	isV4 := strings.Contains(meta.Format, "vParquet4")

	var schema *pq.Schema
	if isV4 {
		schema = pq.SchemaOf(new(vparquet4.Trace))
	} else {
		schema = pq.SchemaOf(new(vparquet5.Trace))
	}

	// Read traces and build cohorts
	type cohortAccum struct {
		sigSet  map[string]struct{}
		count   int
		rootSvc string
		rootSpan string
	}
	cohortMap := make(map[string]*cohortAccum)
	traceCount := 0

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

				c, ok := cohortMap[key]
				if !ok {
					c = &cohortAccum{sigSet: td.sigSet, rootSvc: td.rootSvc, rootSpan: td.rootSpan}
					cohortMap[key] = c
				}
				c.count++
			}
			if readErr != nil {
				break
			}
		}
		rows.Close()
	}

	// Build cohort list with MinHash
	cohorts := make([]*blockCohort, 0, len(cohortMap))
	for key, c := range cohortMap {
		sigs := make([]string, 0, len(c.sigSet))
		for s := range c.sigSet {
			sigs = append(sigs, s)
		}
		sig := minhash.Compute(sigs)
		cohorts = append(cohorts, &blockCohort{
			sigKey:   key,
			sigSet:   c.sigSet,
			count:    c.count,
			bands:    sig.ToBands(),
			sig:      sig,
			rootSvc:  c.rootSvc,
			rootSpan: c.rootSpan,
		})
	}
	sort.Slice(cohorts, func(i, j int) bool { return cohorts[i].count > cohorts[j].count })

	return &blockResult{path: blockPath, meta: meta, cohorts: cohorts}
}

// TestCrossBlockValidation reads multiple blocks from the same tenant and validates
// that MinHash finds the same structural shapes across time windows.
//
// Set TEMPO_BLOCKS_DIR to a directory containing block subdirectories.
// Run: go test ./pkg/minhash/ -run TestCrossBlock -v -count=1 -timeout 600s
func TestCrossBlockValidation(t *testing.T) {
	blocksDir := os.Getenv("TEMPO_BLOCKS_DIR")
	if blocksDir == "" {
		blocksDir = "/tmp/tempo-block"
	}

	entries, err := os.ReadDir(blocksDir)
	if err != nil {
		t.Skipf("Cannot read %s: %v", blocksDir, err)
	}

	var blocks []*blockResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bp := filepath.Join(blocksDir, e.Name())
		br := readBlock(t, bp, 20000)
		if br != nil && len(br.cohorts) > 0 {
			blocks = append(blocks, br)
		}
	}

	if len(blocks) < 2 {
		t.Skipf("Need at least 2 blocks, found %d", len(blocks))
	}

	// === 1: Per-block summary ===
	t.Log("\n=== Block Summary ===")
	for _, b := range blocks {
		totalTraces := 0
		for _, c := range b.cohorts {
			totalTraces += c.count
		}
		multiSig := 0
		for _, c := range b.cohorts {
			if len(c.sigSet) > 1 {
				multiSig++
			}
		}
		t.Logf("  %s", filepath.Base(b.path))
		t.Logf("    time: %s → %s", b.meta.StartTime[:19], b.meta.EndTime[:19])
		t.Logf("    traces: %d, shapes: %d, multi-sig shapes: %d",
			totalTraces, len(b.cohorts), multiSig)
		for i, c := range b.cohorts {
			if i >= 5 {
				break
			}
			t.Logf("    top[%d]: [%d traces, %d sigs] %s|%s", i, c.count, len(c.sigSet), c.rootSvc, c.rootSpan)
		}
	}

	// === 2: Cross-block cohort matching ===
	t.Log("\n=== Cross-Block Cohort Matching ===")
	t.Log("  For each cohort in block A, check if a matching cohort exists in block B")

	for i := 0; i < len(blocks); i++ {
		for j := i + 1; j < len(blocks); j++ {
			ba, bb := blocks[i], blocks[j]
			t.Logf("\n  --- %s vs %s ---", filepath.Base(ba.path)[:8], filepath.Base(bb.path)[:8])

			// Only check top cohorts (most common shapes)
			topA := ba.cohorts
			if len(topA) > 30 {
				topA = topA[:30]
			}

			exactMatches := 0
			bandMatches := 0
			noMatches := 0

			for _, ca := range topA {
				bestExactJ := 0.0
				bestBandMatch := false
				var bestCohort *blockCohort

				for _, cb := range bb.cohorts {
					// Exact signature match
					if ca.sigKey == cb.sigKey {
						bestExactJ = 1.0
						bestBandMatch = true
						bestCohort = cb
						break
					}

					// MinHash band match
					if minhash.AnyBandMatches(ca.bands, cb.bands) {
						// Compute exact Jaccard
						intersection := 0
						for k := range ca.sigSet {
							if _, ok := cb.sigSet[k]; ok {
								intersection++
							}
						}
						union := len(ca.sigSet) + len(cb.sigSet) - intersection
						j := float64(intersection) / float64(union)
						if j > bestExactJ {
							bestExactJ = j
							bestBandMatch = true
							bestCohort = cb
						}
					}
				}

				label := ""
				if bestExactJ == 1.0 {
					exactMatches++
					label = "EXACT"
				} else if bestBandMatch {
					bandMatches++
					label = fmt.Sprintf("SIMILAR J=%.2f", bestExactJ)
				} else {
					noMatches++
					label = "NOT FOUND"
				}

				if ca.count >= 10 || bestExactJ > 0 { // only show significant cohorts
					matchInfo := ""
					if bestCohort != nil && bestExactJ < 1.0 {
						matchInfo = fmt.Sprintf(" → %s|%s (%d traces)",
							bestCohort.rootSvc, bestCohort.rootSpan, bestCohort.count)
					}
					t.Logf("    [%4d traces, %2d sigs] %s|%s → %s%s",
						ca.count, len(ca.sigSet), ca.rootSvc, ca.rootSpan, label, matchInfo)
				}
			}

			t.Logf("    Summary: exact=%d band_similar=%d not_found=%d (of %d cohorts)",
				exactMatches, bandMatches, noMatches, len(topA))
		}
	}

	// === 3: Temporal stability ===
	t.Log("\n=== Temporal Stability ===")
	t.Log("  Shapes that appear in ALL blocks (stable over time)")

	// Collect all unique shapes across blocks
	type shapePresence struct {
		sigKey   string
		sigSet   map[string]struct{}
		rootSvc  string
		rootSpan string
		blocks   []int // which blocks contain this shape
		counts   []int // trace count per block
	}
	allShapes := make(map[string]*shapePresence)

	for bi, b := range blocks {
		for _, c := range b.cohorts {
			sp, ok := allShapes[c.sigKey]
			if !ok {
				sp = &shapePresence{
					sigKey: c.sigKey, sigSet: c.sigSet,
					rootSvc: c.rootSvc, rootSpan: c.rootSpan,
				}
				allShapes[c.sigKey] = sp
			}
			sp.blocks = append(sp.blocks, bi)
			sp.counts = append(sp.counts, c.count)
		}
	}

	// Sort by number of blocks present (stability)
	shapes := make([]*shapePresence, 0, len(allShapes))
	for _, sp := range allShapes {
		shapes = append(shapes, sp)
	}
	sort.Slice(shapes, func(i, j int) bool {
		if len(shapes[i].blocks) != len(shapes[j].blocks) {
			return len(shapes[i].blocks) > len(shapes[j].blocks)
		}
		totalI, totalJ := 0, 0
		for _, c := range shapes[i].counts {
			totalI += c
		}
		for _, c := range shapes[j].counts {
			totalJ += c
		}
		return totalI > totalJ
	})

	stableCount := 0
	for _, sp := range shapes {
		if len(sp.blocks) == len(blocks) {
			stableCount++
		}
	}
	t.Logf("  Total unique shapes: %d", len(shapes))
	t.Logf("  Present in ALL %d blocks: %d shapes", len(blocks), stableCount)
	t.Log("  Most stable shapes:")
	for i, sp := range shapes {
		if i >= 20 {
			break
		}
		t.Logf("    [%d/%d blocks, counts=%v, %d sigs] %s|%s",
			len(sp.blocks), len(blocks), sp.counts, len(sp.sigSet), sp.rootSvc, sp.rootSpan)
	}
}
