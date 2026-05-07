# Tempo Write Path — Architecture Deep Dive

This document describes how trace data flows through Tempo's write path, from
receiver ingestion to durable block storage in the object store.

---

## High-Level Overview

```
  ┌───────────────────────────────────────────────────────────────┐
  │                     Instrumented Apps                         │
  │               (OTLP / Jaeger / Zipkin)                        │
  └────────────────────────────┬──────────────────────────────────┘
                               │
                               ▼
  ┌───────────────────────────────────────────────────────────────┐
  │                       Distributor                             │
  │                                                               │
  │  1. Receive traces (OTLP/Jaeger/Zipkin receivers)             │
  │  2. Validate, rate limit, truncate attributes                 │
  │  3. Group spans by trace ID                                   │
  │  4. Hash trace IDs → partition ring tokens                    │
  │  5. Write to Kafka (or push locally in single-binary)         │
  │  6. Forward to Metrics Generator (async)                      │
  │  7. Forward to external forwarders (if configured)            │
  └─────────────┬──────────────────────────┬──────────────────────┘
                │                          │
         Kafka records                Local push (single-binary)
                │                          │
       ┌────────┴─────────┐               │
       ▼                  ▼               ▼
  ┌──────────┐     ┌──────────────┐  ┌──────────────┐
  │  Live    │     │    Block     │  │   Metrics    │
  │  Store   │     │   Builder    │  │  Generator   │
  └────┬─────┘     └──────┬───────┘  └──────────────┘
       │                  │
       │   complete       │   build blocks
       │   blocks         │   from Kafka
       │                  │
       ▼                  ▼
  ┌───────────────────────────────────────┐
  │            Object Store               │
  │          (S3 / GCS / Azure)           │
  └───────────┬───────────────────────────┘
              │
              ▼
  ┌───────────────────────────────────────┐
  │    Backend Scheduler + Workers        │
  │       (compaction, retention)         │
  └───────────────────────────────────────┘
```

---

## 1. The Distributor

The Distributor is the entry point for all trace data. It receives spans, validates
them, and routes them to Kafka (or directly to the Live Store in single-binary mode).

### 1.1 Receivers

The Distributor hosts OpenTelemetry Collector receivers via a shim layer. Supported
protocols:

- **OTLP** (gRPC and HTTP)
- **Jaeger** (gRPC and Thrift HTTP)
- **Zipkin**

The receivers are configured using the same format as the OpenTelemetry Collector's
receiver configuration. All protocols converge into a single `ConsumeTraces` call
on the Distributor, which converts the data to internal proto format.

Source: `modules/distributor/receiver/shim.go`

### 1.2 Ingestion Pipeline (`PushTraces`)

When traces arrive, the Distributor processes them through a sequential pipeline:

```
ConsumeTraces (from receiver)
    │
    ▼
PushTraces
    │
    ├─ 1. Execute TracePushMiddlewares (fail-open hooks)
    │
    ├─ 2. Extract tenant ID, span count, proto size
    │
    ├─ 3. Rate limiting check
    │     └─ Local or Global strategy (based on config)
    │        Global: limit / healthy_distributors
    │
    ├─ 4. Marshal ptrace.Traces → tempopb.Trace (wire-compatible)
    │
    ├─ 5. Usage tracking (cost attribution, if enabled)
    │
    ├─ 6. requestsByTraceID:
    │     ├─ Group ResourceSpans by trace ID
    │     ├─ Validate trace IDs (must be 128-bit) and span IDs (64-bit, non-zero)
    │     ├─ Truncate oversized attributes (key/value > max_attribute_bytes)
    │     ├─ Compute ring tokens: hash(tenantID + traceID)
    │     └─ Produce rebatchedTrace structs with start/end timestamps
    │
    ├─ 7. Forward to external forwarders (if configured)
    │
    └─ 8. Route to storage:
          ├─ Kafka mode: sendToKafka()
          └─ Local mode:  pushLocal() → Live Store + Generator
```

Source: `modules/distributor/distributor.go` (`PushTraces`, `requestsByTraceID`)

### 1.3 Rate Limiting

