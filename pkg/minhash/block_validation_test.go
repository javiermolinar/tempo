package minhash_test

import (
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

var semanticAttrKeys = map[string]bool{
	"http.route": true, "http.request.method": true, "http.method": true,
	"rpc.service": true, "rpc.method": true,
	"db.system": true, "db.operation": true, "db.name": true,
	"messaging.system": true, "messaging.operation": true,
	"messaging.destination.name": true, "messaging.destination": true,
}

type traceData struct {
	traceID   string
	rootSvc   string
	rootSpan  string
	spanCount int
	sigs      []string
	sigSet    map[string]struct{}
}

func buildSig(svcName, spanName string, attrs interface{ GetAttrs() []struct{ Key string; Value []string } }) string {
	// We can't use a generic interface easily here, so signature building is inlined per version
	return svcName + "|" + spanName
}

func extractFromV4(tr *vparquet4.Trace) *traceData {
	seen := make(map[string]struct{})
	spanCount := 0
	for _, rs := range tr.ResourceSpans {
		svcName := rs.Resource.ServiceName
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				spanCount++
				parts := make([]string, 0, 4)
				for _, a := range s.Attrs {
					if semanticAttrKeys[a.Key] && len(a.Value) > 0 {
						parts = append(parts, a.Value[0])
					}
				}
				sort.Strings(parts)
				sig := svcName + "|" + s.Name
				if len(parts) > 0 {
					sig += "|" + strings.Join(parts, "|")
				}
				seen[sig] = struct{}{}
			}
		}
	}
	sigs := make([]string, 0, len(seen))
	for s := range seen {
		sigs = append(sigs, s)
	}
	return &traceData{
		traceID: fmt.Sprintf("%x", tr.TraceID), rootSvc: tr.RootServiceName,
		rootSpan: tr.RootSpanName, spanCount: spanCount, sigs: sigs, sigSet: seen,
	}
}

func extractFromV5(tr *vparquet5.Trace) *traceData {
	seen := make(map[string]struct{})
	spanCount := 0
	for _, rs := range tr.ResourceSpans {
		svcName := rs.Resource.ServiceName
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				spanCount++
				parts := make([]string, 0, 4)
				for _, a := range s.Attrs {
					if semanticAttrKeys[a.Key] && len(a.Value) > 0 {
						parts = append(parts, a.Value[0])
					}
				}
				sort.Strings(parts)
				sig := svcName + "|" + s.Name
				if len(parts) > 0 {
					sig += "|" + strings.Join(parts, "|")
				}
				seen[sig] = struct{}{}
			}
		}
	}
	sigs := make([]string, 0, len(seen))
	for s := range seen {
		sigs = append(sigs, s)
	}
	return &traceData{
		traceID: fmt.Sprintf("%x", tr.TraceID), rootSvc: tr.RootServiceName,
		rootSpan: tr.RootSpanName, spanCount: spanCount, sigs: sigs, sigSet: seen,
	}
}

