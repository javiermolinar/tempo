---
Author(s): Javier Molina (@javiermolinar)
Created: May 2026
Status: Draft
Reviewer(s): Awaiting Review
---

# Design Doc: Baseline Trace Selection via Structural Similarity

## 1. Background

Trace comparison is a core debugging workflow: given a slow or broken trace, find a structurally similar healthy trace from a different time window and diff them to understand what changed.

The Tempo Private Eye hackathon built this end-to-end: baseline selection, trace diff, and a Grafana Explore UX. The trace diff engine and UX are solid. The weak point is **finding the baseline** — the hackathon implementation requires client-side orchestration, multiple round-trips, and heuristics that may not generalize across tenants.

This proposal introduces a Tempo-native structural similarity primitive that solves the baseline selection problem at the storage level, eliminating client-side complexity and enabling a single TraceQL query to find baseline candidates.

### The N×N problem

Without structural filtering, finding a baseline requires:

1. Search for candidate traces (by service name, operation, time range) → returns N candidates
2. Fetch each candidate by ID → N trace-by-ID calls
3. Compute structural similarity against the seed trace → N comparisons
4. Pick the best match

For N=200 candidates, this means ~200 full trace fetches (20-40 MB) just to compute set intersections. Every consumer (Grafana, CLI tools, LLM agents) must implement this orchestration independently.

## 2. The Problem

| Issue | Impact |
|-------|--------|
| **Client-side N+1 fetches** | Scoring requires fetching full traces for each candidate to compute similarity |
| **No storage-level filtering** | All traces matching the selector are returned; structural filtering happens after the fact |
| **Duplicated logic** | Every consumer must reimplement the same orchestration and heuristics |
| **Not composable** | Cannot combine structural similarity with TraceQL metrics (`quantile_over_time`, `rate`) |
| **Heuristic-dependent** | The hackathon's cohort selection (which attributes to match, time window exclusion, confidence scoring) may not work for all tenants |

## 3. Proposals

### Option A: Client-side orchestration (hackathon approach)

Keep baseline selection outside Tempo. Each consumer (Grafana plugin, CLI, agent) implements the full workflow: fetch seed trace → extract attributes → build TraceQL search → fetch candidates → score structurally → pick best match.

**Pros:**
- No Tempo changes required
- Already prototyped and working in the hackathon

**Cons:**
- Code duplication across every consumer
- N+1 fetch overhead for candidate scoring
- Heuristic-dependent — cohort selection logic (which attributes to match, how to weight them) is baked into each consumer and may not generalize across tenants
- Cannot leverage storage-level optimizations
- Not composable with TraceQL metrics pipeline

### Option B: MinHash-based structural similarity in Tempo (proposed)

Compute a MinHash signature for each trace at block build time. Store it as 4 uint64 columns in the existing parquet schema. Expose a `similar_to()` predicate in TraceQL that filters traces by structural similarity at the storage level.

**Pros:**
- Centralized — one implementation, all consumers benefit
- Storage-level filtering — dissimilar traces are skipped before span data is read
- Probabilistic, not heuristic — MinHash approximates Jaccard similarity from the data, not from hand-tuned rules
- Composable — works inside `{}` with all existing TraceQL features including the metrics pipeline
- Minimal overhead — 32 bytes per trace, 2µs computation at block build time, zero impact on ingestion

**Cons:**
- Requires parquet schema change (additive, backward compatible)
- Probabilistic — false positives at low similarity (mitigated by other predicates in the query)
- Depends on span name quality — poorly instrumented spans produce weaker signatures

## 4. How it works

### What is structural similarity?

Two traces are structurally similar when they traverse the same set of services and operations — regardless of latency, errors, or timing. A checkout trace that calls `payment → inventory → shipping` has a different structure than one that calls `payment → fraud-check → inventory → shipping`, even if both are from the same endpoint.

The canonical representation is the **operation signature set**: the set of unique strings derived from each span's `service.name`, `span.name`, and well-known semantic attributes (`http.route`, `db.operation`, `rpc.method`, etc.).

### Jaccard similarity

Jaccard similarity measures the overlap between two sets:

```
J(A, B) = |A ∩ B| / |A ∪ B|
```

