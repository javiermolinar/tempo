package minhash_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	pq "github.com/parquet-go/parquet-go"

	"github.com/grafana/tempo/pkg/drain"
	"github.com/grafana/tempo/pkg/minhash"
	vparquet4 "github.com/grafana/tempo/tempodb/encoding/vparquet4"
	vparquet5 "github.com/grafana/tempo/tempodb/encoding/vparquet5"
)

type compactionBlockStats struct {
	blockID          string
	compactionLevel  int
	totalTraces      int
	uniqueShapes     int
	multiSigShapes   int
	singletonShapes  int
	sigSizeP50       int
	sigSizeP90       int
	sigSizeMax       int
	spansPerTraceP50 int
	spansPerTraceP90 int
	spansPerTraceMax int
	bandMatchPairs   int
	totalPairs       int
	badMatchesJ01    int
	badMatchesJ02    int
	goodMatchesJ07   int
	worstJ           float64
}

// TestCompactionImpact compares signature quality across blocks with different
// compaction levels. Higher compaction = more span fragments merged = richer signatures.
//
// Set TEMPO_BLOCKS_DIR to a directory containing block subdirectories.
// Run: go test ./pkg/minhash/ -run TestCompactionImpact -v -count=1 -timeout 600s
func TestCompactionImpact(t *testing.T) {
	dirs := []string{}

	// Collect from both tenants
	for _, base := range []string{
		os.Getenv("TEMPO_BLOCKS_DIR"),
		"/tmp/tempo-block",
		"/tmp/tempo-block-1026056",
	} {
		if base == "" {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(base, e.Name()))
			}
		}
	}

	if len(dirs) == 0 {
		t.Skip("No block directories found")
	}

	var allStats []compactionBlockStats

	for _, blockPath := range dirs {
		parquetPath := filepath.Join(blockPath, "data.parquet")
		fi, err := os.Stat(parquetPath)
		if err != nil {
			continue
		}

		// Read meta
		var meta struct {
			BlockID         string `json:"blockID"`
			CompactionLevel int    `json:"compactionLevel"`
			Format          string `json:"format"`
		}
		metaBytes, _ := os.ReadFile(filepath.Join(blockPath, "meta.json"))
		_ = json.Unmarshal(metaBytes, &meta)

		f, err := os.Open(parquetPath)
		if err != nil {
			continue
		}

		pf, err := pq.OpenFile(f, fi.Size())
		if err != nil {
			f.Close()
			continue
		}

		isV4 := strings.Contains(meta.Format, "vParquet4")
		var schema *pq.Schema
		if isV4 {
			schema = pq.SchemaOf(new(vparquet4.Trace))
		} else {
			schema = pq.SchemaOf(new(vparquet5.Trace))
		}

		// Read traces with stateless normalization
		type cohortInfo struct {
			sigSet map[string]struct{}
			sigs   []string
			bands  minhash.Bands
			mhSig  minhash.Signature
			count  int
		}
		cohortMap := make(map[string]*cohortInfo)
		var sigSizes []int
		var spanCounts []int
		traceCount := 0
		maxTraces := 20000

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

					var svcSpans []struct{ svc, name string }
					var spanCount int
					var attrs []struct{ key, val string }

					if isV4 {
						tr := new(vparquet4.Trace)
						if err := schema.Reconstruct(tr, row); err != nil {
							continue
						}
						for _, rs := range tr.ResourceSpans {
							for _, ss := range rs.ScopeSpans {
								for _, s := range ss.Spans {
									spanCount++
									svcSpans = append(svcSpans, struct{ svc, name string }{rs.Resource.ServiceName, s.Name})
									for _, a := range s.Attrs {
										if len(a.Value) > 0 {
											attrs = append(attrs, struct{ key, val string }{a.Key, a.Value[0]})
										}
									}
								}
							}
						}
					} else {
						tr := new(vparquet5.Trace)
						if err := schema.Reconstruct(tr, row); err != nil {
							continue
						}
						for _, rs := range tr.ResourceSpans {
							for _, ss := range rs.ScopeSpans {
								for _, s := range ss.Spans {
									spanCount++
									svcSpans = append(svcSpans, struct{ svc, name string }{rs.Resource.ServiceName, s.Name})
									for _, a := range s.Attrs {
										if len(a.Value) > 0 {
											attrs = append(attrs, struct{ key, val string }{a.Key, a.Value[0]})
										}
									}
								}
							}
						}
					}

					if spanCount == 0 {
						continue
					}
					traceCount++
					spanCounts = append(spanCounts, spanCount)

					// Build signatures with stateless normalization
					seen := make(map[string]struct{})
					attrMap := make(map[string]string)
					for _, a := range attrs {
						attrMap[a.key] = a.val
					}

					for _, sp := range svcSpans {
						normName := normalizeStateless(sp.name)
						parts := make([]string, 0, 4)
						for _, key := range []string{"http.route",
							"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
							"messaging.system", "messaging.operation", "messaging.destination.name"} {
							if v, ok := attrMap[key]; ok {
								parts = append(parts, v)
							}
						}
						sort.Strings(parts)
						sig := sp.svc + "|" + normName
						if len(parts) > 0 {
							sig += "|" + strings.Join(parts, "|")
						}
						seen[sig] = struct{}{}
					}

					sigs := make([]string, 0, len(seen))
					for s := range seen {
						sigs = append(sigs, s)
					}
					sigSizes = append(sigSizes, len(sigs))

					sort.Strings(sigs)
					key := strings.Join(sigs, "\n")
					if _, ok := cohortMap[key]; !ok {
						sig := minhash.Compute(sigs)
						cohortMap[key] = &cohortInfo{
							sigSet: seen, sigs: sigs,
							bands: sig.ToBands(), mhSig: sig,
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
		f.Close()

		if traceCount < 10 {
			continue
		}

		// Build cohort list
		cohorts := make([]*cohortInfo, 0, len(cohortMap))
		for _, c := range cohortMap {
			cohorts = append(cohorts, c)
		}

		// Pairwise comparison
		stats := compactionBlockStats{
			blockID:         meta.BlockID[:8],
			compactionLevel: meta.CompactionLevel,
			totalTraces:     traceCount,
			uniqueShapes:    len(cohorts),
			worstJ:          1.0,
		}

		for _, c := range cohorts {
			if len(c.sigSet) > 1 {
				stats.multiSigShapes++
			}
			if c.count == 1 {
				stats.singletonShapes++
			}
		}

		sort.Ints(sigSizes)
		sort.Ints(spanCounts)
		stats.sigSizeP50 = sigSizes[len(sigSizes)/2]
		stats.sigSizeP90 = sigSizes[int(float64(len(sigSizes))*0.9)]
		stats.sigSizeMax = sigSizes[len(sigSizes)-1]
		stats.spansPerTraceP50 = spanCounts[len(spanCounts)/2]
		stats.spansPerTraceP90 = spanCounts[int(float64(len(spanCounts))*0.9)]
		stats.spansPerTraceMax = spanCounts[len(spanCounts)-1]

		for i := 0; i < len(cohorts); i++ {
			for j := i + 1; j < len(cohorts); j++ {
				stats.totalPairs++
				if !minhash.AnyBandMatches(cohorts[i].bands, cohorts[j].bands) {
					continue
				}
				stats.bandMatchPairs++

				intersection := 0
				for k := range cohorts[i].sigSet {
					if _, ok := cohorts[j].sigSet[k]; ok {
						intersection++
					}
				}
				union := len(cohorts[i].sigSet) + len(cohorts[j].sigSet) - intersection
				exactJ := float64(intersection) / float64(union)

				if exactJ < 0.1 {
					stats.badMatchesJ01++
				}
				if exactJ < 0.2 {
					stats.badMatchesJ02++
				}
				if exactJ >= 0.7 {
					stats.goodMatchesJ07++
				}
				if exactJ < stats.worstJ {
					stats.worstJ = exactJ
				}
			}
		}

		allStats = append(allStats, stats)
	}

	// Sort by compaction level
	sort.Slice(allStats, func(i, j int) bool {
		if allStats[i].compactionLevel != allStats[j].compactionLevel {
			return allStats[i].compactionLevel < allStats[j].compactionLevel
		}
		return allStats[i].blockID < allStats[j].blockID
	})

	t.Log("\n=== Compaction Level Impact on MinHash Quality ===\n")
	t.Logf("  %-10s %-7s %-7s %-8s %-8s %-8s %-8s %-8s %-8s %-7s %-7s %-7s %-8s",
		"Block", "Compact", "Traces", "Shapes", "MultiSg", "SigP50", "SigP90", "SpnP50", "SpnP90",
		"Bands", "J<0.1", "J<0.2", "WorstJ")
	t.Logf("  %s", strings.Repeat("─", 140))

	for _, s := range allStats {
		t.Logf("  %-10s %-7d %-7d %-8d %-8d %-8d %-8d %-8d %-8d %-7d %-7d %-7d %-8.3f",
			s.blockID, s.compactionLevel, s.totalTraces, s.uniqueShapes, s.multiSigShapes,
			s.sigSizeP50, s.sigSizeP90, s.spansPerTraceP50, s.spansPerTraceP90,
			s.bandMatchPairs, s.badMatchesJ01, s.badMatchesJ02, s.worstJ)
	}

	// Summary by compaction level
	t.Log("\n=== Summary by Compaction Level ===\n")
	levels := map[int][]compactionBlockStats{}
	for _, s := range allStats {
		levels[s.compactionLevel] = append(levels[s.compactionLevel], s)
	}
	sortedLevels := make([]int, 0, len(levels))
	for l := range levels {
		sortedLevels = append(sortedLevels, l)
	}
	sort.Ints(sortedLevels)

	for _, lvl := range sortedLevels {
		blocks := levels[lvl]
		totalTraces := 0
		totalBad01 := 0
		totalBad02 := 0
		totalGood07 := 0
		totalBands := 0
		avgSigP50 := 0
		avgSpnP50 := 0
		for _, b := range blocks {
			totalTraces += b.totalTraces
			totalBad01 += b.badMatchesJ01
			totalBad02 += b.badMatchesJ02
			totalGood07 += b.goodMatchesJ07
			totalBands += b.bandMatchPairs
			avgSigP50 += b.sigSizeP50
			avgSpnP50 += b.spansPerTraceP50
		}
		n := len(blocks)
		t.Logf("  Compaction %d: %d blocks, %d traces, avg sig p50=%d, avg span p50=%d, total J<0.1=%d, total J<0.2=%d, total J≥0.7=%d",
			lvl, n, totalTraces, avgSigP50/n, avgSpnP50/n, totalBad01, totalBad02, totalGood07)
	}
}

func normalizeStateless(name string) string {
	var tokenizer drain.DefaultTokenizerWrapper
	var tokens []string
	tokens = tokenizer.Tokenize(name, tokens)
	if len(tokens) == 0 {
		return name
	}
	changed := false
	for i, t := range tokens {
		if t == "<END>" {
			continue
		}
		if drain.IsLikelyData(t) {
			tokens[i] = "<_>"
			changed = true
		}
	}
	if !changed {
		return name
	}
	return tokenizer.Join(tokens)
}
