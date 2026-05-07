---
Authors: Javier Molina (@javiermolinar)
Created: May 2025
Last updated: 2025-05-07
---

# TraceQL Structural Similarity

## Summary

This document proposes adding a `similar_to()` predicate to TraceQL that enables finding structurally similar traces using MinHash-based locality-sensitive hashing.
The goal is to support baseline trace selection, regression analysis, and trace comparison workflows without requiring client-side orchestration or new API endpoints.

### Table of Contents

- [Context](#context)
- [Goals and non-goals](#goals-and-non-goals)
- [Background: the baseline trace problem](#background-the-baseline-trace-problem)
- [Design](#design)
  - [Storage: MinHash columns in parquet](#storage-minhash-columns-in-parquet)
  - [Ingestion: computing MinHash at flush time](#ingestion-computing-minhash-at-flush-time)
  - [TraceQL: the similar_to predicate](#traceql-the-similar_to-predicate)
  - [Frontend: reference trace resolution](#frontend-reference-trace-resolution)
  - [Composability with metrics pipeline](#composability-with-metrics-pipeline)
- [Fragmentation and partial traces](#fragmentation-and-partial-traces)
- [Tuning: K, B, and R parameters](#tuning-k-b-and-r-parameters)
- [Empirical validation](#empirical-validation)
  - [Parameter selection](#parameter-selection)
  - [Real scenario validation](#real-scenario-validation)
  - [Computation overhead](#computation-overhead)
- [Alternatives considered](#alternatives-considered)
- [Rollout and backward compatibility](#rollout-and-backward-compatibility)
- [Future work](#future-work)

## Context

Trace comparison is a common debugging workflow: given a slow or broken trace, find a structurally similar healthy trace from a different time window and diff them to understand what changed.
This workflow requires answering the question: "which traces have the same shape as this one?"

Today this is only possible through client-side orchestration.
The Tempo Private Eye hackathon (Hackathon 16) built a prototype that:

1. Fetches the seed trace and extracts service name, operation name, and resource attributes.
2. Builds a TraceQL search query filtered to `status != error`.
3. Searches for candidates in a historical time window.
4. Fetches up to 10 full traces and scores them by Jaccard similarity over operation signature sets.
5. Picks the best match as the baseline.

This works but has significant drawbacks:

- **Client-side N+1 fetches.** Scoring requires fetching full traces for each candidate just to compute a set intersection.
- **No storage-level filtering.** All traces matching the selector are returned; structural filtering happens after the fact.
- **Duplicated logic.** Every consumer (Grafana, CLI tools, LLM agents) must reimplement the same orchestration.
- **Not composable.** Cannot combine structural similarity with TraceQL metrics (`quantile_over_time`, `rate`, etc.).

## Goals and non-goals

### Goals

- Enable storage-level structural similarity filtering within TraceQL.
- Support the baseline trace selection use case end-to-end in a single query.
- Compose naturally with existing TraceQL features (selectors, metrics, exemplars).
- Degrade gracefully when traces are fragmented across blocks.
- Keep the storage cost low (tens of bytes per trace).

### Non-goals

- Tempo does not define what a "baseline" is. Cohort selection heuristics (time window, status filters, percentile targets, confidence scoring) remain in the client (Grafana).
- Tempo does not return a similarity score in the response. The predicate is a filter, not a ranking function.
- Span-level diff (matching individual spans, computing deltas) is a separate concern, addressed by the trace diff engine.

## Background: the baseline trace problem

### What "structurally similar" means

Two traces are structurally similar when they traverse the same set of services and operations.
The canonical representation is the **operation signature set**: the set of `service.name|span.name` pairs across all spans in the trace.

For example, a checkout trace might produce:

```
{ "checkout|POST /cart", "checkout|validateCart", "payment|charge", "inventory|checkStock", "shipping|estimateDelivery" }
```

**Jaccard similarity** over these sets measures structural overlap:

```
J(A, B) = |A ∩ B| / |A ∪ B|
```

Two traces with J=1.0 traverse exactly the same code paths.
Two traces with J=0.0 share no services or operations.

### Why this must happen at the storage level

The alternative — fetching candidate traces and scoring them client-side — requires O(N) full trace fetches where N is the candidate count.
Each trace can be hundreds of KB.
For a cohort of 200 candidates, this means fetching ~20-40 MB of trace data just to compute set intersections.

With storage-level filtering, Tempo evaluates the structural predicate against indexed metadata.
Candidates that don't match are never fetched.
The client receives only traces that are already known to be structurally similar.

### Why the root span is not required

Resource attributes (`resource.service.name`) and span names (`span.name`) are present on **every span**, not just the root.
The signature set can be computed from any subset of a trace's spans.
This is critical because Tempo cannot guarantee root span availability — it may arrive late, be in a different block, or never arrive at all.

## Design

### Storage: MinHash columns in parquet

[MinHash](https://en.wikipedia.org/wiki/MinHash) is a locality-sensitive hashing technique that estimates Jaccard similarity from compact fixed-size signatures.

For each trace, compute K hash values from the operation signature set.
Group these K values into B bands of R rows each.
Hash each band into a single uint64.
Store the B band hashes as trace-level columns in the parquet schema.

With K=8, B=4, R=2:

```go
type Trace struct {
    // ... existing fields ...
    MinHashBand0 uint64 `parquet:",delta"`
    MinHashBand1 uint64 `parquet:",delta"`
    MinHashBand2 uint64 `parquet:",delta"`
    MinHashBand3 uint64 `parquet:",delta"`
}
```

**Storage cost:** 32 bytes per trace. For a block with 1M traces, this adds ~32 MB.

**Band matching probability by Jaccard similarity (B=4, R=2) — validated empirically with 10,000 Monte Carlo trials per Jaccard level:**

| Jaccard | P(at least one band matches) | False negative rate |
|---------|------|------|
| 1.0     | 100% | 0%   |
| 0.9     | 99.6% | 0.4% |
| 0.83    | 99.6% | 0.4% |
| 0.8     | 99.6% | 0.4% |
| 0.7     | 95.7% | 4.3% |
| 0.5     | 68.3% | 31.7% |
| 0.3     | 30.2% | 69.8% |
| 0.2     | 14.9% | 85.1% |

Traces with Jaccard ≥ 0.7 are found with >95% probability.
Traces with Jaccard ≤ 0.3 pass the filter ~30% of the time — these false positives are eliminated by the other predicates in the selector (`service.name`, `name`, `status`).

**Why R=2 and not R=4:** empirical validation showed that R=4 requires all 4 hash values in a band to match simultaneously. With K=8, this produces unacceptable false negative rates: the real-world booking scenario (Jaccard=0.833) was filtered out 100% of the time with B=2,R=4 but passes 99.6% of the time with B=4,R=2 at the same storage cost (32 bytes). See `docs/design-proposals/similar-to-validation/` for the full validation tool and results.

### Block builders: computing MinHash at block build time

The MinHash computation runs at block build time, **not during span ingestion**.
The baseline use case targets historical data ("find a healthy trace from hours/days ago"), so recent data in the ingester's live store does not need MinHash columns.
This eliminates any overhead on the span ingestion hot path (`instance.push()`).

The computation is inserted in `assignNestedSetModelBoundsAndServiceStats` (`tempodb/encoding/vparquet5/nested_set_model.go`), which already iterates every span in the trace during block flush to build `ServiceStats` and nested set model bounds.
The MinHash piggybacks on the same loop with no extra I/O.

The implementation lives in `pkg/minhash/`, a standalone package with zero dependencies on Tempo internals:

```go
// pkg/minhash/minhash.go
func Compute(signatures []string) Signature  // K minimum hashes, zero allocations
func (s Signature) ToBands() Bands           // B band hashes from K values
func AnyBandMatches(a, b Bands) bool         // filter predicate: 1.4ns per check
```

**Insertion point:** The block builder calls `minhash.Compute` with the `service.name|span.name` pairs extracted from the same span loop that already populates `ServiceStats`, then stores the resulting 4 band hashes in the parquet trace-level columns.

### TraceQL: the similar_to predicate

`similar_to()` is a span-level predicate inside `{}`, not a pipeline stage.
It filters traces at the storage layer using MinHash band matching.

```
{ resource.service.name = "checkout" && name = "POST /cart" && similar_to("abc123def456") }
```

Semantics:

- The argument is a trace ID (hex string).
- The predicate evaluates to `true` for traces where at least one MinHash band matches the reference trace.
- It composes with all other predicates using `&&` and `||`.

**Why a predicate and not a pipeline stage:**

A pipeline stage (`| similar_to(...)`) operates on spansets after the storage layer returns them.
A predicate inside `{}` pushes the filter down to parquet column reads.
The MinHash band columns have dictionary encoding and support predicate pushdown — the parquet engine skips entire row groups where no band matches.
This is the difference between scanning all candidates and skipping 95% of them.

### Frontend: reference trace resolution

The frontend resolves `similar_to("abc123")` before sharding:

1. Parse the query and detect the `similar_to` predicate.
2. Fetch trace `abc123` via the existing TraceByID path.
3. Compute the operation signature set from the fetched trace.
4. Compute MinHash band values using the same hash functions and seeds.
5. Rewrite the predicate to storage-level band filters: `minHashBand0 = X || minHashBand1 = Y || minHashBand2 = Z || minHashBand3 = W`.
6. Shard the rewritten query to ingesters and queriers as usual.

Each shard receives concrete band values — no shard needs to fetch the reference trace.
The resolution adds one TraceByID call at the frontend before sharding begins.

```
// Before rewrite (user-facing):
{ resource.service.name = "checkout" && similar_to("abc123") }

// After rewrite (internal, sent to shards):
{ resource.service.name = "checkout" && (minHashBand0 = 0x1A2B || minHashBand1 = 0x3C4D || minHashBand2 = 0x5E6F || minHashBand3 = 0x7A8B) }
```

The `minHashBand{0..3}` columns are internal intrinsics — they exist in the parquet schema and are queryable by the engine, but are not exposed directly in the TraceQL language.
Only `similar_to()` generates predicates against them.

### Composability with metrics pipeline

Because `similar_to()` is a predicate inside `{}`, it composes naturally with the metrics pipeline:

```
{ resource.service.name = "checkout"
  && name = "POST /cart"
  && status != error
  && similar_to("abc123") }
| quantile_over_time(duration, 0.5)
```

This single query:

1. Filters to healthy checkout traces that are structurally similar to `abc123`.
2. Computes p50 duration over the matching cohort.
3. Returns exemplar trace IDs at the p50 bucket.

The exemplars are traces that are both structurally similar and have representative duration — ideal baseline candidates.

**This is the complete baseline trace selection flow in one TraceQL query.**
Grafana builds the query (adding time window, status filter, service/operation from the seed trace).
Tempo executes it using existing infrastructure.
Grafana picks the exemplar trace ID from the metrics response and feeds it into the diff engine.

## Fragmentation, compaction, and partial traces

A trace's spans can be distributed across multiple blocks and ingesters.
Each block computes MinHash from the spans it has — a partial signature set.

MinHash degrades gracefully with partial data:

- If a block contains 80% of a trace's spans, ~80% of the signature set is present.
- Most of the K minimum hash values will be the same as the full trace's values (the minimum is robust to missing elements when most are present).
- Some bands will still match the reference, just with lower probability than the full trace.

**Comparison with exact hashing:** an exact structure key computed from a partial span set produces a completely different hash — binary mismatch.
MinHash produces a partially matching signature — probabilistic degradation instead of total failure.

### Compaction automatically recomputes MinHash

The compaction path already merges trace fragments and recomputes trace-level metadata.
No special handling is needed for MinHash.

The flow during compaction:

1. The compactor reads trace fragments from multiple input blocks.
2. When the same trace ID appears in multiple blocks, `Combiner` merges the fragments: it deduplicates spans by ID+kind and unions resource spans.
3. `Combiner.Result()` calls `finalizeTrace()`, which calls `assignNestedSetModelBoundsAndServiceStats()`.
4. That function iterates **all spans of the merged trace** — the complete combined span set, not the partial set from either fragment.
5. The MinHash computation runs inside the same function, so it sees the full structure.
6. The trace is written to the new compacted block with correct MinHash bands.

This means:

- **First flush:** may produce a partial MinHash from an incomplete trace.
- **After compaction:** MinHash is recomputed from the merged trace with all fragments combined.
- **No extra code required:** the existing compaction path (`Combiner` → `finalizeTrace` → `assignNestedSetModelBoundsAndServiceStats`) is the same code path that computes MinHash at initial block build time.

The partial MinHash from the first flush is a best-effort approximation.
Compaction heals it automatically when fragments merge.

### Worst case

A long-running trace with spans arriving over hours may never fully consolidate.
For these traces, the MinHash band filter may miss them (false negative).
This is acceptable — the selector predicates (`service.name`, `name`) still return them; they simply bypass the MinHash optimization.

## Tuning: K, B, and R parameters

| Parameter | Meaning | Proposed value |
|-----------|---------|----------------|
| K | Total number of hash functions | 8 |
| B | Number of bands | 4 |
| R | Rows per band (K/B) | 2 |

These values were selected based on empirical validation against real trace scenarios (see `docs/design-proposals/similar-to-validation/`).
Six configurations were tested with 10,000 Monte Carlo trials and real tercios scenarios:

| Config | Storage | Match rate at J=0.83 | False positive rate at J=0.3 |
|--------|---------|---------------------|------------------------------|
| K=8, B=2, R=4 | 16 bytes | 0% ❌ | 1.5% |
| **K=8, B=4, R=2** | **32 bytes** | **99.6%** ✓ | **30.2%** |
| K=16, B=4, R=4 | 32 bytes | 87.7% | 2.9% |
| K=12, B=4, R=3 | 32 bytes | 98.6% | 9.7% |
| K=12, B=6, R=2 | 48 bytes | 100% | 41.8% |
| K=16, B=8, R=2 | 64 bytes | 100% | 50.7% |

The critical finding: **R (rows per band) dominates false negative behavior.** R=4 requires 4 hash slots to agree simultaneously per band, which fails catastrophically at small K. R=2 only needs 2 slots per band, giving 4 independent chances with B=4 bands.

- **K=8** is sufficient for the signature set sizes observed in real traces (10-30 unique service|operation pairs).
- **B=4** gives four independent band checks, reducing false negatives to <1% at J≥0.8.
- **R=2** keeps each band check permissive enough that near-similar traces pass, while the 30% false positive rate at J=0.3 is acceptable because other predicates in the selector eliminate false matches.

These are compile-time constants, not per-tenant configuration.
Changing them requires a new block format version (vParquet6) because existing blocks would have incompatible band values.

The hash function seeds are also compiled into the binary and must be stable across versions.

## Empirical validation

All design decisions in this document are backed by empirical validation.
The validation tools and benchmarks live in the repository and can be reproduced by anyone.

### Parameter selection

Six MinHash configurations were tested with 10,000 Monte Carlo trials per Jaccard level using synthetic signature sets, plus 10,000 trials against real tercios trace scenarios (booking distributed slow-cache-miss vs fast-cache-hit, Jaccard = 0.833).

**Tool:** `docs/design-proposals/similar-to-validation/` — standalone Go program, run with `go run .`

Key finding: the initial proposal (K=8, B=2, R=4) had a **100% false negative rate** on the real booking scenario.
R=4 requires all 4 hash values in a band to agree simultaneously, which fails catastrophically at small K.
Switching to B=4, R=2 at the same storage cost (32 bytes) achieved a **99.6% match rate**.

Full results in the [Tuning section](#tuning-k-b-and-r-parameters).

### Real scenario validation

The real-world test scenarios come from the Tempo Private Eye hackathon (tercios scenario files):

- **Booking slow (cache-miss):** 10 services, 10 unique `service|operation` signatures.
- **Booking fast (cache-hit):** same 10 services + 2 new (`pricing-cache-gateway`, `pricing-cache-warmer`), 12 unique signatures.
- **Exact Jaccard:** 10/12 = 0.833.

Validation results with the chosen configuration (K=8, B=4, R=2):

| Test case | Exact J | Est. J | Bands matched | Filter result |
|-----------|---------|--------|---------------|---------------|
| Identical traces | 1.000 | 1.000 | 4/4 | ✓ Pass |
| Slow vs Fast (J=0.833) | 0.833 | 0.750 | 2/4 | ✓ Pass |
| Slow vs Unrelated auth | 0.000 | 0.000 | 0/4 | ✗ Filtered |
| 20% fragment loss (same trace) | 0.800 | 0.750 | 2/4 | ✓ Pass |
| 50% fragment loss (same trace) | 0.400 | 0.375 | 0/4 | ✗ Filtered |

**Tool:** `pkg/minhash/` — `go test ./pkg/minhash/ -v` runs all scenario-based tests including fragmentation simulation.

### Computation overhead

Benchmarked on Apple M3 Pro. The MinHash computation runs at block build time, not on the span ingestion hot path.

| Trace size (unique sigs) | Compute + ToBands | Allocations |
|--------------------------|-------------------|-------------|
| 5 | 0.6 µs | 0 |
| 15 (typical) | 2.0 µs | 0 |
| 30 | 3.6 µs | 0 |
| 50 | 5.7 µs | 0 |
| 100 (large) | 11 µs | 0 |

The filter check (`AnyBandMatches`) is **1.4 ns** — four uint64 comparisons.

**Zero heap allocations** in both computation and filtering.

For a block with 100k traces at 15 signatures each: 100k × 2 µs = **0.2 seconds** total added to a block build that takes minutes.

**Tool:** `pkg/minhash/` — `go test ./pkg/minhash/ -bench=. -benchmem`

## Alternatives considered

### Exact structure key (single uint64 hash of the full signature set)

Lower storage cost (8 bytes) but binary match semantics — either identical structure or no match.
Any missing span from fragmentation produces a completely different hash.
No graceful degradation.
Rejected in favor of MinHash's probabilistic approach.

### Client-side scoring with full trace fetches

The approach used in the Hackathon 16 prototype.
Works but requires O(N) trace fetches for candidate scoring.
Cannot leverage storage-level filtering.
Logic must be duplicated across every consumer.
This design replaces that approach.

### `similar_to()` as a pipeline stage

A pipeline stage operates on spansets after they leave the storage layer.
This means all candidates matching the selector are fetched before similarity filtering.
As a predicate, `similar_to()` pushes the filter into parquet column reads — far more efficient.
A pipeline stage would also require a different execution model (scoring at the combiner level after merging partial results across blocks), adding significant complexity.

### Pre-computed Jaccard distance in a new API endpoint

An endpoint like `GET /api/traces/{id}/similarity?target={id}` that fetches both traces and returns the Jaccard score.
This is a pure function that any client can implement after two TraceByID calls — it doesn't leverage Tempo's storage capabilities.
There is no reason for this to be a Tempo API.

### Storing operation names in ServiceStats

Extend `ServiceStats` to carry operation names per service.
The combiner already unions `ServiceStats` across blocks, so operation names would be merged correctly.
This solves the fragmentation problem for metadata-level scoring but does not provide storage-level filtering — the parquet engine cannot skip row groups based on set containment of repeated fields.
MinHash band columns support predicate pushdown; repeated string fields do not.

## Rollout and backward compatibility

### New block format version

The MinHash columns require a new parquet schema — this would be part of the next vParquet version (e.g., vParquet6).
Blocks written with older formats do not have MinHash columns.

### Query behavior with mixed block versions

When `similar_to()` is present in a query and a shard processes an older block without MinHash columns:

- The shard ignores the MinHash band predicates (treats them as `true`).
- The selector predicates (`service.name`, `name`, `status`) still filter candidates.
- Results from older blocks are unfiltered by structural similarity — more false positives, but no false negatives.

This is safe: the query returns a superset of the structurally similar traces until all blocks are compacted to the new format.

### Feature gate

`similar_to()` is gated behind a feature flag during the rollout period.
Queries using `similar_to()` on deployments without the flag return an error.

## Future work

- **Adaptive K/B/R tuning.** Analyze production workloads to determine if K=8 is sufficient or if certain tenants benefit from larger signatures.
- **MinHash in the ingester.** Compute and expose MinHash for in-flight traces in the ingester, enabling similarity filtering on recent data before block flush. Currently not needed because the baseline use case targets historical data.
- **Weighted signatures.** Weight signatures by span duration or frequency so that dominant services contribute more to the hash than ephemeral ones.
- **Similarity as a metric.** Expose structural similarity as a value in the metrics pipeline (e.g., `| similarity_to("abc123")` returning a float) for distribution analysis. This is distinct from the filter predicate and would require combiner-level scoring after merging partial results.
- **Cross-tenant similarity.** Investigate whether MinHash can support cross-tenant structural comparison for multi-tenant platforms.

## Appendix: validation artifacts

| Artifact | Path | Run command |
|----------|------|-------------|
| MinHash package (computation + filtering) | `pkg/minhash/` | `go test ./pkg/minhash/ -v` |
| Benchmarks | `pkg/minhash/bench_test.go` | `go test ./pkg/minhash/ -bench=. -benchmem` |
| Monte Carlo + real scenario validation | `pkg/minhash/minhash_test.go` | Included in `go test -v` |
| Parquet integration pattern tests | `pkg/minhash/parquet_test.go` | Included in `go test -v` |
| Parameter search tool (standalone) | `docs/design-proposals/similar-to-validation/` | `cd similar-to-validation && go run .` |
| Tercios scenario files (external) | hackathon-16 repo | See [Test Scenarios Playbook](../../TempoPrivateEye-Test-Scenarios.md) |
