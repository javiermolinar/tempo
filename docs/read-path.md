# Tempo Read Path — Architecture Deep Dive

This document describes how a query flows through Tempo's read path, from HTTP/gRPC
entry point to storage-level data retrieval. It covers every major component and how
they interact.

---

## High-Level Overview

```
                                    ┌─────────────────────────┐
                                    │       Client            │
                                    │   (Grafana / API)       │
                                    └───────────┬─────────────┘
                                                │
                                        HTTP / gRPC
                                                │
                                                ▼
                                    ┌─────────────────────────┐
                                    │    Query Frontend        │
                                    │                         │
                                    │  1. Pipeline middleware  │
                                    │  2. Sharding into jobs  │
                                    │  3. Per-tenant queuing  │
                                    │  4. Batch dispatch      │
                                    └───────────┬─────────────┘
                                                │
                                        gRPC streams
                                    (batches of HTTP requests)
                                                │
                                                ▼
                                    ┌─────────────────────────┐
                                    │       Querier            │
                                    │                         │
                                    │  Executes each job:     │
                                    │  - Live Store (recent)  │
                                    │  - Object Store (blocks)│
                                    │  - External (optional)  │
                                    └───────────┬─────────────┘
                                                │
                              ┌─────────────────┼──────────────────┐
                              │                 │                  │
                              ▼                 ▼                  ▼
                      ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
                      │  Live Store  │  │ Object Store │  │   External   │
                      │  (Kafka +    │  │  (S3/GCS/    │  │   Backend    │
                      │   WAL)       │  │   Azure)     │  │  (optional)  │
                      └──────────────┘  └──────────────┘  └──────────────┘
```

---

## 1. Query Entry Points

Tempo supports multiple query types, each entering through dedicated HTTP and gRPC handlers:

| Query Type          | HTTP Handler                    | gRPC Handler (streaming)          |
|---------------------|---------------------------------|-----------------------------------|
| Trace by ID         | `TraceByIDHandler`              | —                                 |
| TraceQL Search      | `SearchHandler`                 | `StreamingQuerier.Search`         |
| Tag Names           | `SearchTagsHandler`             | `StreamingQuerier.SearchTags`     |
| Tag Names V2        | `SearchTagsV2Handler`           | `StreamingQuerier.SearchTagsV2`   |
| Tag Values          | `SearchTagsValuesHandler`       | `StreamingQuerier.SearchTagValues`|
| Tag Values V2       | `SearchTagsValuesV2Handler`     | `StreamingQuerier.SearchTagValuesV2` |
| Metrics Query Range | `MetricsQueryRangeHandler`      | `StreamingQuerier.MetricsQueryRange` |
| Metrics Instant     | `MetricsQueryInstantHandler`    | `StreamingQuerier.MetricsQueryInstant` |

Both HTTP and gRPC entry points converge into the same **pipeline** for a given query type.

Source: `modules/frontend/frontend.go`

---

## 2. The Query Frontend

The Query Frontend is the central coordinator of the read path. It does **not** execute
queries itself. Instead, it breaks queries into jobs, queues them, and dispatches them to
Queriers.

### 2.1 Pipeline Architecture

Each query type has a dedicated pipeline built using `pipeline.Build()`. A pipeline is
a chain of middleware, split into two phases:

```
  ┌────────── Async Phase ──────────┐   ┌────────── Sync Phase ──────────┐
  │                                 │   │                                │
  │  header strip → adjust time →   │   │  cache → status code adjust   │
  │  deny list → weight assign →    │──▶│  → retry                      │
  │  multi-tenant → tenant valid →  │   │                                │
  │  sharding                       │   │                                │
  └─────────────────────────────────┘   └────────────────────────────────┘
                                                      │
                                                      ▼
                                              RoundTripper (v1.Frontend)
                                              enqueues into RequestQueue
```

**Async middleware** (1:N fan-out): Can produce multiple sub-requests from one input.
This is where sharding and multi-tenant expansion happen.

**Sync middleware** (1:1): Each sub-request passes through caching, status code
adjustment, and retry logic before being dispatched.