- J = 1.0 → identical structure (same services, same operations)
- J = 0.0 → completely different structure (no shared operations)
- J = 0.83 → high overlap (e.g., same flow with 2 extra services added)

### MinHash: approximating Jaccard at scale

Computing exact Jaccard requires materializing both signature sets — which means fetching full traces. MinHash avoids this by compressing each set into a fixed-size signature (K hash values) at write time.

For each trace, K independent hash functions are applied to every signature in the set. For each hash function, only the minimum value is kept. The resulting K minimums form the MinHash signature.

The key property: the probability that two traces' MinHash values agree at any position equals their Jaccard similarity. By comparing K positions, we estimate Jaccard without ever seeing the original sets.

### Locality-sensitive hashing with bands

To make MinHash filterable at the storage level, the K values are grouped into B bands of R values each. Each band is hashed into a single uint64 and stored as a parquet column.

Two traces match if **at least one band** is identical. This creates a tunable threshold:
- More rows per band (higher R) → stricter matching, fewer false positives, more false negatives
- More bands (higher B) → more chances to match, fewer false negatives, more false positives

With K=8, B=4, R=2 (the selected configuration), traces with Jaccard ≥ 0.7 match with >95% probability, while traces with Jaccard ≤ 0.3 match only ~30% of the time — and those false positives are eliminated by the other predicates in the query.

### Signature composition

Each span contributes one signature string. The signature is built from:

| Component | Source | Always present |
|-----------|--------|----------------|
| Service name | `resource.service.name` | Yes |
| Span name | `span.name` (normalized via stateless heuristic) | Yes |
| Semantic attributes | `http.route`, `rpc.method`, `db.operation`, etc. | When present |

Span names are normalized using the same tokenizer and data heuristic from Tempo's `pkg/drain` package (already used in the metrics generator): UUIDs, hex strings, and numeric tokens are replaced with `<_>` so that `/users/12345` and `/users/67890` produce the same signature.

Spans where `span.name` is a bare HTTP method (`GET`, `POST`, etc.) with no `http.route` are excluded — they produce generic signatures that collide across unrelated endpoints.

### What we don't preserve

