---
Authors: Javier Molina (@javiermolinar)
Created: May 2026
Last updated: 2026-05-08
---

# TraceQL Structural Similarity — Validation Report

This document reports empirical validation of the MinHash-based structural similarity design proposed in [TraceQL Structural Similarity](./2026-05%20TraceQL%20Structural%20Similarity.md).

All results are reproducible from the test suite in `pkg/minhash/`.

## Table of Contents

- [What we hashed](#what-we-hashed)
- [Datasets](#datasets)
  - [Synthetic: tercios scenarios](#synthetic-tercios-scenarios)
  - [Production: Grafana Cloud tenant](#production-grafana-cloud-tenant)
- [Parameter selection](#parameter-selection)
- [Single-block validation](#single-block-validation)
  - [Signature set statistics](#signature-set-statistics)
  - [Structural cohorts](#structural-cohorts)
  - [Within-cohort matching](#within-cohort-matching)
  - [Cross-cohort false positives](#cross-cohort-false-positives)
- [Cross-block validation](#cross-block-validation)
  - [Same service, same time range](#same-service-same-time-range)
  - [Same service, different time range](#same-service-different-time-range)
  - [Different services](#different-services)
  - [Temporal stability](#temporal-stability)
- [Computation overhead](#computation-overhead)
- [Limitations and open questions](#limitations-and-open-questions)

## What we hashed

Each span contributes one signature string to the trace's signature set. The signature is built from:

| Component | Source | Always present |
|-----------|--------|----------------|
| Service name | `resource.service.name` | Yes |
| Span name | `span.name` | Yes |
| Semantic attributes | OTel convention attributes (when present) | No |

The semantic attribute allowlist:

| Protocol | Attributes included |
|----------|-------------------|
| HTTP | `http.route`, `http.request.method`, `http.method` |
| RPC | `rpc.service`, `rpc.method` |
| Database | `db.system`, `db.operation`, `db.name` |
| Messaging | `messaging.system`, `messaging.operation`, `messaging.destination.name` |

**Signature format:** `service.name|span.name[|attr1|attr2|...]`

Semantic attributes are sorted alphabetically and appended only when present. Duplicate signatures within a trace are deduplicated (the signature set contains only unique entries).

**Examples from production data:**

```
neon-console|Postgres GetContext
neon-control-plane|HTTP internal-computeclient GetComputeState|GET
neon-console|GetProjectEndpoint|/projects/{project_id}/endpoints/{endpoint_id}|GET
neon-console|HTTP orb FetchSubscriptionsByAccountID|GET
pageserver|GET_PAGE
pageserver|read_and_wait
pageserver|task_main
```

## Datasets

### Synthetic: tercios scenarios

Two booking-flow scenarios from the [tercios](https://github.com/javiermolinar/tercios) trace generator:

| Scenario | Services | Unique signatures | Description |
|----------|----------|-------------------|-------------|
| Slow (cache-miss) | 10 | 10 | Full booking flow with cache misses, repeated DB lookups |
| Fast (cache-hit) | 12 | 12 | Same flow + 2 new services (`pricing-cache-gateway`, `pricing-cache-warmer`) |

Expected Jaccard: 10/12 = **0.833**

### Production: Grafana Cloud tenant

Five parquet blocks from a single Grafana Cloud tenant (tenant ID: 1000087), spanning April 11 – May 7, 2026.

| Block | Date | Traces | Shapes | Format | Size |
|-------|------|--------|--------|--------|------|
| `0005fda8` | Apr 11 | 123,681 | 188 | vParquet4 | 27 MB |
| `fe87871b` | Apr 13 | 149,895 | 162 | vParquet4 | 43 MB |
| `3ffaa5ab` | May 4 | 5,878 | 7 | vParquet4 | 10 MB |
| `808eac91` | May 4 | 610 | 9 | vParquet4 | 1 MB |
| `c2ff3990` | May 7 | 17 | 3 | vParquet4 | 84 KB |

The tenant runs two distinct workloads:
- **neon-console / neon-control-plane / neon-billing-manager** — API services with many single-span DB traces and some multi-span API flows (4–14 signatures).
- **pageserver** — storage engine with multi-span traces (6–14 signatures).

## Parameter selection

Six MinHash configurations were tested with 10,000 Monte Carlo trials at each Jaccard level, plus 10,000 trials against the real tercios scenarios.

| Config | Storage | Match rate at J=0.83 | False positive at J=0.3 |
|--------|---------|---------------------|------------------------|
| K=8, B=2, R=4 | 16 bytes | **0%** ❌ | 1.5% |
| **K=8, B=4, R=2** | **32 bytes** | **99.6%** ✓ | **30.2%** |
| K=16, B=4, R=4 | 32 bytes | 87.7% | 2.9% |
| K=12, B=4, R=3 | 32 bytes | 98.6% | 9.7% |
| K=12, B=6, R=2 | 48 bytes | 100% | 41.8% |
| K=16, B=8, R=2 | 64 bytes | 100% | 50.7% |

**Selected: K=8, B=4, R=2** (32 bytes per trace, 4 parquet columns).

Critical finding: **R (rows per band) dominates false negative behavior.** The initial proposal (R=4) had 100% false negative rate on the real scenario. R=2 reduced this to 0.4%.

**Reproduce:** `cd docs/design-proposals/similar-to-validation && go run .`

## Single-block validation

Analyzed 10,000 traces from block `0005fda8` (Apr 11, 27 MB).

### Signature set statistics

| Metric | Value |
|--------|-------|
| Traces analyzed | 10,000 |
| Spans/trace: min, p50, p90, max | 1, 1, 3, 77 |
| Signatures/trace: min, p50, p90, max | 1, 1, 2, 19 |
| Unique structural shapes | 147 |
| Singleton shapes (1 trace only) | 46 (31.3%) |

Most traces in this block are single-span database operations. Multi-service API flows (4+ signatures) are the minority but are the primary target for structural similarity.

### Structural cohorts

Top cohorts by trace count:

| Traces | Sigs | Root service | Root operation |
|--------|------|-------------|----------------|
| 2,201 | 1 | neon-console | Postgres GetContext |
| 1,250 | 1 | neon-billing-manager | Postgres Exec |
| 1,013 | 1 | neon-control-plane | HTTP internal-computeclient GetComputeState |
| 962 | 1 | neon-control-plane | Postgres Get |
| 873 | 1 | neon-console | Postgres Get |
| 106 | 4 | neon-console | GetProjectEndpoint |
| 63 | 2 | neon-console | PostUsageEventBatch |

### Within-cohort matching

Traces with identical signature sets must produce identical MinHash bands.

| Pairs tested | Matches | Rate |
|-------------|---------|------|
| 36,773 | 36,773 | **100.0%** |

Zero false negatives for exact structural matches.

### Cross-cohort false positives

Different structural shapes should mostly NOT match on MinHash bands.

| Pairs tested | Matches | False positive rate |
|-------------|---------|-------------------|
| 435 | 5 | **1.1%** |

The 5 cross-matches were genuine partial overlaps:

| Cohort A | Cohort B | Exact Jaccard | Estimated Jaccard |
|----------|----------|--------------|-------------------|
| neon-console\|Postgres Get (873t) | PostUsageEventBatch (63t) | 0.500 | 0.62 |
| neon-console\|Postgres Get (873t) | another shape (31t) | 0.125 | 0.38 |
| shape (63t) | shape (31t) | 0.111 | 0.38 |
| shape (55t) | shape (43t) | 0.600 | 0.88 |
| shape (39t) | shape (31t) | 0.786 | 0.88 |

**Reproduce:** `go test ./pkg/minhash/ -run TestBlockValidation -v`

## Cross-block validation

Compared all 5 blocks pairwise to test whether MinHash finds the same structural shapes across time windows.

### Same service, same time range

Block `0005fda8` (Apr 11) vs `fe87871b` (Apr 13) — same tenant, 2 days apart:

| Result | Count |
|--------|-------|
| **Exact match** | 28 |
| Similar (J ≥ 0.5) | 1 |
| Not found | 1 |

28 of 30 cohorts matched exactly across a 2-day gap. The neon-console and neon-control-plane services produce stable, repeatable trace shapes.

The one similar match was `neon-console|ListProjectEndpoints` (8 sigs → different shape with J=0.50), indicating a code change between the two blocks that altered the downstream call graph.

### Same service, different time range

Pageserver blocks from different dates:

| Block pair | Exact | Similar | Not found |
|-----------|-------|---------|-----------|
| `3ffaa5ab` (May 4 01:41) vs `808eac91` (May 4 20:29) | 7/7 | 0 | 0 |
| `3ffaa5ab` (May 4) vs `c2ff3990` (May 7) | 2/7 | 5 | 0 |
| `808eac91` (May 4) vs `c2ff3990` (May 7) | 3/9 | 6 | 0 |

Same-day blocks match exactly (7/7). Cross-day blocks show slight drift (J=0.73–0.93) but **all cohorts are found** — either exact or via band similarity. Zero shapes lost.

Jaccard values for the SIMILAR matches:

| Cohort (sigs) | Jaccard | Interpretation |
|--------------|---------|----------------|
| GET_VECTORED (10 sigs) | 0.91 | 1 span added/removed |
| GET_PAGE (14 sigs) | 0.93 | 1 span added/removed |
| GET_VECTORED (8 sigs) | 0.73 | 2-3 spans changed |
| GET_PAGE (11 sigs) | 0.85 | 1-2 spans changed |
| GET_VECTORED (9 sigs) | 0.82 | 1-2 spans changed |

### Different services

neon-console shapes (blocks `0005fda8`, `fe87871b`) vs pageserver shapes (blocks `3ffaa5ab`, `808eac91`, `c2ff3990`):

**Zero cross-service matches.** No neon-console shape ever matched a pageserver shape. MinHash correctly separates completely unrelated service topologies.

### Temporal stability

Shapes present across all 5 blocks:

| Shape | Sigs | Blocks | Trace counts per block |
|-------|------|--------|----------------------|
| pageserver\|GET_PAGE | 6 | 5/5 | 50, 2693, 249, 1, 266 |
| pageserver\|GET_PAGE | 13 | 5/5 | 10, 115, 109, 15, 6 |
| pageserver\|GET_VECTORED | 10 | 4/5 | 7, 2720, 238, 175 |
| pageserver\|GET_PAGE | 14 | 4/5 | 75, 341, 2, 73 |
| pageserver\|GET_VECTORED | 11 | 4/5 | 62, 2, 1, 97 |

The core pageserver shapes are stable across a month of data. Trace counts vary (traffic volume) but the structural shape is consistent.

neon-console shapes appear in 2/5 blocks (the April blocks only), which is expected — the May blocks contain different workloads from the same tenant.

**Reproduce:** `go test ./pkg/minhash/ -run TestCrossBlock -v`

## Computation overhead

Benchmarked on Apple M3 Pro. Computation runs at block build time, not on the span ingestion hot path.

| Trace size (unique sigs) | Compute + ToBands | Allocations |
|--------------------------|-------------------|-------------|
| 5 | 0.6 µs | 0 |
| 15 (typical) | 2.0 µs | 0 |
| 30 | 3.6 µs | 0 |
| 50 | 5.7 µs | 0 |
| 100 (large) | 11 µs | 0 |

Filter check (`AnyBandMatches`): **1.4 ns** — four uint64 comparisons.

For a block with 100k traces at 15 signatures each: 100k × 2 µs = **0.2 seconds** added to a block build that takes minutes.

**Reproduce:** `go test ./pkg/minhash/ -bench=. -benchmem`

## Limitations and open questions

### Single-span traces

Most traces in the production dataset have 1 span and 1 signature. For these, MinHash is equivalent to an exact string hash — it adds no fuzzy matching value. The design is most useful for multi-service traces (4+ signatures) which are the minority in this dataset but are the primary target for the baseline use case.

### Instrumentation consistency

The production data shows consistent attribute naming within this tenant (`Postgres GetContext`, `HTTP orb FetchSubscriptionsByAccountID`). We have not validated against tenants with:
- Mixed instrumentation libraries (auto-instrumentation vs manual)
- `span.name` set to raw URLs instead of route templates (`/users/12345` vs `/users/{id}`)
- Inconsistent semantic attribute versions (`http.method` vs `http.request.method`)

These would produce different signatures for functionally identical code paths.

### Shape drift over time

The SIMILAR matches (J=0.73–0.93) between May 4 and May 7 blocks suggest that pageserver shapes evolve slightly over days — likely due to code changes, feature flags, or conditional paths. The MinHash band filter catches these (they pass), but a user looking for an "exact same shape" baseline would also see these near-misses.

### Dataset diversity

All blocks are from a single tenant. Cross-tenant validation is needed to confirm the approach works for different instrumentation patterns, service counts, and trace depths. The current dataset is biased toward single-span DB traces.