The final `RoundTripper` at the end of the sync pipeline is the v1 Frontend, which
enqueues each job into the `RequestQueue`.

Source: `modules/frontend/pipeline/pipeline.go`, `modules/frontend/pipeline/readme.md`

#### Key Middleware

| Middleware                | Phase | Purpose |
|---------------------------|-------|---------|
| `StripHeadersWare`        | Async | Removes non-allowed headers |
| `AdjustStartEndWare`      | Async | Applies `query_backend_after` and `query_end_cutoff` |
| `URLDenyListWare`          | Async | Blocks blacklisted URL patterns |
| `QueryValidatorWare`      | Async | Enforces max query expression size |
| `WeightRequestWare`       | Async | Assigns a weight to each job (see §2.3) |
| `MultiTenantMiddleware`   | Async | Splits request per tenant if multi-tenant |
| `TenantValidatorMiddleware`| Async | Validates tenant ID |
| `Sharder` (per query type) | Async | Splits into N sub-jobs (see §2.2) |
| `CachingWare`             | Sync  | Frontend-level result cache |
| `StatusCodeAdjustWare`    | Sync  | Remaps querier error codes for the pipeline |
| `RetryWare`               | Sync  | Retries failed jobs (default: 2 retries) |

### 2.2 Sharding (Job Creation)

Sharding is what transforms a single user query into many parallel jobs. Each query
type has its own sharder:

#### TraceQL Search (`asyncSearchSharder`)

A search query is split into:
1. **Live Store jobs** (1–3): Cover recent data based on `query_backend_after`.
   The recent time range is split into `ingester_shards` sub-ranges (default: 3).
   Note: the config key retains the legacy "ingester" naming.
2. **Backend jobs** (many): Each block in the time range is divided into page-ranges
   based on `target_bytes_per_job` (default: 100MB). A block with 1GB of data produces
   ~10 jobs.

Jobs are pushed into a buffered channel. Backend jobs are generated asynchronously in a
goroutine while Live Store jobs are sent immediately. A `ConcurrentRequests` limit
(default: 1000) caps how many jobs execute in parallel within the pipeline.

Blocks are sorted newest-first and grouped into shards (max `most_recent_shards`,
default: 200). The shard tracker allows the combiner to detect when all results for a
time window have been received, enabling early termination.

Source: `modules/frontend/search_sharder.go`

#### Trace by ID (`asyncTraceSharder`)

The block ID space (`0x00..00` to `0xff..ff`) is divided into `query_shards`
(default: 50) ranges using precomputed block boundaries. Each shard scans blocks whose
IDs fall within its range. One additional job targets the Live Store (and optionally
an external backend).

Source: `modules/frontend/traceid_sharder.go`

#### Metrics Query Range (`asyncQueryRangeSharder`)

Similar to search: blocks are identified by time overlap, divided into page-based jobs,
and dispatched concurrently.

Source: `modules/frontend/metrics_query_range_sharder.go`

#### Tags / Tag Values (`asyncTagSharder`)

Same block-based sharding as search, with ingester and backend splits.

Source: `modules/frontend/tag_sharder.go`

### 2.3 Weight Assignment

Each job receives a weight before being enqueued. Weights are used during batching to
limit how much work a single querier receives.

```
┌─────────────────────────────────────────────────────────┐
│  Query Type         │  Base Weight  │  Increments       │
├─────────────────────┼───────────────┼───────────────────┤
│  Default            │  1            │  —                │
│  Trace by ID        │  2            │  —                │
│  TraceQL Search     │  1            │  +1 complex query │
│                     │               │  +1 OR operations │
│                     │               │  +1 full trace    │
│                     │               │  +1 select all    │
│  TraceQL Metrics    │  (same as search)                 │
│  Retry              │  current + 1  │  per retry        │
└─────────────────────────────────────────────────────────┘
```

Max weight a single query can reach: **5** (TraceQL with all flags).

Source: `modules/frontend/pipeline/async_weight_middleware.go`

### 2.4 Per-Tenant Request Queue

After pipeline processing, each job enters the `RequestQueue`:

```
                    EnqueueRequest(tenantID, job)
                              │
                              ▼
            ┌─────────────────────────────────┐
            │         RequestQueue             │
            │                                 │
            │  tenant-A: [job1, job2, job3]   │  ← buffered channel (max: 2000)
            │  tenant-B: [job4, job5]         │
            │  tenant-C: [job6]               │
            │                                 │
            │  Round-robin tenant selection    │
            │  Lock held only during dequeue  │
            └─────────────────────────────────┘
```

Each tenant gets a buffered Go channel (capacity: `max_outstanding_per_tenant`,
default: 2000). When a querier asks for work, the queue:

1. Selects the next tenant via round-robin (fairness).
2. Dequeues a **batch** of jobs from that tenant's channel.
3. Returns the batch to the querier.

Source: `modules/frontend/queue/queue.go`, `modules/frontend/queue/user_queues.go`

### 2.5 Batching and Dispatch

When a Querier worker calls `GetNextRequestForQuerier`, the queue builds a batch
using `getBatchBuffer`:

```go
func getBatchBuffer(batchBuffer []Request, userID string, queue chan Request) []Request {
    requestedCount := len(batchBuffer)  // == MaxBatchSize (default: 7)
    guaranteedInQueue := min(len(queue), requestedCount)

    totalWeight := 0
    for i := range guaranteedInQueue {
        batchBuffer[i] = <-queue
        totalWeight += batchBuffer[i].Weight()
        if totalWeight >= requestedCount {  // weight limit == size limit
            break
        }
    }
    return batchBuffer[:actuallyInBatch]
}
```

**Key property**: `max_batch_size` (default: 7) acts as **both** the maximum number of
jobs **and** the maximum total weight. Since max query weight is 5, a batch of 2 heavy
jobs (weight 3+4=7) already hits the cap, leaving the batch physically small (2 jobs)
but at maximum weight.

The batch is sent over the gRPC stream as a `FrontendToClient` proto message:
- Single job: `Type_HTTP_REQUEST` (backwards compatibility)
- Multiple jobs: `Type_HTTP_REQUEST_BATCH`

The querier processes the batch and sends back `ClientToFrontend` with the responses.

Source: `modules/frontend/v1/frontend.go`, `modules/frontend/v1/request_batch.go`

### 2.6 Response Collection

Responses flow back through the pipeline in reverse. Two collector types aggregate
results:

**HTTP Collector** (`httpCollector`): Collects all responses, passes them to a
`Combiner`, and returns a single `http.Response`.

**gRPC Collector** (`GRPCCollector`): Same as HTTP, but periodically sends streaming
diffs (every 500ms) via `srv.Send()` for real-time results.

Combiners are query-type-specific:
- **Search**: `TypedSearch` — accumulates trace metadata, deduplicates, sorts by time, enforces limit
- **TraceByID**: `TypedTraceByID` — merges partial traces from multiple sources
- **Metrics**: `QueryRangeCombiner` — aggregates time series across shards
- **Tags**: Deduplicates tag names/values across shards

The collector cancels remaining in-flight jobs when:
- The result limit is reached (e.g., 20 search results)
- An error occurs
- The client disconnects
- The combiner signals `ShouldQuit()`

Source: `modules/frontend/pipeline/collector_http.go`, `modules/frontend/pipeline/collector_grpc.go`

---

## 3. The Querier

Queriers are stateless workers that execute individual jobs received from the Query
Frontend. They don't know about the original query — they only see the sharded sub-request.

### 3.1 Worker Architecture

Each Querier runs a `querierWorker` that manages connections to Query Frontend pods:

```
┌──────────────────────────────────────────────────┐
│                  Querier Pod                     │
│                                                  │
│  ┌────────────────────────────────────────────┐  │
│  │           querierWorker                    │  │
│  │                                            │  │
│  │  DNS discovery → Frontend pod addresses    │  │
│  │                                            │  │
│  │  Per Frontend pod:                         │  │
│  │    processorManager                        │  │
│  │      ├─ goroutine 1 (stream)              │  │
│  │      ├─ goroutine 2 (stream)              │  │
│  │      ├─ ...                               │  │
│  │      └─ goroutine N (stream)              │  │
│  │                                            │  │
│  │  N = parallelism (default: 10)            │  │
│  └────────────────────────────────────────────┘  │
│                                                  │
│  ┌────────────────────────────────────────────┐  │
│  │      frontendProcessor                     │  │
│  │                                            │  │
│  │  Per stream:                               │  │
│  │    loop:                                   │  │
│  │      recv FrontendToClient                │  │
│  │      → identify type (single/batch/getID) │  │
│  │      → dispatch to handler goroutine      │  │
│  │      → send ClientToFrontend response     │  │
│  └────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────┘
```