The Distributor enforces per-tenant ingestion rate limits in bytes/second with two
strategies:

- **Local**: The configured limit applies directly to each Distributor instance.
- **Global**: The configured limit is divided by the number of healthy Distributors
  in the ring (`limit / instances_count`). Each instance enforces its share.

Burst is always applied per-instance regardless of strategy. If the batch size exceeds
both the per-second limit and the burst, a `ResourceExhausted` error is returned with
retry information (if `retry_after_on_resource_exhausted` is configured, default: 5s).

Source: `modules/distributor/distributor.go` (`checkForRateLimits`),
`modules/distributor/ingestion_rate_strategy.go`

### 1.4 Attribute Truncation

Before writing, the Distributor truncates attribute keys and string values that exceed
`max_attribute_bytes` (default: 2048 bytes). Truncation is applied at all scopes:
resource, scope, span, event, and link attributes. A rate-limited log captures the
first truncation example per batch for diagnostics.

Source: `modules/distributor/distributor.go` (`processAttributes`)

### 1.5 Trace Rebatching

The `requestsByTraceID` function reorganizes incoming ResourceSpans into per-trace
groups. This is necessary because a single push may contain spans from many traces
mixed across ResourceSpans, and each trace must be routed to a specific Kafka partition.

Each trace gets:
- A **ring token**: `hash(tenantID + traceID)` — used for partition selection
- A **rebatchedTrace** struct containing the grouped `tempopb.Trace`, start/end
  timestamps, and span count

Source: `modules/distributor/distributor.go` (`requestsByTraceID`)

---

## 2. Kafka Ingestion Path

In the standard deployment, the Distributor writes to Kafka. This decouples ingestion
from storage and enables independent scaling of consumers.

### 2.1 Writing to Kafka (`sendToKafka`)

```
rebatchedTraces (grouped by trace ID)
    │
    ├─ Marshal each trace to proto bytes
    │
    ├─ Partition ring lookup:
    │     partitionRing.ShuffleShard(tenantID, shardSize)
    │     → tenant-specific subset of partitions
    │
    ├─ ring.DoBatchWithOptions(Write, keys, ...)
    │     → routes each trace to its partition by ring token
    │
    ├─ For each partition batch:
    │     ├─ Build PushBytesRequest (trace bytes + IDs)
    │     ├─ ingest.Encode() → Kafka records
    │     │   (respects ProducerMaxRecordSizeBytes)
    │     └─ kafkaProducer.ProduceSync()
    │
    └─ Record metrics: records/request, write latency, bytes/partition
```

Key properties:
- **Partition assignment**: Uses the partition ring with shuffle sharding per tenant.
  Each trace's ring token determines which partition it lands on.
- **Record encoding**: `ingest.Encode` serializes `PushBytesRequest` into Kafka records,
  splitting large requests to respect `ProducerMaxRecordSizeBytes`.
- **Synchronous writes**: `ProduceSync` ensures durability before acknowledging.

Source: `modules/distributor/distributor.go` (`sendToKafka`)

### 2.2 Local Push (Single-Binary Mode)

When Kafka is not configured (`push_spans_to_kafka: false`), the Distributor pushes
directly to the in-process Live Store and Metrics Generator:

```go
func (d *Distributor) pushLocal(ctx, userID, keys, traces) error {
    d.pushTracesToLiveStore(ctx, userID, traces)  // PushBytesRequest
    d.pushTracesToGenerator(ctx, userID, keys, traces) // async, via queue
}
```

The Live Store receives a `PushBytesRequest` with pre-marshaled trace bytes.
The Generator receives `PushSpansRequest` with ResourceSpans batches, forwarded
asynchronously through a per-tenant queue (`generatorForwarder`).

Source: `modules/distributor/distributor.go` (`pushLocal`, `pushTracesToLiveStore`)

---

## 3. The Live Store

The Live Store consumes trace data from Kafka and holds it in memory/WAL for serving
recent queries. It is the primary consumer of the Kafka ingest path.

### 3.1 Kafka Consumption

