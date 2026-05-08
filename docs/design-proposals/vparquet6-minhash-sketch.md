# MinHash Columns — Implementation Sketch

Phase 1: write-only. Columns are computed and stored but not queryable via TraceQL yet.

## Why vParquet5, not vParquet6

Parquet schema evolution is additive. When `Reconstruct` reads a row:
- **Old block missing the columns** → struct fields stay at zero. Zero bands never match — correct behavior (no structural filtering on old blocks).
- **New block with the columns** → works normally.
- **Old Tempo binary reading new blocks** → the old `Trace` struct doesn't have MinHash fields. Extra columns in the file are silently ignored. No crash.
- **Old Tempo binary writing blocks** → omits the columns. New Tempo reads those blocks and gets zeros — same as reading an old block.

No new block format version needed. Just add the fields to vParquet5's `Trace` struct. Mixed deployments are safe.

## Insertion point

One function handles all write paths:

```
Ingester flush       → traceToParquet() → finalizeTrace() → assignNestedSetModelBoundsAndServiceStats()
Block builder flush  → traceToParquet() → finalizeTrace() → assignNestedSetModelBoundsAndServiceStats()
Compaction merge     → Combiner.Result() → finalizeTrace() → assignNestedSetModelBoundsAndServiceStats()
```

Zero changes to the distributor, WAL, ingestion pipeline, or query path.

## Files to change

### 1. Schema: add 4 columns to `Trace`

File: `tempodb/encoding/vparquet5/schema.go`

```go
type Trace struct {
    TraceID           []byte         `parquet:""`
    TraceIDText       string         `parquet:",snappy"`
    StartTimeUnixNano uint64         `parquet:",delta"`
    EndTimeUnixNano   uint64         `parquet:",delta"`
    DurationNano      uint64         `parquet:",delta"`
    RootServiceName   string         `parquet:",dict"`
    RootSpanName      string         `parquet:",dict"`
    ServiceStats      []ServiceStats `parquet:""`

    // MinHash bands for structural similarity (similar_to predicate).
    // Computed at block build time from service.name|span.name|semantic_attrs.
    // K=8, B=4, R=2. See docs/design-proposals/2026-05 TraceQL Structural Similarity.md
    MinHashBand0 uint64 `parquet:",delta"`
    MinHashBand1 uint64 `parquet:",delta"`
    MinHashBand2 uint64 `parquet:",delta"`
    MinHashBand3 uint64 `parquet:",delta"`

    ResourceSpans []ResourceSpans `parquet:"rs"`
}
```

### 2. Compute MinHash at flush time

File: `tempodb/encoding/vparquet5/nested_set_model.go`

In `assignNestedSetModelBoundsAndServiceStats`, after the existing span loop that builds `ServiceStats`:

```go
import (
    "sort"
    "strings"

    "github.com/grafana/tempo/pkg/drain"
    "github.com/grafana/tempo/pkg/minhash"
)

var signatureAttrKeys = []string{
    "http.route",
    "rpc.service", "rpc.method",
    "db.system", "db.operation", "db.name",
    "messaging.system", "messaging.operation", "messaging.destination.name",
}

var httpMethodNames = map[string]bool{
    "GET": true, "POST": true, "PUT": true, "DELETE": true,
    "PATCH": true, "HEAD": true, "OPTIONS": true, "TRACE": true,
}

// Inside assignNestedSetModelBoundsAndServiceStats, after ServiceStats computation:

seen := make(map[string]struct{}, spanCount)
for _, rs := range trace.ResourceSpans {
    svcName := rs.Resource.ServiceName
    for _, ss := range rs.ScopeSpans {
        for _, s := range ss.Spans {
            // Skip bare HTTP method spans without a route
            if httpMethodNames[s.Name] {
                hasRoute := false
                for _, a := range s.Attrs {
                    if a.Key == "http.route" && len(a.Value) > 0 {
                        hasRoute = true
                        break
                    }
                }
                if !hasRoute {
                    continue
                }
            }

            normName := normalizeSpanName(s.Name)

            parts := make([]string, 0, 4)
            for _, key := range signatureAttrKeys {
                for _, a := range s.Attrs {
                    if a.Key == key && len(a.Value) > 0 {
                        parts = append(parts, a.Value[0])
                        break
                    }
                }
            }
            sort.Strings(parts)

            sig := svcName + "|" + normName
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
signature := minhash.Compute(sigs)
bands := signature.ToBands()
trace.MinHashBand0 = bands[0]
trace.MinHashBand1 = bands[1]
trace.MinHashBand2 = bands[2]
trace.MinHashBand3 = bands[3]
```

```go
func normalizeSpanName(name string) string {
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
```

### 3. Tests

Add to existing vparquet5 tests:

- **`TestTraceToParquet`**: verify MinHash bands are non-zero for multi-span traces with different services
- **`TestTraceToParquet`**: verify bands are deterministic (same trace → same bands)
- **`TestTraceToParquet`**: verify bands are zero for traces where all spans are skipped (bare HTTP methods only)
- **`TestCompaction`**: verify bands are recomputed after compaction merges fragments
- **Schema backward compat**: read a parquet file written without MinHash columns, verify struct fields are zero
- All existing tests should pass unchanged (new columns are additive)

## What this does NOT include

- No `similar_to()` TraceQL syntax
- No frontend reference trace resolution
- No new TraceQL intrinsics for MinHash columns
- No query-time filtering on MinHash bands
- No new block format version

Those come in Phase 2 after validating the columns in production.

## Dependencies

- `pkg/minhash` — already exists on this branch
- `pkg/drain` — already exists, `public.go` wrapper added on this branch

## Risk

Minimal:
- Additive schema change — old and new binaries coexist safely
- Written at block build time, not ingestion hot path
- 32 bytes per trace (~0.003% overhead on a 1MB trace)
- Zero-allocation computation (benchmarked at 2µs per trace)
- Existing query paths ignore unknown columns