- **DNS discovery** resolves Frontend addresses every 10s.
- **Parallelism** (default: 10) controls how many gRPC streams each Querier opens to
  each Frontend pod. gRPC multiplexes these over a single TCP connection.
- Each stream is a blocking pull loop — `Recv()` blocks until the Frontend has a batch.
- Batch processing: all jobs in a batch execute in parallel via goroutines, results are
  collected, and a single `ClientToFrontend` response is sent back.

Source: `modules/querier/worker/worker.go`, `modules/querier/worker/frontend_processor.go`

### 3.2 Job Execution

Each job arrives as an `httpgrpc.HTTPRequest`. The request URL determines the handler.
Jobs are routed based on `mode` and path parameters:

#### Trace by ID

```
mode=ingesters → query Live Store via partition ring (legacy mode name)
mode=blocks    → query Object Store (specific block range)
mode=external  → query external backend (if configured)
mode=all       → query all sources, combine results
```

The Querier uses a `trace.Combiner` to merge partial traces from multiple sources,
respecting `MaxBytesPerTrace` limits.

Source: `modules/querier/querier.go` (`FindTraceByID`)

#### Search (TraceQL)

Two paths depending on the URL:
- **Recent search**: Fans out to Live Store instances via the partition ring.
  Results are combined, deduplicated by trace ID, sorted by time, and capped at the limit.
- **Block search**: Targets a specific block with `blockID`, `startPage`, `pagesToSearch`.
  Uses the `traceql.Engine` to evaluate the query against a `SpansetFetcher`.

Source: `modules/querier/querier.go` (`SearchRecent`, `SearchBlock`)

#### Metrics Query Range

- **Recent**: Fans out to Live Store's `MetricsClient.QueryRange`, combines time series.
- **Block**: Compiles the TraceQL expression into a `MetricsEvaluator`, fetches spans
  from the block, and produces time series.

Source: `modules/querier/querier_query_range.go`

### 3.3 Live Store Fan-Out

When a job targets recent data, the Querier fans out to Live Store instances:

```
Querier
  │
  ├── partition ring lookup
  │     → GetReplicationSetsForOperation(Read)
  │     → returns N Live Store addresses
  │
  ├── for each Live Store (parallel):
  │     gRPC call: client.SearchRecent(req)
  │     gRPC call: client.FindTraceByID(req)
  │
  └── combine results
```

The `liveStorePool` manages a connection pool to Live Store instances, discovered
through the ring.

Source: `modules/querier/querier.go` (`forLiveStoreRing`)

---

## 4. The Live Store

The Live Store holds recent trace data that has not yet been flushed to the backend
object store. It consumes from Kafka, writes to WAL, and serves queries against
in-memory and on-disk data.

### 4.1 Data Lifecycle

```
  Kafka partition
       │
       ▼
  PartitionReader
       │ consume()
       ▼
  ┌─────────────────────────────────────┐
  │           Live Store Instance        │
  │           (per tenant)               │
  │                                     │
  │  ┌───────────┐   cut idle   ┌────────────┐   cut blocks   ┌────────────────┐
  │  │ liveTraces │──────────▶│  headBlock  │──────────────▶│   walBlocks     │
  │  │ (in-memory)│   traces   │  (in-memory)│               │  (on-disk WAL)  │
  │  └───────────┘            └────────────┘               └───────┬────────┘
  │                                                                │
  │                                                         complete block
  │                                                                │
  │                                                    ┌───────────▼────────┐
  │                                                    │   completeBlocks    │
  │                                                    │  (local columnar)   │
  │                                                    └───────────┬────────┘
  │                                                                │
  └────────────────────────────────────────────────────────────────┘
                                                                   │
                                                            flush to backend
                                                                   │
                                                                   ▼
                                                           Object Store
                                                          (S3/GCS/Azure)
```