```
Kafka partition
    │
    ▼
PartitionReader
    │ fetch records
    ▼
LiveStore.consume(records)
    │
    ├─ For each record:
    │     ├─ Decode PushBytesRequest
    │     ├─ Get or create per-tenant instance
    │     ├─ instance.pushBytes(timestamp, request)
    │     └─ Drop records older than CompleteBlockTimeout
    │
    └─ Return last consumed offset for commit
```

Each Live Store instance owns one Kafka partition (mapped via `IngesterPartitionID`).
It registers itself in both a **partition ring** (for partition ownership) and a
**membership ring** (for query-time service discovery by Queriers).

On startup, the Live Store:
1. Reloads blocks from the local WAL
2. Starts the partition lifecycler and membership ring
3. Begins consuming from Kafka
4. Waits until Kafka lag is within `readiness_target_lag` before marking ready

Source: `modules/livestore/live_store.go` (`consume`, `starting`)

### 3.2 Data Lifecycle within Live Store

```
pushBytes()
    │
    ▼
┌─────────────────────────────────────────────────┐
│                  instance                        │
│                  (per tenant)                    │
│                                                  │
│  liveTraces (in-memory map)                      │
│       │                                          │
│       │ cutIdleTraces (periodic)                 │
│       ▼                                          │
│  headBlock (in-memory, appendable)               │
│       │                                          │
│       │ cutBlocks (when head exceeds limits)     │
│       ▼                                          │
│  walBlocks[] (on-disk WAL, read-only)            │
│       │                                          │
│       │ globalCompleteLoop (encode to columnar)  │
│       ▼                                          │
│  completeBlocks[] (local columnar blocks)        │
│       │                                          │
│       │ Kafka mode: age-based deletion           │
│       │ Local mode: flush to object store        │
│       ▼                                          │
│  Object Store (S3/GCS/Azure)                     │
└──────────────────────────────────────────────────┘
```

**liveTraces** → **headBlock**: Periodic goroutine (`perTenantCutToWalLoop`) moves
idle traces from the in-memory map to the head block.

**headBlock** → **walBlocks**: When the head block exceeds configured limits, it's
cut to a WAL block on disk.

**walBlocks** → **completeBlocks**: Background workers (`globalCompleteLoop`) encode
WAL blocks into the columnar format, producing complete local blocks.

**completeBlocks** → **Object Store**: Depends on the mode:
- **Kafka mode** (`kafkaCompleteBlockLifecycle`): Complete blocks are deleted by age.
  The Block Builder is responsible for building durable blocks from Kafka.
- **Local mode** (`localCompleteBlockLifecycle`): Complete blocks are flushed to the
  object store via a background flush queue with retry logic.

Source: `modules/livestore/live_store.go`, `modules/livestore/live_store_background.go`,
`modules/livestore/complete_block_lifecycle.go`

---

## 4. The Block Builder

The Block Builder is a separate service that consumes from Kafka and builds durable
blocks in the object store. It runs independently of the Live Store and operates on
a cycle-based schedule.

### 4.1 Consume Cycle

```
BlockBuilder.consume()
    │
    ├─ Get assigned partitions (from partition ring config)
    │
    ├─ Fetch partition offsets (committed + end)
    │
    ├─ For each partition (ordered by lag, laggiest first):
    │     │
    │     ├─ consumePartition():
    │     │     ├─ Set consumer offset to last commit
    │     │     ├─ Poll records until:
    │     │     │     - Time window (cycleDuration) elapsed
    │     │     │     - Max bytes per cycle reached
    │     │     │     - No more data
    │     │     │
    │     │     ├─ For each record:
    │     │     │     ├─ Decode PushBytesRequest
    │     │     │     └─ writer.pushBytes(tenant, traces)
    │     │     │           └─ tenantStore.AppendTrace(traceID, bytes, ts)
    │     │     │
    │     │     ├─ writer.flush() → create blocks per tenant
    │     │     ├─ Commit offset to Kafka
    │     │     └─ Allow compaction on written blocks
    │     │
    │     └─ Repeat until lag < cycleDuration for all partitions
    │
    └─ Wait (cycleDuration - lag) before next cycle
```