- **Tree structure** — parent-child relationships, call depth, ordering. The trace can be fragmented across blocks, so the tree is not reliably available.
- **Span counts** — if the same operation is called 3 times vs 1 time, the signature is the same (it's a set, not a multiset).
- **Timing** — latency, duration, start times are not part of the signature.

These are deliberate trade-offs. The signature answers "what services and operations are involved?" not "how are they arranged?" The diff engine handles the structural details after the baseline is selected.

## 5. The TraceQL primitive

`similar_to()` is a predicate inside `{}`, not a pipeline stage. It filters traces at the storage layer.

**Find a healthy baseline for a broken trace:**

```
{ resource.service.name = "checkout"
  && name = "POST /cart"
  && status != error
  && similar_to("abc123def456") }
```

**One-query baseline selection with representative duration:**

```
{ resource.service.name = "checkout"
  && name = "POST /cart"
  && status != error
  && similar_to("abc123def456") }
| quantile_over_time(duration, 0.5)
```

Returns the p50 duration of structurally similar healthy traces, plus exemplar trace IDs at that percentile — ideal baseline candidates.

**How the frontend resolves it:**

1. Parse the query, detect `similar_to("abc123")`
2. Fetch trace `abc123` via existing TraceByID path
3. Compute MinHash bands from the trace's span signatures
4. Rewrite the predicate to storage-level band filters: `minHashBand0 = X || minHashBand1 = Y || ...`
5. Shard the rewritten query to queriers as usual

Each shard receives concrete band values — no shard needs to fetch the reference trace. The MinHash columns exist in parquet but are never exposed in the query language. Users write `similar_to(traceID)`, Tempo handles the rest.

## 6. Storage changes

Four uint64 columns added to the existing vParquet5 `Trace` struct:

```go
MinHashBand0 uint64 `parquet:",delta"`
MinHashBand1 uint64 `parquet:",delta"`
MinHashBand2 uint64 `parquet:",delta"`
MinHashBand3 uint64 `parquet:",delta"`
```

**32 bytes per trace.** Parquet schema evolution is additive — old blocks return zeros for missing columns, old binaries ignore extra columns. No new block format version required. Mixed deployments are safe.

Computed at block build time in `assignNestedSetModelBoundsAndServiceStats` — the same function that already computes `ServiceStats` and nested set bounds. Covers ingester flush, block builder flush, and compaction. Zero changes to the distributor, WAL, or ingestion hot path.

Compaction automatically recomputes MinHash when trace fragments merge, healing partial signatures from the initial flush.

## 7. Use cases beyond baseline selection

The MinHash columns are a general-purpose structural fingerprint. Beyond baseline selection:

- **Regression detection after deploys** — compare trace shapes before/after, detect disappeared or new code paths
- **SLOs per code path** — scope latency targets to specific call graphs, not just endpoints
- **Canary comparison** — verify canary traces match stable ring structure
- **Alert grouping** — partition failing traces by structural identity
- **Intelligent sampling** — sample one exemplar per shape instead of random sampling

## 8. Validation results

Validated against production data from 2 Grafana Cloud tenants (10 parquet blocks, ~1.3M traces total). Full results in the [Validation Report](./2026-05%20TraceQL%20Structural%20Similarity%20-%20Validation%20Report.md).

| Metric | Tenant A (infra) | Tenant B (mobile, 20+ services) |
|--------|-------------------|----------------------------------|
| Within-cohort match rate | **100%** | **100%** |
| Cross-cohort false positive rate | 1.1% | 0% |
| Cross-block exact match (best pair) | 28/30 (93%) | 30/30 (100%) |
| Shapes stable across all blocks | 2 | 69 |
| Zero false matches cross-service | ✓ | ✓ |

### Signature strategy comparison

5 strategies tested. Stateless heuristic normalization (using drain's tokenizer) eliminates all J<0.1 bad matches:

| Strategy | J<0.1 bad matches | J<0.2 bad matches | Good matches (J≥0.7) |
|----------|-------|-------|------|
| Baseline (raw span names) | 14 | 59 | 33 |
| **Stateless heuristic normalization** | **0** | **22** | **32** |

### Computation overhead

| Trace size | Compute + ToBands | Allocations |
|------------|-------------------|-------------|
| 15 sigs (typical) | 2.0 µs | 0 |
| 50 sigs | 5.7 µs | 0 |

Filter check (`AnyBandMatches`): **1.4 ns**.

## 9. Implementation phases

| Phase | What | Risk |
|-------|------|------|
| **1. Write-only columns** | Add 4 MinHash columns to vParquet5 `Trace` struct. Compute in `assignNestedSetModelBoundsAndServiceStats`. No query support. | Minimal — additive schema, inert columns |
| **2. Query the columns** | Expose as internal trace-level intrinsics. Test predicate pushdown with raw band predicates. | Low — read-only, no new syntax |
| **3. Fragmentation metrics** | Extend `TraceByIDMetrics` to propagate block-level fragment counts from querier to frontend. Emit histograms. | Low — pure observability |
| **4. `similar_to()` predicate** | TraceQL syntax, frontend reference trace resolution, query rewriting. Full end-to-end. | Medium — parser + frontend changes |
| **5. Grafana integration** | Wire into baseline trace selection UX. Replace hackathon client-side orchestration. | Low — consumer of Tempo APIs |

## 10. Risks and mitigations

| Risk | Mitigation |
|------|------------|
| False positives at low Jaccard | Other predicates in the query (`service.name`, `name`, `status`) eliminate them. Validated: 0-1.1% false positive rate on production data. |
| Poor instrumentation (generic span names) | Stateless heuristic normalization eliminates all J<0.1 collisions. Spans without `http.route` are excluded. |
| Trace fragmentation degrades signatures | Compaction recomputes MinHash on merged traces. Baseline queries target historical (compacted) blocks. |
| MinHash parameters need tuning | K=8, B=4, R=2 validated with 10,000 Monte Carlo trials + 2 production tenants. Parameters are compile-time constants. |

## 11. Success criteria

- A single TraceQL query returns structurally similar baseline candidates without client-side orchestration
- Within-cohort match rate remains 100% on production data
- False positive rate stays below 5%
- Zero measurable impact on block build time or query latency for non-`similar_to` queries
- Grafana baseline selection workflow reduces from N+1 fetches to 1 query