// TestBlockValidation reads a real Tempo parquet block and validates MinHash on production data.
//
// Set TEMPO_BLOCK_PATH to a block directory (containing data.parquet + meta.json).
// Run: go test ./pkg/minhash/ -run TestBlockValidation -v -count=1 -timeout 300s
func TestBlockValidation(t *testing.T) {
	blockPath := os.Getenv("TEMPO_BLOCK_PATH")
	if blockPath == "" {
		blockPath = "/tmp/tempo-block/0005fda8-8756-47d5-827b-d5d24ad08cfc"
	}

	parquetPath := filepath.Join(blockPath, "data.parquet")
	fi, err := os.Stat(parquetPath)
	if err != nil {
		t.Skipf("Block not found at %s (set TEMPO_BLOCK_PATH): %v", blockPath, err)
	}
	t.Logf("Reading block: %s (%.1f MB)", parquetPath, float64(fi.Size())/(1024*1024))

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

	// Use the Go-struct-derived schema for Reconstruct, not the file schema.
	// The file schema may have different encoding for maps (key_value groups)
	// that don't match the raw file schema's reconstruct logic.
	var schema *pq.Schema
	if isV4 {
		schema = pq.SchemaOf(new(vparquet4.Trace))
	} else {
		schema = pq.SchemaOf(new(vparquet5.Trace))
	}

	t.Logf("Format: vParquet4=%v, RowGroups: %d", isV4, len(pf.RowGroups()))

	// --- Read traces ---
	var traces []*traceData
	maxTraces := 10000
	errors := 0

	for _, rg := range pf.RowGroups() {
		rows := rg.Rows()
		buf := make([]pq.Row, 64)
		for {
			n, readErr := rows.ReadRows(buf)
			if n == 0 || len(traces) >= maxTraces {
				break
			}
			for _, row := range buf[:n] {
				if len(traces) >= maxTraces {
					break
				}
				var td *traceData
				if isV4 {
					tr := new(vparquet4.Trace)
					if err := schema.Reconstruct(tr, row); err != nil {
						errors++
						continue
					}
					td = extractFromV4(tr)
				} else {
					tr := new(vparquet5.Trace)
					if err := schema.Reconstruct(tr, row); err != nil {
						errors++
						continue
					}
					td = extractFromV5(tr)
				}
				if len(td.sigs) > 0 {
					traces = append(traces, td)
				}
			}
			if readErr != nil {
				break
			}
		}
		rows.Close()
	}

	t.Logf("Read %d traces (%d decode errors)", len(traces), errors)
	if len(traces) < 10 {
		t.Fatalf("Too few traces (%d)", len(traces))
	}

	// === 1: Signature statistics ===
	t.Log("\n=== Signature Set Statistics ===")
	sigSizes := make([]int, len(traces))
	spanCounts := make([]int, len(traces))
	for i, tr := range traces {
		sigSizes[i] = len(tr.sigSet)
		spanCounts[i] = tr.spanCount
	}
	sort.Ints(sigSizes)
	sort.Ints(spanCounts)
	t.Logf("  Traces: %d", len(traces))
	t.Logf("  Spans/trace:  min=%d p50=%d p90=%d max=%d",
		spanCounts[0], spanCounts[len(spanCounts)/2], spanCounts[int(float64(len(spanCounts))*0.9)], spanCounts[len(spanCounts)-1])
	t.Logf("  Sigs/trace:   min=%d p50=%d p90=%d max=%d",
		sigSizes[0], sigSizes[len(sigSizes)/2], sigSizes[int(float64(len(sigSizes))*0.9)], sigSizes[len(sigSizes)-1])

	// === 2: Structural cohorts ===
	t.Log("\n=== Structural Cohorts ===")
	type cohort struct {
		sigSet  map[string]struct{}
		indices []int
	}
	cohortMap := make(map[string]*cohort)
	for i, tr := range traces {
		sorted := make([]string, len(tr.sigs))
		copy(sorted, tr.sigs)
		sort.Strings(sorted)
		key := strings.Join(sorted, "\n")
		c, ok := cohortMap[key]
		if !ok {
			c = &cohort{sigSet: tr.sigSet}
			cohortMap[key] = c
		}
		c.indices = append(c.indices, i)
	}
	cohorts := make([]*cohort, 0, len(cohortMap))
	for _, c := range cohortMap {
		cohorts = append(cohorts, c)
	}
	sort.Slice(cohorts, func(i, j int) bool { return len(cohorts[i].indices) > len(cohorts[j].indices) })

	singletons := 0
	for _, c := range cohorts {
		if len(c.indices) == 1 {
			singletons++
		}
	}
	t.Logf("  Unique shapes: %d", len(cohorts))
	t.Logf("  Singletons: %d (%.1f%%)", singletons, float64(singletons)/float64(len(cohorts))*100)
	t.Log("  Top cohorts:")
	for i, c := range cohorts {
		if i >= 15 {
			break
		}
		tr := traces[c.indices[0]]
		t.Logf("    [%4d traces, %2d sigs] svc=%s op=%s", len(c.indices), len(c.sigSet), tr.rootSvc, tr.rootSpan)
		sorted := make([]string, len(tr.sigs))
		copy(sorted, tr.sigs)
		sort.Strings(sorted)
		for j, s := range sorted {
			if j >= 3 {
				t.Logf("      ... +%d more", len(sorted)-3)
				break
			}
			t.Logf("      %s", s)
		}
	}

	// === 3: MinHash bands ===
	t.Log("\n=== MinHash Matching ===")
	bandsByIdx := make([]minhash.Bands, len(traces))
	sigsByIdx := make([]minhash.Signature, len(traces))
	for i, tr := range traces {
		sig := minhash.Compute(tr.sigs)
		sigsByIdx[i] = sig
		bandsByIdx[i] = sig.ToBands()
	}

	// Within-cohort: same shape → must match
	withinTotal, withinMatch := 0, 0
	for _, c := range cohorts {
		if len(c.indices) < 2 {
			continue
		}
		limit := len(c.indices)
		if limit > 50 {
			limit = 50
		}
		for i := 0; i < limit; i++ {
			for j := i + 1; j < limit; j++ {
				withinTotal++
				if minhash.AnyBandMatches(bandsByIdx[c.indices[i]], bandsByIdx[c.indices[j]]) {
					withinMatch++
				}
			}
		}
	}
	if withinTotal > 0 {
		t.Logf("  Within-cohort: %d/%d match (%.1f%%)", withinMatch, withinTotal,
			float64(withinMatch)/float64(withinTotal)*100)
	}

	// Cross-cohort: different shapes → mostly should NOT match
	topN := 30
	if topN > len(cohorts) {
		topN = len(cohorts)
	}
	crossTotal, crossMatch := 0, 0
	for i := 0; i < topN; i++ {
		for j := i + 1; j < topN; j++ {
			ri, rj := cohorts[i].indices[0], cohorts[j].indices[0]
			crossTotal++
			if minhash.AnyBandMatches(bandsByIdx[ri], bandsByIdx[rj]) {
				crossMatch++
				estJ := minhash.EstimateJaccard(sigsByIdx[ri], sigsByIdx[rj])
				intersection := 0
				for k := range cohorts[i].sigSet {
					if _, ok := cohorts[j].sigSet[k]; ok {
						intersection++
					}
				}
				union := len(cohorts[i].sigSet) + len(cohorts[j].sigSet) - intersection
				exactJ := float64(intersection) / float64(union)
				t.Logf("  CROSS-MATCH: cohort[%d](%dt) vs cohort[%d](%dt) J=%.3f estJ=%.2f",
					i, len(cohorts[i].indices), j, len(cohorts[j].indices), exactJ, estJ)
			}
		}
	}
	t.Logf("  Cross-cohort: %d/%d match (%.1f%% false positive rate)",
		crossMatch, crossTotal, float64(crossMatch)/float64(crossTotal)*100)

	// === 4: Jaccard distribution ===
	t.Log("\n=== Jaccard Distribution (top cohorts) ===")
	jaccards := make([]float64, 0, topN*topN/2)
	for i := 0; i < topN; i++ {
		for j := i + 1; j < topN; j++ {
			intersection := 0
			for k := range cohorts[i].sigSet {
				if _, ok := cohorts[j].sigSet[k]; ok {
					intersection++
				}
			}
			union := len(cohorts[i].sigSet) + len(cohorts[j].sigSet) - intersection
			if union > 0 {
				jaccards = append(jaccards, float64(intersection)/float64(union))
			}
		}
	}
	sort.Float64s(jaccards)
	if len(jaccards) > 0 {
		t.Logf("  Pairs: %d, min=%.3f p50=%.3f p90=%.3f max=%.3f",
			len(jaccards), jaccards[0], jaccards[len(jaccards)/2],
			jaccards[int(float64(len(jaccards))*0.9)], jaccards[len(jaccards)-1])

		buckets := [][2]float64{{0, 0.1}, {0.1, 0.2}, {0.2, 0.3}, {0.3, 0.4}, {0.4, 0.5},
			{0.5, 0.6}, {0.6, 0.7}, {0.7, 0.8}, {0.8, 0.9}, {0.9, 1.01}}
		for _, b := range buckets {
			count := 0
			for _, j := range jaccards {
				if j >= b[0] && j < b[1] {
					count++
				}
			}
			bar := strings.Repeat("█", count)
			if count > 60 {
				bar = strings.Repeat("█", 60) + fmt.Sprintf("+%d", count-60)
			}
			t.Logf("    [%.1f-%.1f) %4d %s", b[0], b[1], count, bar)
		}
	}
}