Source: `modules/blockbuilder/blockbuilder.go` (`consume`, `consumePartition`)

### 4.2 Block Creation

Within each consume cycle, traces are accumulated per tenant in a `tenantStore`:

1. **Append**: Traces are pushed into `liveTraces` (a trace-ID-keyed accumulator),
   respecting `MaxBytesPerTrace` limits. Excess spans are discarded with
   `ReasonTraceTooLarge`.

2. **Flush**: All accumulated traces are written as a columnar block:
   - A **deterministic block ID** is generated from `(tenantID, partitionID, startOffset)`
     to ensure idempotent replays.
   - If a block with that ID already exists, it's marked as compacted first.
   - The block is created with `CreateWithNoCompactFlag = true` to prevent compaction
     before the cycle completes.
   - Block time range is adjusted for ingestion slack.

3. **Write**: The block is written to the object store via `tempodb.Writer.WriteBlock()`.

4. **Allow compaction**: After the offset is committed, the no-compact flag is removed,
   allowing the compactor to process the block.

Source: `modules/blockbuilder/tenant_store.go` (`AppendTrace`, `Flush`),
`modules/blockbuilder/partition_writer.go`

---

## 5. The Metrics Generator

The Metrics Generator consumes trace data to produce span-derived metrics (e.g.,
service graphs, span metrics). It runs as a separate service.

### 5.1 Ingestion

The Generator receives spans through two paths:

- **Kafka mode**: Consumes directly from Kafka partitions (same topic as Live Store
  and Block Builder). Assigned partitions are determined by the partition ring.
- **Local mode**: Receives `PushSpansRequest` from the Distributor via the
  `generatorForwarder` queue.

The `generatorForwarder` in the Distributor is an async per-tenant queue that buffers
trace pushes. Each tenant gets a configurable queue size (default: 100) and worker
count (default: 2). Queue configuration is watched and updated dynamically from
overrides.

The `SkipMetricsGeneration` flag on `PushBytesRequest` / context allows upstream
systems to signal that metrics have already been generated for these spans.

Source: `modules/generator/generator.go`, `modules/distributor/forwarder.go`

---

## 6. Backend Scheduler and Workers

The Backend Scheduler and Backend Workers handle background maintenance of the object
store.

### 6.1 Backend Scheduler

The Backend Scheduler manages scheduling and execution of backend jobs including:
- **Compaction**: Merging small blocks into larger ones
- **Retention**: Deleting blocks past their retention period
- **Tenant index updates**: Maintaining the per-tenant block index

It reads block metadata from the object store and creates work items dispatched to
Backend Workers.

Source: `modules/backendscheduler/backendscheduler.go`

### 6.2 Backend Workers

Backend Workers pull jobs from the Backend Scheduler and execute them. They:
- Register in a ring for sharding tenant index writes
- Connect to the Backend Scheduler via gRPC
- Execute compaction, retention, and index-writing jobs against the object store

Source: `modules/backendworker/backendworker.go`

---

## 7. External Forwarders

The Distributor supports forwarding traces to external systems via the `forwarders`
configuration. The `forwarder.Manager` manages per-tenant forwarder instances.
Forwarding happens before the Kafka/local write and errors are logged but don't fail
the push (fail-open).

Source: `modules/distributor/forwarder/`, `modules/distributor/distributor.go`

---

## 8. End-to-End Flow: Trace Ingestion (Kafka Mode)