- **liveTraces**: In-memory map of active traces (keyed by trace ID hash). Receives
  pushed bytes from Kafka.
- **headBlock**: In-memory block where idle traces are cut to.
- **walBlocks**: On-disk WAL blocks after the head block is cut.
- **completeBlocks**: Fully compacted local blocks (columnar format) ready for querying
  and eventually flushed to the backend.

### 4.2 Query Execution in Live Store

When a query arrives at the Live Store, it iterates over all block types with bounded
concurrency:

```go
func (i *instance) iterateBlocks(ctx, reqStart, reqEnd, fn) error {
    // 1. Search headBlock (synchronous)
    fn(headBlock)

    // 2. Search walBlocks (concurrent, bounded by QueryBlockConcurrency)
    for _, b := range walBlocks {
        go fn(b)
    }

    // 3. Search completeBlocks (concurrent, bounded)
    for _, b := range completeBlocks {
        go fn(b)
    }
}
```

For **TraceQL searches**, each block is queried using a `traceql.Engine` with a
`SpansetFetcher` that reads columnar data.

For **Trace by ID**, the live traces map is checked first (under lock), then all blocks
are searched.

For **Metrics Query Range**, WAL blocks use a raw evaluator while complete blocks use
a cacheable job-level evaluator, allowing result reuse across overlapping time ranges.

Source: `modules/livestore/instance_search.go`

### 4.3 Lag Awareness

The Live Store tracks its Kafka consumption lag. If the query's time range extends into
a period where the Live Store hasn't caught up yet:
- It increments `tempo_live_store_lagged_requests_total`
- If `fail_on_high_lag` is enabled, it returns an error instead of incomplete results

Source: `modules/livestore/live_store.go` (`isLagged`, `calculateTimeLag`)

---

## 5. Backend Storage (Object Store)

For backend block queries, the Querier reads directly from the object store through
the `storage.Store` interface:

- **`store.Find()`**: Finds a trace by ID across blocks in a block-start/block-end range
- **`store.Fetch()`**: Fetches spans matching a TraceQL query from a specific block
- **`store.Search()`**: Full-text search on a specific block
- **`store.SearchTags/TagValues()`**: Metadata queries on a specific block

Each backend job specifies exact block coordinates: `blockID`, `startPage`,
`pagesToSearch`, `version`, `size`, `footerSize`, and `dedicatedColumns`. These are
computed by the Frontend sharder from block metadata.

The Querier constructs a `BlockMeta` from these parameters and passes it to the store,
which handles:
- Reading parquet/columnar data from the object store
- Page-level filtering based on `startPage` and `pagesToSearch`
- Column pruning based on the query's required attributes
- Bloom filter checks for trace ID lookups

---

## 6. End-to-End Flow: TraceQL Search Example

Here is a complete example of a TraceQL search query flowing through the system:

```
1. Client sends: GET /api/search?q={duration>1s}&start=1h_ago&end=now

2. Query Frontend receives the request
   │
   ├─ Pipeline middleware:
   │   ├─ Strip disallowed headers
   │   ├─ Adjust time range (query_backend_after=15m)
   │   ├─ Assign weight (TraceQL, 1 condition → weight=1)
   │   ├─ Validate tenant
   │   └─ Shard into jobs:
   │       ├─ 3 Live Store jobs (last 15 minutes, split into 3 ranges)
   │       └─ 847 backend jobs (blocks from 1h ago to 15m ago)
   │
   ├─ Jobs enqueued into tenant's channel
   │
   ├─ Round-robin tenant selection
   │
   └─ Batches dispatched to Queriers (max_batch_size=7)
       │
       ├─ Batch [job1(w=1), job2(w=1), job3(w=1), job4(w=1), job5(w=1), job6(w=1), job7(w=1)]
       ├─ Batch [job8(w=1), job9(w=1), ...]
       └─ ...

3. Querier receives batch via gRPC stream
   │
   ├─ For each job in batch (parallel):
   │   ├─ Live Store job → fan out to Live Store instances
   │   │   └─ Live Store: iterate headBlock + walBlocks + completeBlocks
   │   │
   │   └─ Backend job → store.Fetch(blockMeta, traceqlRequest, opts)
   │       └─ Read parquet pages from S3/GCS
   │
   └─ Return batch of HTTP responses

4. Query Frontend receives responses
   │
   ├─ Cache layer stores cacheable results
   │
   ├─ Combiner merges results:
   │   ├─ Deduplicate by trace ID
   │   ├─ Sort by start time
   │   ├─ Enforce limit (default: 20)
   │   └─ Track shard progress for early termination
   │
   └─ Stream diffs to client every 500ms (gRPC) or return final (HTTP)
```

