# Trace Fragmentation Metrics — Implementation Plan

Emit per-lookup fragmentation statistics during trace-by-ID requests. Answers: how many blocks contain fragments of the same trace, and at what compaction levels?

## Why

- Understand how well compaction consolidates traces
- Validate MinHash accuracy assumptions (richer signatures on compacted blocks)
- Detect tenants/workloads with high trace fragmentation
- Generally useful for Tempo operators independent of `similar_to()`

## What to measure

Per trace-by-ID lookup:
- **blocks_with_trace**: how many blocks contained fragments of this trace (across all queriers + ingesters)
- **compaction_levels**: the compaction level of each block that had a fragment

## Changes

### 1. Proto: extend `TraceByIDMetrics`

File: `pkg/tempopb/tempo.proto`

```proto
message TraceByIDMetrics {
    uint64 inspectedBytes = 1;
    uint32 blocksWithTrace = 2;
    repeated uint32 compactionLevels = 3;
}
```

Regenerate: `make gen-proto`

### 2. Store: populate metrics in `tempodb.Find()`

File: `tempodb/tempodb.go`, in `Find()` method

After `RunJobs` returns, we already iterate `partialTraces` to build `partialTraceObjs`. Count hits and collect compaction levels from the block metadata:

```go
// Already have copiedBlocklist ([]interface{}) aligned with partialTraces
blocksHit := uint32(0)
var levels []uint32
for i, pt := range partialTraces {
    if trace, ok := pt.(*tempopb.TraceByIDResponse); ok && trace != nil {
        blocksHit++
        if meta, ok := copiedBlocklist[i].(*backend.BlockMeta); ok {
            levels = append(levels, meta.CompactionLevel)
        }
    }
}
```

Attach to each partial response, or return as a separate aggregated metrics object alongside `partialTraceObjs`. The simplest option: add a new return value or stuff it into the first non-nil response's metrics.

### 3. Querier: propagate to frontend

File: `modules/querier/querier.go`, in `FindTraceByID()`

The querier calls `q.store.Find()` which returns `[]*tempopb.TraceByIDResponse`. Sum `blocksWithTrace` and union `compactionLevels` from all partial responses into the single response returned to the frontend:

```go
resp := &tempopb.TraceByIDResponse{
    Trace: combinedTrace,
    Metrics: &tempopb.TraceByIDMetrics{
        InspectedBytes:  inspectedBytes,
        BlocksWithTrace: totalBlocksHit,       // NEW
        CompactionLevels: allCompactionLevels,  // NEW
    },
}
```

Also count the ingester/live-store contribution as a fragment (compaction level 0).

### 4. Frontend combiner: aggregate and emit

File: `modules/frontend/combiner/response_metrics.go`

Extend `TraceByIDMetricsCombiner.Combine()`:

```go
func (mc *TraceByIDMetricsCombiner) Combine(newMetrics *tempopb.TraceByIDMetrics, resp PipelineResponse) {
    if newMetrics != nil && !IsCacheHit(resp.HTTPResponse()) {
        mc.Metrics.InspectedBytes += newMetrics.InspectedBytes
        mc.Metrics.BlocksWithTrace += newMetrics.BlocksWithTrace
        mc.Metrics.CompactionLevels = append(mc.Metrics.CompactionLevels, newMetrics.CompactionLevels...)
    }
}
```

### 5. Frontend: emit Prometheus metrics

File: `modules/frontend/traceid_handlers.go` (or a new metrics file)

After the combiner finalizes, emit:

```go
var (
    traceFragments = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Namespace: "tempo",
        Name:      "trace_by_id_block_fragments",
        Help:      "Number of blocks containing fragments of a single trace per lookup",
        Buckets:   []float64{1, 2, 3, 4, 5, 8, 10, 15, 20},
    }, []string{"tenant"})

    traceFragmentCompactionLevel = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Namespace: "tempo",
        Name:      "trace_by_id_fragment_compaction_level",
        Help:      "Compaction level of blocks that contained trace fragments",
        Buckets:   []float64{0, 1, 2, 3, 4, 5, 6, 7, 8},
    }, []string{"tenant"})
)

// After combiner.HTTPFinal():
metrics := combiner.MetricsCombiner.Metrics
traceFragments.WithLabelValues(tenantID).Observe(float64(metrics.BlocksWithTrace))
for _, level := range metrics.CompactionLevels {
    traceFragmentCompactionLevel.WithLabelValues(tenantID).Observe(float64(level))
}
```

## What the data tells you

- `p50(block_fragments) = 1` → most traces are in one block, MinHash is accurate at first flush
- `p50(block_fragments) = 3` → traces are scattered, compaction is critical for MinHash quality
- `p90(compaction_level) = 1` → most fragments come from uncompacted blocks, compaction hasn't caught up
- `p90(compaction_level) = 3+` → fragments are in compacted blocks, MinHash sees complete traces

## Scope

- No config changes, no feature flags
- No behavioral changes — pure observability addition
- Backward compatible — new proto fields default to zero