```
1. App sends spans via OTLP gRPC

2. Distributor receives via receiver shim
   │
   ├─ Extract tenant from X-Scope-OrgID header
   ├─ Rate limit check: 15KB batch vs 10MB/s limit → pass
   ├─ Truncate attribute "http.url" value (3KB → 2KB)
   ├─ Group 50 spans into 12 traces by trace ID
   ├─ Compute ring tokens for each trace
   ├─ Forward to external forwarders (if any)
   │
   ├─ Kafka write:
   │     ├─ Partition ring: tenant shard → partitions [3, 7, 12]
   │     ├─ Route traces by ring token → 3 partition batches
   │     ├─ Encode → 3 Kafka records
   │     └─ ProduceSync → Kafka acks
   │
   └─ Async: queue spans for Metrics Generator

3. Live Store (partition 7) consumes record
   │
   ├─ Decode PushBytesRequest
   ├─ Get/create tenant instance
   ├─ Push to liveTraces (in-memory)
   │
   ├─ [periodic] Cut idle traces → headBlock
   ├─ [periodic] Cut headBlock → walBlock (on-disk)
   ├─ [periodic] Complete walBlock → completeBlock (columnar)
   └─ [age-based] Delete old completeBlocks

4. Block Builder (partition 7) consumes same records
   │
   ├─ Accumulate traces per tenant in liveTraces
   ├─ After cycle duration (or max bytes):
   │     ├─ Create columnar block with deterministic ID
   │     ├─ Write block to S3 (with no-compact flag)
   │     ├─ Commit Kafka offset
   │     └─ Remove no-compact flag
   │
   └─ Block is now durable and queryable by Queriers

5. Backend Workers (ongoing)
   │
   ├─ Compact small blocks into larger ones
   ├─ Delete blocks past retention
   └─ Update tenant block index
```

---

## 9. Configuration Reference

| Config | Default | Location | Description |
|--------|---------|----------|-------------|
| `max_attribute_bytes` | 2048 | `distributor` | Truncation limit for attribute keys/values |
| `retry_after_on_resource_exhausted` | 5s | `distributor` | Retry-After hint for rate-limited clients |
| `push_spans_to_kafka` | (wired internally) | `distributor` | Route to Kafka vs local Live Store |
| `receivers` | OTLP gRPC + Jaeger | `distributor` | OpenTelemetry Collector receiver configs |
| `consume_cycle_duration` | (configurable) | `block-builder` | Duration of each consume-and-flush cycle |
| `max_bytes_per_cycle` | (configurable) | `block-builder` | Max bytes consumed per partition per cycle |
| `complete_block_timeout` | (configurable) | `live-store` | Max age of records to accept from Kafka |
| `readiness_target_lag` | (configurable) | `live-store` | Max Kafka lag before marking ready |
| `complete_block_concurrency` | (configurable) | `live-store` | Parallel block completion workers |
| `ingestion_rate_limit` | (per-tenant override) | `overrides` | Bytes/second rate limit |
| `ingestion_burst_size` | (per-tenant override) | `overrides` | Burst allowance in bytes |
| `ingestion_rate_strategy` | local or global | `overrides` | Rate limiting strategy |
| `max_bytes_per_trace` | (per-tenant override) | `overrides` | Max trace size before dropping spans |

---

## 10. Key Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `tempo_distributor_spans_received_total` | Counter | Spans received per tenant |
| `tempo_distributor_bytes_received_total` | Counter | Proto bytes received per tenant (post-limits) |
| `tempo_distributor_ingress_bytes_total` | Counter | Raw bytes received per tenant (pre-limits) |
| `tempo_distributor_traces_per_batch` | Histogram | Traces per push batch |
| `tempo_distributor_attributes_truncated_total` | Counter | Truncated attributes per tenant/scope |
| `tempo_distributor_kafka_records_per_request` | Histogram | Kafka records produced per push |
| `tempo_distributor_kafka_write_latency_seconds` | Histogram | Kafka produce latency |
| `tempo_distributor_kafka_write_bytes_total` | Counter | Bytes written to Kafka per partition |
| `tempo_live_store_records_processed_total` | Counter | Kafka records consumed per tenant |
| `tempo_live_store_records_dropped_total` | Counter | Dropped records per tenant/reason |
| `tempo_live_store_blocks_completed_total` | Counter | WAL → complete block transitions |
| `tempo_live_store_completion_duration_seconds` | Histogram | Block completion time |
| `tempo_block_builder_flushed_blocks` | Counter | Blocks written to object store per tenant |
| `tempo_block_builder_consume_cycle_duration_seconds` | Histogram | Full consume cycle time |
| `tempo_block_builder_fetch_duration_seconds` | Histogram | Kafka fetch latency per partition |
| `tempo_block_builder_spans_deduped_total` | Counter | Duplicate spans removed during block building |
