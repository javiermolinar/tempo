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
  - [Production: Grafana Cloud tenants](#production-grafana-cloud-tenants)
- [Parameter selection](#parameter-selection)
- [Tenant A: cloud infrastructure platform](#tenant-a-cloud-infrastructure-platform)
  - [Single-block validation](#single-block-validation-tenant-a)
  - [Cross-block validation](#cross-block-validation-tenant-a)
- [Tenant B: mobile services platform](#tenant-b-mobile-services-platform)
  - [Single-block validation](#single-block-validation-tenant-b)
  - [Cross-block validation](#cross-block-validation-tenant-b)
- [Cross-tenant summary](#cross-tenant-summary)
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

### Production: Grafana Cloud tenants

Two tenants from Grafana Cloud, randomly selected, with 5 parquet blocks each.

**Tenant A** — cloud infrastructure platform (API + storage engine), April 11 – May 7:

| Block | Date | Traces | Shapes | Size |
|-------|------|--------|--------|------|
| `0005fda8` | Apr 11 | 123,681 | 188 | 27 MB |
| `fe87871b` | Apr 13 | 149,895 | 162 | 43 MB |
| `3ffaa5ab` | May 4 | 5,878 | 7 | 10 MB |
| `808eac91` | May 4 | 610 | 9 | 1 MB |
| `c2ff3990` | May 7 | 17 | 3 | 84 KB |

Services: `neon-console`, `neon-control-plane`, `neon-billing-manager` (API + DB), `pageserver` (storage engine).

**Tenant B** — mobile services platform (20+ microservices), April 12 – May 3:

| Block | Date | Traces | Shapes | Size |
|-------|------|--------|--------|------|
| `ff25fd28` | Apr 12 | 179,559 | 167 | 48 MB |
| `7ef9ac17` | Apr 17 | 180,451 | 104 | 46 MB |
| `0006532d` | Apr 20 | 195,792 | 232 | 56 MB |
| `3dd7c298` | Apr 25 | 358,864 | 142 | 86 MB |
| `d5995959` | May 3 | 181,861 | 144 | 47 MB |

Services: `admin-web`, `plan-management`, `user-service`, `order`, `line-management`, `line-operations`, `usmobile-gateway`, `subscriber-web`, `security-rule-based-engine-service`, `pool`, `emailservice`, `notification-service`, `searchservice`, `device-identifier-service`, `rewards`, `order-exports`, `metering-engine-reports`, `metering-engine-enrichment`, `tmobile-mvnoc`, `esimservice`, `payment-service`, `shipments`, and others.

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

## Tenant A: cloud infrastructure platform

### Single-block validation {#single-block-validation-tenant-a}

Analyzed 10,000 traces from block `0005fda8` (Apr 11, 27 MB).

#### Signature set statistics

| Metric | Value |
|--------|-------|
| Traces analyzed | 10,000 |
| Spans/trace: min, p50, p90, max | 1, 1, 3, 77 |
| Signatures/trace: min, p50, p90, max | 1, 1, 2, 19 |
| Unique structural shapes | 147 |
| Singleton shapes (1 trace only) | 46 (31.3%) |

Most traces in this block are single-span database operations. Multi-service API flows (4+ signatures) are the minority but are the primary target for structural similarity.

#### Structural cohorts

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

#### Within-cohort matching

Traces with identical signature sets must produce identical MinHash bands.

| Pairs tested | Matches | Rate |
|-------------|---------|------|
| 36,773 | 36,773 | **100.0%** |

Zero false negatives for exact structural matches.

#### Cross-cohort false positives

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

**Reproduce:** `TEMPO_BLOCK_PATH=/path/to/block go test ./pkg/minhash/ -run TestBlockValidation -v`

### Cross-block validation {#cross-block-validation-tenant-a}

Compared all 5 blocks pairwise to test whether MinHash finds the same structural shapes across time windows.

#### Same service, same time range

Block `0005fda8` (Apr 11) vs `fe87871b` (Apr 13) — same tenant, 2 days apart:

| Result | Count |
|--------|-------|
| **Exact match** | 28 |
| Similar (J ≥ 0.5) | 1 |
| Not found | 1 |

28 of 30 cohorts matched exactly across a 2-day gap. The neon-console and neon-control-plane services produce stable, repeatable trace shapes.

The one similar match was `neon-console|ListProjectEndpoints` (8 sigs → different shape with J=0.50), indicating a code change between the two blocks that altered the downstream call graph.

#### Same service, different time range

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

#### Different services

neon-console shapes (blocks `0005fda8`, `fe87871b`) vs pageserver shapes (blocks `3ffaa5ab`, `808eac91`, `c2ff3990`):

**Zero cross-service matches.** No neon-console shape ever matched a pageserver shape. MinHash correctly separates completely unrelated service topologies.

#### Temporal stability

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

**Reproduce:** `TEMPO_BLOCKS_DIR=/path/to/blocks go test ./pkg/minhash/ -run TestCrossBlock -v`

## Tenant B: mobile services platform

### Single-block validation {#single-block-validation-tenant-b}

Analyzed 20,000 traces from block `0006532d` (Apr 20, 56 MB).

#### Signature set statistics

| Metric | Value |
|--------|-------|
| Traces analyzed | 20,000 |
| Unique structural shapes | 232 |
| Multi-signature shapes | 104 |
| Top services | admin-web, plan-management, user-service, order, line-management |

This tenant has significantly more services (20+) than Tenant A. The top cohorts are health checks and PING operations across all microservices, but there are also multi-span messaging flows (`security-rule-based-engine-service|user.request.server.response process` with 3 sigs).

#### Top cohorts

| Traces | Sigs | Root service | Root operation |
|--------|------|-------------|----------------|
| 3,091 | 1 | admin-web | PING |
| 1,634 | 1 | plan-management | PING |
| 859 | 1 | user-service | PING |
| 793 | 1 | order | PING |
| 771 | 1 | line-management | PING |
| 686 | 1 | admin-web | GET /manage/health/** |
| 533 | 2 | usmobile-gateway | GET /manage/health |
| 480 | 2 | subscriber-web | GET /manage/health |
| 190 | 1 | security-rule-based-engine-service | basic.ack |
| 186 | 3 | security-rule-based-engine-service | user.request.server.response process |

### Cross-block validation {#cross-block-validation-tenant-b}

Compared all 5 blocks pairwise (Apr 12 – May 3, spanning 21 days).

#### Cross-block matching results

| Block pair | Time gap | Exact | Similar | Not found | Total |
|-----------|----------|-------|---------|-----------|-------|
| Apr 20 vs Apr 25 | 5 days | 27 | 0 | 3 | 30 |
| Apr 20 vs Apr 17 | 3 days | 27 | 0 | 3 | 30 |
| Apr 20 vs May 3 | 13 days | **30** | 0 | 0 | 30 |
| Apr 20 vs Apr 12 | 8 days | 28 | 0 | 2 | 30 |
| Apr 25 vs Apr 17 | 8 days | 24 | 0 | 6 | 30 |
| Apr 25 vs May 3 | 8 days | 25 | 0 | 5 | 30 |
| Apr 25 vs Apr 12 | 13 days | **30** | 0 | 0 | 30 |
| Apr 17 vs May 3 | 16 days | **30** | 0 | 0 | 30 |
| Apr 17 vs Apr 12 | 5 days | **30** | 0 | 0 | 30 |
| May 3 vs Apr 12 | 21 days | **30** | 0 | 0 | 30 |

**Zero false positives. Zero SIMILAR matches.** Every match across blocks is either EXACT or NOT FOUND.

The NOT FOUND cases are RabbitMQ operations (`usmobile-gateway|basic.nack`, `pool|basic.ack`, `usmobile-gateway|queue.declare`, `usmobile-gateway|basic.consume`) that only appear intermittently — these are genuinely absent workloads, not signature drift.

#### Temporal stability

**69 shapes present in ALL 5 blocks** over a 21-day span.

Top stable shapes:

| Shape | Sigs | Trace counts per block (Apr 12 → May 3) |
|-------|------|-------------------------------------------|
| admin-web\|PING | 1 | 3057, 3254, 3091, 1598, 3158 |
| plan-management\|PING | 1 | 1671, 1826, 1634, 828, 1728 |
| user-service\|PING | 1 | 833, 880, 859, 391, 869 |
| line-management\|PING | 1 | 828, 784, 771, 410, 873 |
| order\|PING | 1 | 791, 805, 793, 363, 789 |
| usmobile-gateway\|GET /manage/health | 2 | 542, 578, 533, 267, 583 |
| subscriber-web\|GET /manage/health | 2 | 502, 490, 480, 251, 477 |

Trace shapes are extremely consistent for this tenant. The same services produce the same operations week after week. Traffic volume varies but structural identity is stable.

**Reproduce:** `TEMPO_BLOCKS_DIR=/path/to/blocks go test ./pkg/minhash/ -run TestCrossBlock -v`

## Cross-tenant summary

| Property | Tenant A (infra platform) | Tenant B (mobile services) |
|----------|--------------------------|---------------------------|
| Services | 4 main | 20+ |
| Blocks tested | 5 (Apr 11 – May 7) | 5 (Apr 12 – May 3) |
| Traces analyzed | ~280k | ~1.1M |
| Unique shapes | 232 | 373 |
| Multi-sig shapes | 143 | 104 |
| Within-cohort match rate | **100%** | **100%** |
| Cross-cohort false positive rate | **1.1%** | **0%** |
| Cross-block exact match rate (best pair) | 28/30 (93%) | 30/30 (100%) |
| Cross-block not-found (worst pair) | 29/30 (different workloads) | 6/30 (intermittent queues) |
| Shapes stable across all blocks | 2 | 69 |
| SIMILAR matches (J < 1.0) | Yes (pageserver drift, J=0.73–0.93) | None — all exact or absent |
| Zero false positives cross-service | ✓ | ✓ |

**Key takeaways:**

1. **MinHash produces zero false negatives for exact structural matches** in both tenants (100% within-cohort match rate).
2. **Cross-block matching works across weeks** — Tenant B achieves 30/30 exact matches over a 21-day span.
3. **False positive rate is very low** (0–1.1%), and the false positives are genuine partial overlaps (shared operations), not random noise.
4. **Shape stability varies by tenant** — Tenant A shows some drift in the pageserver service (J=0.73–0.93), Tenant B shows perfect stability. MinHash handles both: exact matches pass, near-matches pass via band similarity, unrelated shapes are filtered.
5. **The NOT FOUND cases are correct** — they represent genuinely different workloads (different services in different time windows, or intermittent queue operations), not MinHash failures.

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

## Bad matches: real-world false positives

Exhaustive pairwise comparison of all 303 unique shapes in Tenant B's largest block (50,000 traces, 45,753 cohort pairs) found **223 band matches (0.49%)**.

Of those, **14 had Jaccard < 0.1** — traces that `similar_to()` would pass but share almost no structure.

### The root cause: generic HTTP span names

Every worst match shares the same signature: `subscriber-web|GET|GET`. This is a span where:
- `service.name` = `subscriber-web`
- `span.name` = `GET` (the HTTP method, not the endpoint)
- `http.method` = `GET`
- `http.route` = absent

Without `http.route`, the signature cannot distinguish between different API endpoints. Every GET request to `subscriber-web` produces the same signature regardless of whether it hits `/api/v1/configuration`, `/mobile/v1/internal-port`, or `/api/v1/pools/transfer`.

### Worst matches

| Jaccard | Trace A | Trace B | Shared signatures |
|---------|---------|---------|-------------------|
| 0.059 | GET /api/v1/configuration (11 sigs) | GET /mobile/v1/app-config (7 sigs) | `subscriber-web\|GET\|GET` |
| 0.059 | GET /mobile/v1/internal-port (5 sigs) | InternalPortCleanupCron (13 sigs) | `usmobile-gateway\|find internal_ports` |
| 0.060 | POST /api/v1/oauth/login (32 sigs) | GET /api/v1/freetrial/eligibility (21 sigs) | `subscriber-web\|GET\|GET` + 2 shared DB ops |
| 0.071 | GET /api/v1/configuration (11 sigs) | GET (4 sigs) | `subscriber-web\|GET\|GET` |
| 0.071 | GET /api/v1/pools/transfer (4 sigs) | GET /api/v1/configuration (11 sigs) | `subscriber-web\|GET\|GET` |

These are **completely different API flows** that collide because one generic span signature (`GET|GET`) appears in both. A user querying `similar_to()` for a `/api/v1/configuration` trace would also get `/api/v1/pools/transfer` traces — that's garbage.

### Jaccard distribution of all band matches

```
[0.0-0.1)  14  ██████████████
[0.1-0.2)  45  █████████████████████████████████████████████
[0.2-0.3)  54  ██████████████████████████████████████████████████████
[0.3-0.4)  24  ████████████████████████
[0.4-0.5)   9  █████████
[0.5-0.6)  18  ██████████████████████████
[0.6-0.7)  26  ██████████████████████████
[0.7-0.8)   9  █████████
[0.8-0.9)  13  █████████████
[0.9-1.0)  11  ███████████
```

113 of 223 band matches (51%) have Jaccard < 0.4. These are the false positives that the other predicates in the query (`service.name`, `name`, `status`) must eliminate.

### Mitigation: signature composition strategies tested

Five signature composition strategies were tested against the same block (50,000 traces, exhaustive pairwise comparison):

| Strategy | Band matches | J<0.1 | J<0.2 | J≥0.7 | Empty traces | Worst J |
|---|---|---|---|---|---|---|
| **Baseline (current)** | 223 | 14 | 59 | 33 | 0 | 0.059 |
| Drop HTTP-method-only spans | 187 | 10 | 39 | 30 | 2 | 0.062 |
| Replace HTTP-method-only with sentinel | 199 | 3 | 34 | 34 | 0 | 0.059 |
| **Drop HTTP-method-only + exclude http.method** | **148** | **4** | **20** | **29** | **2** | **0.085** |
| Aggressive generic drop (also short names) | 130 | 4 | 20 | 27 | 26,993 | 0.085 |

**Winner: "Drop HTTP-method-only + exclude http.method from sigs"**

Two changes from the baseline:

1. **Drop spans where `span.name` is a bare HTTP method (`GET`, `POST`, etc.) and no `http.route` is present.** These produce generic signatures like `subscriber-web|GET|GET` that collide across unrelated endpoints. Only 1,400 spans dropped across 50,000 traces (2 traces become empty).

2. **Exclude `http.method` / `http.request.method` from the semantic attribute allowlist.** When the span name already contains the HTTP method (e.g., `GET /api/v1/configuration`), including `http.method=GET` in the signature is redundant. For well-instrumented spans it adds noise; for poorly-instrumented spans (bare method names) it creates the collision problem.

Results vs baseline:
- Bad matches (J<0.2): **59 → 20** (66% reduction)
- Very bad matches (J<0.1): **14 → 4** (71% reduction)
- Good matches (J≥0.7): 33 → 29 (minimal loss)
- Worst match Jaccard: **0.059 → 0.085**

The remaining worst match (`GET /mobile/v2/profile` vs `GET /api/v1/announcement`, J=0.085) shares 4 real DB operations (`UserRepository.findById`, `UserRoleAssignmentRepository.findByAccountIdAndUserId`, etc.) — a legitimate partial overlap from shared authentication middleware, not a generic-span collision.

### Updated semantic attribute allowlist

Based on the strategy comparison, the recommended allowlist excludes `http.method` / `http.request.method`:

| Protocol | Attributes included |
|----------|-------------------|
| HTTP | `http.route` |
| RPC | `rpc.service`, `rpc.method` |
| Database | `db.system`, `db.operation`, `db.name` |
| Messaging | `messaging.system`, `messaging.operation`, `messaging.destination.name` |

The aggressive strategy (also dropping spans with short names ≤ 4 chars) was rejected — it kills 27,000 traces (54%) for no additional improvement in bad match rate.

**Reproduce:**
- Bad matches: `TEMPO_BLOCK_PATH=/path/to/block go test ./pkg/minhash/ -run TestBadMatches -v`
- Strategy comparison: `TEMPO_BLOCK_PATH=/path/to/block go test ./pkg/minhash/ -run TestSignatureStrategies -v`

## Compaction impact

Higher compaction levels merge trace fragments from multiple blocks into a single block, producing more complete traces with richer signature sets. This should improve MinHash quality because:

1. A trace split across two level-1 blocks might have 5 signatures in each. After compaction merges them into one level-3 block, the trace has up to 10 signatures — the full structural picture.
2. MinHash computed on the complete signature set is more accurate than MinHash computed on partial sets.
3. The `assignNestedSetModelBoundsAndServiceStats` function (where MinHash is computed) runs on the merged trace during compaction, automatically recomputing bands from the full span set.

**This is validated by design, not empirically.** Our test blocks contain different trace IDs at different compaction levels — we cannot compare the same trace pre- vs post-compaction. Testing the compaction effect would require capturing the same trace ID in both a level-1 block (fragmented) and a level-4 block (merged), which requires a controlled environment.

What we did observe across compaction levels:

| Compaction | Blocks | Avg sig p50 | Avg span p50 | J<0.1 | J<0.2 |
|---|---|---|---|---|---|
| 1 | 1 | 13 | 23 | 0 | 0 |
| 3 | 7 | 3 | 6 | 0 | 1 |
| 4 | 2 | 1 | 1 | 0 | 4 |

These differences reflect workload mix (which services land in which blocks), not compaction quality. The level-1 block contains only pageserver traces (deep call graphs), while level-4 blocks are dominated by single-span DB traces.

For the baseline use case — "find a healthy trace from hours/days ago" — the target blocks will typically be at compaction level 3+ where most traces are fully merged. MinHash quality is highest in exactly this scenario.

**Reproduce:** `go test ./pkg/minhash/ -run TestCompactionImpact -v`

## Recommended instrumentation: trace fragmentation metrics

The compaction hypothesis ("higher compaction = better MinHash") cannot be validated from static blocks because we can't observe the same trace pre- and post-compaction. A runtime metric would provide this data.

**Proposed metric:** emit trace fragmentation statistics during trace-by-ID lookups.

During a trace-by-ID request, the frontend shards the lookup across queriers and ingesters. Each querier calls `tempodb.Find()` against its blocks and returns partial traces. The frontend's `TraceByIDCombiner` merges them. Today, `TraceByIDMetrics` only carries `inspectedBytes`. Extending it would give fragmentation visibility:

```proto
message TraceByIDMetrics {
    uint64 inspectedBytes = 1;
    uint32 blocksWithTrace = 2;           // blocks that contained fragments
    repeated uint32 compactionLevels = 3; // compaction levels of those blocks
}
```

The `TraceByIDMetricsCombiner` at the frontend sums `blocksWithTrace` and unions `compactionLevels` across all querier and ingester responses. Then emits:

- `tempo_trace_by_id_fragments_total` — histogram of how many blocks contained fragments per lookup.
- `tempo_trace_by_id_fragment_compaction_level` — histogram of compaction levels of blocks that contained fragments.

This would answer:
- **How fragmented are traces in practice?** If p50 fragments = 1, most traces are in one block and MinHash is accurate at first flush. If p50 = 3, compaction is critical.
- **At what compaction level do traces consolidate?** If most fragments are at level 1 and single fragments are at level 3+, compaction is doing its job.
- **Does fragmentation vary by tenant/workload?** Short-lived traces (API calls) vs long-running traces (batch jobs) likely have different fragmentation profiles.

This is independent of the `similar_to()` feature — it's generally useful for understanding Tempo's trace assembly behavior and would validate the MinHash compaction assumptions with production data.

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

Two tenants have been validated. More tenants with different characteristics would strengthen confidence:
- Tenants with deep call graphs (10+ services per trace)
- Tenants with mixed instrumentation (auto + manual)
- Tenants with high-cardinality span names (raw URLs instead of route templates)
- Tenants with frequent code deploys causing shape drift
