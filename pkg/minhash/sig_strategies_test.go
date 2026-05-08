package minhash_test

import (
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

type rawTrace struct {
	rootSvc  string
	rootSpan string
	spans    []rawSpan
}

type rawSpan struct {
	svcName string
	info    spanInfo
}

// httpMethods is the set of standard HTTP methods that produce generic span names.
var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true, "TRACE": true,
}

// signatureStrategy defines how to build the signature set from a trace.
type signatureStrategy struct {
	name    string
	setup   func(rawTraces []rawTrace) // optional: called once before extraction
	extract func(svcName string, span spanInfo) (string, bool) // returns signature and whether to include it
}

type spanInfo struct {
	name  string
	attrs map[string]string // key → first string value
	kind  int
}

func extractSpanAttrs(attrs []vparquet4.Attribute) map[string]string {
	m := make(map[string]string)
	for _, a := range attrs {
		if len(a.Value) > 0 {
			m[a.Key] = a.Value[0]
		}
	}
	return m
}

func extractSpanAttrsV5(attrs []vparquet5.Attribute) map[string]string {
	m := make(map[string]string)
	for _, a := range attrs {
		if len(a.Value) > 0 {
			m[a.Key] = a.Value[0]
		}
	}
	return m
}

// buildDrainNormalizer creates a DRAIN instance, trains it on all span names,
// and returns a function that normalizes span names using the learned patterns.
func buildDrainNormalizer(rawTraces []rawTrace) func(string) string {
	d := drain.New("test", drain.DefaultConfig())

	// Train on all span names
	for _, rt := range rawTraces {
		for _, sp := range rt.spans {
			d.Train(sp.info.name)
		}
	}

	// Build lookup from original → normalized
	return func(name string) string {
		cluster := d.Train(name)
		if cluster == nil {
			return name
		}
		return cluster.String()
	}
}

// strategies to test
func getStrategies() []signatureStrategy {
	return []signatureStrategy{
		{
			name: "baseline (current design)",
			extract: func(svcName string, span spanInfo) (string, bool) {
				parts := make([]string, 0, 4)
				for _, key := range []string{"http.route", "http.request.method", "http.method",
					"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
					"messaging.system", "messaging.operation", "messaging.destination.name", "messaging.destination"} {
					if v, ok := span.attrs[key]; ok {
						parts = append(parts, v)
					}
				}
				sort.Strings(parts)
				sig := svcName + "|" + span.name
				if len(parts) > 0 {
					sig += "|" + strings.Join(parts, "|")
				}
				return sig, true
			},
		},
		{
			name: "drop HTTP-method-only spans",
			extract: func(svcName string, span spanInfo) (string, bool) {
				// If span.name is a bare HTTP method and no http.route, skip it
				if httpMethods[span.name] {
					if _, hasRoute := span.attrs["http.route"]; !hasRoute {
						return "", false
					}
				}
				parts := make([]string, 0, 4)
				for _, key := range []string{"http.route", "http.request.method", "http.method",
					"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
					"messaging.system", "messaging.operation", "messaging.destination.name", "messaging.destination"} {
					if v, ok := span.attrs[key]; ok {
						parts = append(parts, v)
					}
				}
				sort.Strings(parts)
				sig := svcName + "|" + span.name
				if len(parts) > 0 {
					sig += "|" + strings.Join(parts, "|")
				}
				return sig, true
			},
		},
		{
			name: "replace HTTP-method-only with 'svc|HTTP' sentinel",
			extract: func(svcName string, span spanInfo) (string, bool) {
				if httpMethods[span.name] {
					if _, hasRoute := span.attrs["http.route"]; !hasRoute {
						// Replace with a service-specific but operation-agnostic sentinel
						return svcName + "|HTTP", true
					}
				}
				parts := make([]string, 0, 4)
				for _, key := range []string{"http.route", "http.request.method", "http.method",
					"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
					"messaging.system", "messaging.operation", "messaging.destination.name", "messaging.destination"} {
					if v, ok := span.attrs[key]; ok {
						parts = append(parts, v)
					}
				}
				sort.Strings(parts)
				sig := svcName + "|" + span.name
				if len(parts) > 0 {
					sig += "|" + strings.Join(parts, "|")
				}
				return sig, true
			},
		},
		{
			name: "drop HTTP-method-only + exclude http.method from sigs",
			extract: func(svcName string, span spanInfo) (string, bool) {
				if httpMethods[span.name] {
					if _, hasRoute := span.attrs["http.route"]; !hasRoute {
						return "", false
					}
				}
				// Don't include http.method/http.request.method — it's already in span.name for well-instrumented spans
				parts := make([]string, 0, 4)
				for _, key := range []string{"http.route",
					"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
					"messaging.system", "messaging.operation", "messaging.destination.name", "messaging.destination"} {
					if v, ok := span.attrs[key]; ok {
						parts = append(parts, v)
					}
				}
				sort.Strings(parts)
				sig := svcName + "|" + span.name
				if len(parts) > 0 {
					sig += "|" + strings.Join(parts, "|")
				}
				return sig, true
			},
		},
		// --- DRAIN-based strategies ---
		func() signatureStrategy {
			var normalize func(string) string
			return signatureStrategy{
				name: "DRAIN-normalized span names",
				setup: func(rawTraces []rawTrace) {
					normalize = buildDrainNormalizer(rawTraces)
				},
				extract: func(svcName string, span spanInfo) (string, bool) {
					normName := normalize(span.name)
					parts := make([]string, 0, 4)
					for _, key := range []string{"http.route",
						"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
						"messaging.system", "messaging.operation", "messaging.destination.name", "messaging.destination"} {
						if v, ok := span.attrs[key]; ok {
							parts = append(parts, v)
						}
					}
					sort.Strings(parts)
					sig := svcName + "|" + normName
					if len(parts) > 0 {
						sig += "|" + strings.Join(parts, "|")
					}
					return sig, true
				},
			}
		}(),
		func() signatureStrategy {
			var normalize func(string) string
			return signatureStrategy{
				name: "DRAIN + drop HTTP-method-only + no http.method",
				setup: func(rawTraces []rawTrace) {
					normalize = buildDrainNormalizer(rawTraces)
				},
				extract: func(svcName string, span spanInfo) (string, bool) {
					if httpMethods[span.name] {
						if _, hasRoute := span.attrs["http.route"]; !hasRoute {
							return "", false
						}
					}
					normName := normalize(span.name)
					parts := make([]string, 0, 4)
					for _, key := range []string{"http.route",
						"rpc.service", "rpc.method", "db.system", "db.operation", "db.name",
						"messaging.system", "messaging.operation", "messaging.destination.name", "messaging.destination"} {
						if v, ok := span.attrs[key]; ok {
							parts = append(parts, v)
						}
					}
					sort.Strings(parts)
					sig := svcName + "|" + normName
					if len(parts) > 0 {
						sig += "|" + strings.Join(parts, "|")
					}
					return sig, true
				},
			}
		}(),
	}
}