---

## 7. Configuration Reference

Key configuration knobs that control read path behavior:

| Config | Default | Location | Description |
|--------|---------|----------|-------------|
| `max_batch_size` | 7 | `query-frontend` | Max jobs per batch AND max total weight |
| `max_outstanding_per_tenant` | 2000 | `query-frontend` | Per-tenant queue depth |
| `max_retries` | 2 | `query-frontend` | Retries per failed job |
| `concurrent_jobs` | 1000 | `search sharder` | Max concurrent backend jobs in pipeline |
| `target_bytes_per_job` | 100MB | `search sharder` | Target block data per job |
| `query_backend_after` | 15m | `search sharder` | Recency threshold: data newer than this is queried from Live Store only |
| `ingester_shards` | 3 | `search sharder` | Number of Live Store time sub-ranges (legacy name) |
| `most_recent_shards` | 200 | `search sharder` | Max backend shards for progress tracking |
| `query_shards` | 50 | `trace_by_id` | Number of block boundary shards for trace ID |
| `parallelism` | 10 | `querier.worker` | gRPC streams per Querier per Frontend |
| `response_consumers` | 10 | `query-frontend` | Goroutines consuming responses from pipeline |
| `request_with_weights` | true | `weights` | Enable weight-based batching |
| `max_traceql_conditions` | 4 | `weights` | Condition count threshold for weight bump |
| `max_regex_conditions` | 1 | `weights` | Regex condition threshold for weight bump |

---

## 8. Key Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `tempo_query_frontend_queue_length` | Gauge | Jobs pending per tenant |
| `tempo_query_frontend_actual_batch_size` | Histogram | Jobs per dispatched batch |
| `tempo_query_frontend_batch_weight` | Histogram | Total weight per batch |
| `tempo_query_frontend_queue_duration_seconds` | Histogram | Time jobs spend queued |
| `tempo_query_frontend_connected_clients` | Gauge | Querier worker streams connected |
| `tempo_query_frontend_jobs_per_query` | Histogram | Jobs created per query (by type) |
| `tempo_query_frontend_retries` | Histogram | Retries per job |
| `tempo_query_frontend_discarded_requests_total` | Counter | Jobs dropped (queue full) |
| `tempo_querier_worker_request_executed_total` | Counter | Total jobs executed by queriers |
| `tempo_live_store_lagged_requests_total` | Counter | Queries with incomplete Live Store data |

---

## Appendix: Proto Messages

The Frontend↔Querier protocol uses two proto messages over a bidirectional gRPC stream:

```protobuf
// Frontend → Querier
message FrontendToClient {
  httpgrpc.HTTPRequest httpRequest = 1;    // single job (backwards compat)
  Type type = 2;                           // HTTP_REQUEST | GET_ID | HTTP_REQUEST_BATCH
  repeated httpgrpc.HTTPRequest httpRequestBatch = 4;  // batch of jobs
}

// Querier → Frontend
message ClientToFrontend {
  httpgrpc.HTTPResponse httpResponse = 1;  // single response (backwards compat)
  string clientID = 2;                     // querier identity
  repeated httpgrpc.HTTPResponse httpResponseBatch = 4; // batch responses
  int32 features = 5;                      // capability flags (e.g. REQUEST_BATCHING)
}
```

Source: `modules/frontend/v1/frontendv1pb/frontend.proto`