type strategyResult struct {
	name            string
	totalCohorts    int
	totalPairs      int
	bandMatches     int
	badMatches      int // J < 0.2
	veryBadMatches  int // J < 0.1
	goodMatches     int // J >= 0.7
	exactMatches    int // J = 1.0
	avgBadJ         float64
	worstJ          float64
	worstA          string
	worstB          string
	worstShared     string
	droppedSpans    int
	emptyTraces     int
}

// TestSignatureStrategies compares different signature composition strategies
// against the same production block.
//
// Run: TEMPO_BLOCK_PATH=/tmp/tempo-block-1026056/... go test ./pkg/minhash/ -run TestSignatureStrategies -v -timeout 600s
func TestSignatureStrategies(t *testing.T) {
	blockPath := os.Getenv("TEMPO_BLOCK_PATH")
	if blockPath == "" {
		blockPath = "/tmp/tempo-block-1026056/0006532d-e3b7-47bc-80fc-b6048c5aed79"
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

	var rawTraces []rawTrace
	maxTraces := 50000

	for _, rg := range pf.RowGroups() {
		rows := rg.Rows()
		buf := make([]pq.Row, 64)
		for {
			n, readErr := rows.ReadRows(buf)
			if n == 0 || len(rawTraces) >= maxTraces {
				break
			}
			for _, row := range buf[:n] {
				if len(rawTraces) >= maxTraces {
					break
				}
				var rt rawTrace
				if isV4 {
					tr := new(vparquet4.Trace)
					if err := schema.Reconstruct(tr, row); err != nil {
						continue
					}
					rt.rootSvc = tr.RootServiceName
					rt.rootSpan = tr.RootSpanName
					for _, rs := range tr.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, s := range ss.Spans {
								rt.spans = append(rt.spans, rawSpan{
									svcName: rs.Resource.ServiceName,
									info:    spanInfo{name: s.Name, attrs: extractSpanAttrs(s.Attrs), kind: s.Kind},
								})
							}
						}
					}
				} else {
					tr := new(vparquet5.Trace)
					if err := schema.Reconstruct(tr, row); err != nil {
						continue
					}
					rt.rootSvc = tr.RootServiceName
					rt.rootSpan = tr.RootSpanName
					for _, rs := range tr.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, s := range ss.Spans {
								rt.spans = append(rt.spans, rawSpan{
									svcName: rs.Resource.ServiceName,
									info:    spanInfo{name: s.Name, attrs: extractSpanAttrsV5(s.Attrs), kind: s.Kind},
								})
							}
						}
					}
				}
				if len(rt.spans) > 0 {
					rawTraces = append(rawTraces, rt)
				}
			}
			if readErr != nil {
				break
			}
		}
		rows.Close()
	}

	t.Logf("Read %d traces", len(rawTraces))

	// Run each strategy
	strategies := getStrategies()
	results := make([]strategyResult, len(strategies))

	for si, strategy := range strategies {
		res := &results[si]
		res.name = strategy.name
		res.worstJ = 1.0

		if strategy.setup != nil {
			strategy.setup(rawTraces)
		}

		// Build cohorts for this strategy
		type cohortInfo struct {
			sigSet  map[string]struct{}
			sigs    []string
			bands   minhash.Bands
			mhSig   minhash.Signature
			count   int
			rootSvc string
			rootOp  string
		}
		cohortMap := make(map[string]*cohortInfo)

		for _, rt := range rawTraces {
			seen := make(map[string]struct{})
			dropped := 0
			for _, sp := range rt.spans {
				sig, include := strategy.extract(sp.svcName, sp.info)
				if include && sig != "" {
					seen[sig] = struct{}{}
				} else {
					dropped++
				}
			}
			res.droppedSpans += dropped

			if len(seen) == 0 {
				res.emptyTraces++
				continue
			}

			sigs := make([]string, 0, len(seen))
			for s := range seen {
				sigs = append(sigs, s)
			}
			sort.Strings(sigs)
			key := strings.Join(sigs, "\n")

			c, ok := cohortMap[key]
			if !ok {
				sig := minhash.Compute(sigs)
				c = &cohortInfo{
					sigSet:  seen,
					sigs:    sigs,
					bands:   sig.ToBands(),
					mhSig:   sig,
					rootSvc: rt.rootSvc,
					rootOp:  rt.rootSpan,
				}
				cohortMap[key] = c
			}
			c.count++
		}

		cohorts := make([]*cohortInfo, 0, len(cohortMap))
		for _, c := range cohortMap {
			cohorts = append(cohorts, c)
		}
		res.totalCohorts = len(cohorts)

		// Exhaustive pairwise comparison
		for i := 0; i < len(cohorts); i++ {
			for j := i + 1; j < len(cohorts); j++ {
				res.totalPairs++
				if !minhash.AnyBandMatches(cohorts[i].bands, cohorts[j].bands) {
					continue
				}
				res.bandMatches++

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

				if exactJ >= 1.0 {
					res.exactMatches++
				}
				if exactJ >= 0.7 {
					res.goodMatches++
				}
				if exactJ < 0.2 {
					res.badMatches++
					res.avgBadJ += exactJ
				}
				if exactJ < 0.1 {
					res.veryBadMatches++
				}
				if exactJ < res.worstJ {
					res.worstJ = exactJ
					res.worstA = cohorts[i].rootSvc + "|" + cohorts[i].rootOp
					res.worstB = cohorts[j].rootSvc + "|" + cohorts[j].rootOp
					sort.Strings(shared)
					res.worstShared = strings.Join(shared, ", ")
				}
			}
		}
		if res.badMatches > 0 {
			res.avgBadJ /= float64(res.badMatches)
		}
	}

	// Report
	t.Log("\n=== Strategy Comparison ===\n")
	t.Logf("  %-50s %7s %7s %7s %7s %7s %7s %7s %8s",
		"Strategy", "Cohorts", "Pairs", "Bands", "J<0.1", "J<0.2", "J≥0.7", "Empty", "WorstJ")
	t.Logf("  %s", strings.Repeat("─", 130))

	for _, r := range results {
		t.Logf("  %-50s %7d %7d %7d %7d %7d %7d %7d %8.3f",
			r.name, r.totalCohorts, r.totalPairs, r.bandMatches,
			r.veryBadMatches, r.badMatches, r.goodMatches, r.emptyTraces, r.worstJ)
	}

	t.Log("\n=== Worst Match Details ===\n")
	for _, r := range results {
		t.Logf("  %s:", r.name)
		t.Logf("    Worst Jaccard: %.3f", r.worstJ)
		if r.worstJ < 1.0 {
			t.Logf("    A: %s", r.worstA)
			t.Logf("    B: %s", r.worstB)
			t.Logf("    Shared: %s", r.worstShared)
		}
		t.Logf("    Dropped spans: %d, Empty traces: %d", r.droppedSpans, r.emptyTraces)
		t.Log()
	}
}
