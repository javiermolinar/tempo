# Tempo v3.0.0-rc.1 Release Notes

Tempo 3.0 is a major release candidate focused on the new ingest/write architecture, removal of deprecated 2.x components, migration tooling, TraceQL metrics improvements, and live-store/block-builder correctness and observability fixes.

## Breaking Changes

* Remove duplicate "compaction" prefix from CompactorConfig CLI flags. Affected flags: `compaction.block-retention`, `compaction.max-objects-per-block`, `compaction.max-block-bytes`, `compaction.compaction-window` by @electron0zero in https://github.com/grafana/tempo/pull/6909
* Enable RetryInfo by default. `distributor.retry_after_on_resource_exhausted` now defaults to `5s` (was `0`) so OTLP clients receive a retry hint on `ResourceExhausted` errors by @electron0zero in https://github.com/grafana/tempo/pull/7088
  Set to `0` to disable cluster-wide, or set the per-tenant override `ingestion.retry_info_enabled: false` to disable for a single tenant.
* Centralize block and WAL config: `block_builder` and `live_store` now always use `storage.trace.block` settings; per-module block config fields are removed by @stoewer in https://github.com/grafana/tempo/pull/6647
* Remove Opencensus receiver by @javiermolinar in https://github.com/grafana/tempo/pull/6523
* Remove legacy `mem-ballast-size-mbs` cli flag by @orkhan-huseyn in https://github.com/grafana/tempo/pull/6403
* tempo-cli: Support relative time (now, now-1h) for start/end args and standardize on RFC3339 in all commands by @electron0zero in https://github.com/grafana/tempo/pull/6458
  `query search` command no longer accepts timestamps without timezone (e.g. `2024-01-01T00:00:00`), use RFC3339 (e.g. `2024-01-01T00:00:00Z`) or relative time instead.
* Consolidate read configuration for recent data cutoff. `query_frontend.search.query_ingesters_until` is removed in favor of only `query_frontend.search.query_backend_after` by @mapno in https://github.com/grafana/tempo/pull/6507
* Remove deprecated `querier.query_live_store` config. This field must be removed from configs on upgrade by @javiermolinar in https://github.com/grafana/tempo/pull/7048
* Optimize TraceQL AST by rewriting conditions on the same attribute to their array equivalent by @stoewer in https://github.com/grafana/tempo/pull/6353
  Slightly changes the array matching semantics of != and !~ operators and introduces stricter rules for regex literals.
* Remove partition ring livestore config by @javiermolinar in https://github.com/grafana/tempo/pull/6981
* Remove ingester module by @javiermolinar in https://github.com/grafana/tempo/pull/6959
* Remove ingest.enabled config by @javiermolinar in https://github.com/grafana/tempo/pull/6873
* Disable legacy (flat, unscoped) overrides by default. Tempo will refuse to start if legacy overrides are detected. Set `enable_legacy_overrides: true` or `-config.enable-legacy-overrides=true` to opt back in temporarily. Legacy overrides will be removed in a future release by @electron0zero in https://github.com/grafana/tempo/pull/6741
* Remove remaining app ingester config by @javiermolinar in https://github.com/grafana/tempo/pull/6667
* Remove span-metrics leftovers and lazy-init generator clients by @javiermolinar in https://github.com/grafana/tempo/pull/6618
* Decommission livestore MetricsGenerator query service by @javiermolinar in https://github.com/grafana/tempo/pull/6615
* Remove metrics-generator localblocks processor and related local block storage plumbing by @javiermolinar in https://github.com/grafana/tempo/pull/6555
* Remove ingesters by @javiermolinar in https://github.com/grafana/tempo/pull/6504
* Remove ingesters and compactor alerts by @javiermolinar in https://github.com/grafana/tempo/pull/6369
* Removed `v2` block encoding and compactor component by @joe-elliott in https://github.com/grafana/tempo/pull/6273
  This includes the removal of the following CLI commands which were `v2` specific: `list block`, `list index`, `view index`, `gen index`, `gen bloom`.
* SpanMetricsSummary is removed and querier code simplified by @javiermolinar in https://github.com/grafana/tempo/pull/6496 and https://github.com/grafana/tempo/pull/6510
* Sets the `all` target to be 3.0 compatible and removes the `scalable-single-binary` target by @joe-elliott in https://github.com/grafana/tempo/pull/6283
* Clean up enterprise jsonnet by @javiermolinar in https://github.com/grafana/tempo/pull/6505

## Changes

* Stop publishing 32-bit ARM binary archives. Release artifacts continue to include amd64 and arm64 binaries by @javiermolinar in https://github.com/grafana/tempo/pull/7106
* Upgrade Tempo to Go 1.26.0 by @stoewer in https://github.com/grafana/tempo/pull/6443
* Allow duplicate dimensions for span metrics and service graphs. This is a valid use case if using different instrumentation libraries, with spans having "deployment.environment" and others "deployment_environment", for example by @carles-grafana in https://github.com/grafana/tempo/pull/6288
* Update default max duration for TraceQL metrics queries up to one day by @javiermolinar in https://github.com/grafana/tempo/pull/6285
* Set TraceQL query metrics checks by default in Vulture by @javiermolinar in https://github.com/grafana/tempo/pull/6275
* Make Tempo single-binary example use the local backend by @javiermolinar in https://github.com/grafana/tempo/pull/7033
* Bump ingestion limits by @javiermolinar in https://github.com/grafana/tempo/pull/7034
* TraceQL metrics - change default step intervals to align with new vParquet5 timestamp columns by @mdisibio in https://github.com/grafana/tempo/pull/6413
* Remove all traces of ingesters from the dashboards by @javiermolinar in https://github.com/grafana/tempo/pull/6352
* jsonnet: Add emptyDir data volume to block-builder StatefulSet by @mapno in https://github.com/grafana/tempo/pull/6648
* Add quick checks to tempo mixin runbook by @javiermolinar in https://github.com/grafana/tempo/pull/6696
* Deprecate metrics-generator no-local-blocks by @javiermolinar in https://github.com/grafana/tempo/pull/6707
* Own local block and partition ring helpers by @javiermolinar in https://github.com/grafana/tempo/pull/6808
* Track invalid trace and span id discards by @javiermolinar in https://github.com/grafana/tempo/pull/6799
* Deprecate `query_frontend.rf1_after` and query all blocks regardless of replication factor for non-metrics paths. Simplifies 2.x to 3.0 migration by @mapno in https://github.com/grafana/tempo/pull/6969
* Flush blocks to backend storage from the Live store in single binary mode by @javiermolinar in https://github.com/grafana/tempo/pull/6941
* Remove stale config from the examples by @javiermolinar in https://github.com/grafana/tempo/pull/6980
* tempo-cli: Rewrite `migrate overrides-config` and add `migrate overrides-per-tenant` command to help migrate legacy flat overrides to the new scoped format by @electron0zero in https://github.com/grafana/tempo/pull/6793
* Decouple livestore from metrics-generator by @javiermolinar in https://github.com/grafana/tempo/pull/6506 and https://github.com/grafana/tempo/pull/6535
* Expose otlp http and grpc ports for Docker examples by @javiermolinar in https://github.com/grafana/tempo/pull/6296

## Features

* Add span profiling support via otelpyroscope. Enable with `span_profiling: true` (or `-span-profiling` CLI flag) to attach pprof labels to OTel spans by @simonswine in https://github.com/grafana/tempo/pull/7063
* Add `tempo-cli migrate config` command for migrating Tempo 2.x configs to 3.0 by @mapno in https://github.com/grafana/tempo/pull/6982
* jsonnet: Add KEDA-based horizontal pod autoscaling support for microservices deployment by @mapno in https://github.com/grafana/tempo/pull/6970
* Add automemlimit support for automatic GOMEMLIMIT configuration. Enable with `memory.automemlimit_enabled: true` by @oleg-kozlyuk-grafana in https://github.com/grafana/tempo/pull/6313
* Support comparison operators in TraceQL Metrics queries by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/6474
* metrics-generator: Add span filtering to service graphs through `filter_policies` by @javiermolinar in https://github.com/grafana/tempo/pull/6453
* Add new include_any filter policy for spanmetrics filter by @javiermolinar in https://github.com/grafana/tempo/pull/6392
* Add span_multiplier_key to overrides. This allows tenants to specify the attribute key used for span multiplier values to compensate for head-based sampling by @carles-grafana in https://github.com/grafana/tempo/pull/6260
* metrics-generator: Add per-label limiter to control cardinality by @electron0zero in https://github.com/grafana/tempo/pull/6414
  Adds `max_cardinality_per_label` per tenant override and new metrics to estimate per label cardinality demand estimate.
* Add an extension mechanism for per-tenant overrides by @stoewer in https://github.com/grafana/tempo/pull/6758
* Extend `TraceRedactor` interface to support hiding complete traces via `ErrTraceHidden` by @stoewer in https://github.com/grafana/tempo/pull/6811
* Single-binary mode: push distributor local ingest directly to live-store and metrics-generator without Kafka by @javiermolinar in https://github.com/grafana/tempo/pull/6729

## Enhancements

* Support OR conditions for tag name and tag value autocomplete (search tags v2) by @ie-pham in https://github.com/grafana/tempo/pull/6827
* Expose MinIO retry settings via S3 config by @rwhitty in https://github.com/grafana/tempo/pull/6561
* Reduce default livestore WAL size and align query defaults: `max_block_duration` `1m` to `30s`, `max_block_bytes` `100MiB` to `50MiB`, `complete_block_timeout` `1h` to `20m`, metrics `query_backend_after` `30m` to `15m` by @zhxiaogg in https://github.com/grafana/tempo/pull/6974
* Enable native histogram emission for all promauto-registered histograms, including `tempo_request_duration_seconds`. Both classic and native formats are emitted simultaneously; existing scrapers are unaffected by @zalegrala in https://github.com/grafana/tempo/pull/6910
* tempo-cli: Add `--header` flag to `query api` commands for custom headers by @Nouuu in https://github.com/grafana/tempo/pull/6768
* tempo-cli: add `redact` command to submit trace redaction jobs to the backend scheduler by @zalegrala in https://github.com/grafana/tempo/pull/6832
* Block builder: deduplicate spans within traces during block creation and track removed duplicates via `tempo_block_builder_spans_deduped_total` metric by @zhxiaogg in https://github.com/grafana/tempo/pull/6539
* metrics-generator: Support extracting span multiplier from W3C tracestate OTel probability sampling threshold via `enable_tracestate_span_multiplier` config option by @csmarchbanks in https://github.com/grafana/tempo/pull/6684
* Add new alerts and runbooks entries by @javiermolinar in https://github.com/grafana/tempo/pull/6276
* Double the maximum number of dedicated string columns in vParquet5 and update tempo-cli to determine the optimum number for the data by @mdisibio in https://github.com/grafana/tempo/pull/6282
* TraceQL metrics - experimental faster read path for most metrics queries, accessible behind the query hint `spanonly_fetch=true` when `unsafe_query_hints` is enabled by @mdisibio in https://github.com/grafana/tempo/pull/6359
* TraceQL metrics - add new per-tenant override to opt-in or opt-out of the new experimental faster read path for most metrics queries by @mdisibio in https://github.com/grafana/tempo/pull/6849
* Vulture: extend data consistency checks to include more strings, integers, and blobs, at resource/span/event scopes, and perform deeper trace content check by @mdisibio in https://github.com/grafana/tempo/pull/6731
* Improve attribute truncating observability by @javiermolinar in https://github.com/grafana/tempo/pull/6400
* Log truncated oversized attributes by @carles-grafana in https://github.com/grafana/tempo/pull/6467
* livestore: make `trace_too_large` log line an insight by @carles-grafana in https://github.com/grafana/tempo/pull/6371
* Remove live-store partition owner from ring on shutdown to prevent stale owner entries by @oleg-kozlyuk-grafana in https://github.com/grafana/tempo/pull/6409
* Improved live store readiness check and added `readiness_target_lag` and `readiness_max_wait` config parameters. Live store will now - if `readiness_target_lag` is set - not report `/ready` until Kafka lag is brought under the specified value by @oleg-kozlyuk-grafana and @ruslan-mikhailov in https://github.com/grafana/tempo/pull/6238 and https://github.com/grafana/tempo/pull/6405
* Expose a new histogram metric to track the jobs per query distribution by @javiermolinar in https://github.com/grafana/tempo/pull/6343
* Do deep validation for filter policies in user configurable overrides API by @electron0zero in https://github.com/grafana/tempo/pull/6407
* Allow span_name_sanitization to be set via user-configurable overrides API by @Logiraptor in https://github.com/grafana/tempo/pull/6411
* Add `fail_on_high_lag` parameter to allow live-store to fail if it is lagged by @ruslan-mikhailov and @carles-grafana in https://github.com/grafana/tempo/pull/6363, https://github.com/grafana/tempo/pull/6567 and https://github.com/grafana/tempo/pull/7066
* Add support for per-tenant left-padding of trace IDs by @mapno in https://github.com/grafana/tempo/pull/6489
* Add new metric for generator ring size: `tempo_distributor_metrics_generator_tenant_ring_size` by @zalegrala in https://github.com/grafana/tempo/pull/5686
* Remove explicit `runtime.GC()` calls in vParquet5 compactor/block creation and CLI by @oleg-kozlyuk-grafana in https://github.com/grafana/tempo/pull/6603
* Reduce allocations in `extendReuseSlice` growth path during WAL writes and block creation by @mapno in https://github.com/grafana/tempo/pull/6863
* Implemented anti-affinity for pods in same livestore zone by @zhxiaogg in https://github.com/grafana/tempo/pull/6757
* Livestore: skipped WAL complete op during shutdown by @zhxiaogg in https://github.com/grafana/tempo/pull/6839
* Add metric to track livestore block cut reasons by @zhxiaogg in https://github.com/grafana/tempo/pull/6922
* Enable async parquet read mode for WAL completion path by @zhxiaogg in https://github.com/grafana/tempo/pull/6967
* metrics-generator: add `leave_consumer_group_on_shutdown` to send LeaveGroup on shutdown for immediate partition reassignment instead of waiting for session timeout by @zalegrala in https://github.com/grafana/tempo/pull/6575

## Bugfixes

* Fix tempo-vulture ignoring `-tempo-push-tls` flag in normal operating mode by @zachfi in https://github.com/grafana/tempo/pull/6976
* livestore: check readiness before lag for SearchRecent and QueryRange queries by @zhxiaogg in https://github.com/grafana/tempo/pull/6911
* Fix integer overflow in query parameters by using `strconv.ParseUint` instead of `strconv.Atoi`/`strconv.ParseInt` for unsigned integer fields by @ricardbejarano in https://github.com/grafana/tempo/pull/6612
* Fix live-store SearchTagValuesV2 disk cache never being populated on complete blocks by @mapno in https://github.com/grafana/tempo/pull/6858
* Fix dedicated columns fallback in `block_builder` and `live_store` to use `storage.trace.block.parquet_dedicated_columns` when not set via overrides by @stoewer in https://github.com/grafana/tempo/pull/6647
* Force live-store to rehydrate from Kafka lookback period when local data is missing (e.g. PVC wipe, new node) instead of resuming from the committed consumer group offset by @oleg-kozlyuk-grafana in https://github.com/grafana/tempo/pull/6428
* fix: reload span_name_sanitization overrides during runtime by @electron0zero in https://github.com/grafana/tempo/pull/6435
* fix: live store honor the config options for block and WAL versions by @mdisibio in https://github.com/grafana/tempo/pull/6509
* fix: block builder honor the global storage block config for block and WAL versions by @Harry-kp in https://github.com/grafana/tempo/pull/6532
* fix: normalize allowlist headers when building the allowlist map by @javiermolinar in https://github.com/grafana/tempo/pull/6481
* fix: bug related to dedicated column filtering by @stoewer in https://github.com/grafana/tempo/pull/6586
* fix: compactor deduped spans metric uses wrong type (gauge instead of counter) by @bejaratommy in https://github.com/grafana/tempo/pull/6576
* metrics-generator: Fix active-series counter underflow in local series limiter when overflow series are deleted by @carles-grafana in https://github.com/grafana/tempo/pull/6568
* fix: skip per-label limiter and sanitizer for target_info and host_info metrics in metrics-generator by @electron0zero in https://github.com/grafana/tempo/pull/6660
* fix(traceql): err on division by zero by @Proximyst in https://github.com/grafana/tempo/pull/6580
* fix(traceql): stop intPow from hanging by @Proximyst in https://github.com/grafana/tempo/pull/6581
* fix(traceql): Fix incorrect search results for some queries on new blob columns by @mdisibio in https://github.com/grafana/tempo/pull/6815
* fix(vparquet5) Fix buffer-reuse bug where event attributes in dedicated columns could be persisted on additional spans and events by @mdisibio in https://github.com/grafana/tempo/pull/6914
* fix: race condition where `remove_owner_on_shutdown` flag was set too late — after context cancellation already triggered the lifecycler's shutdown, causing the partition owner to remain in the ring by @oleg-kozlyuk-grafana in https://github.com/grafana/tempo/pull/6693
* Return 400 instead of 500 when query_range or query_instant requests have unparseable start/end parameters by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/6694
* fix: correct block-builder fetch metrics to use counters instead of gauges by @WinterCabbage in https://github.com/grafana/tempo/pull/6578
* Log tenant on receiver push errors by @javiermolinar in https://github.com/grafana/tempo/pull/6780
* Fix race conditions in WAL block by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/6773
* metrics-generator: Fix `target_info` being skipped when resource attributes have empty values by @carles-grafana in https://github.com/grafana/tempo/pull/6774
* metrics-generator: Drain old series on metric replacement to prevent limiter leak and permanent overflow by @carles-grafana in https://github.com/grafana/tempo/pull/6653
* live-store: fixed unsuccessful deregistering from membership/partition rings during shutdown by @zhxiaogg in https://github.com/grafana/tempo/pull/6848
* fix: respect context cancellation when reading WAL block iterator by @zhxiaogg in https://github.com/grafana/tempo/pull/6928
* Complete lifecycler shutdown on errors by @javiermolinar in https://github.com/grafana/tempo/pull/6906
* livestore: fix concurrent WAL writes from periodic and shutdown flushes by @zhxiaogg in https://github.com/grafana/tempo/pull/6972
* live-store: fix race conditions for tag values endpoint by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/7000
* live-store: correct backoff duration calculation by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/6999
* vulture: fix for recent traces when query_end_cutoff is enabled by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/7018
* Fix live-store producing WAL blocks exceeding max_block_bytes when flushing large batches of idle traces by @ruslan-mikhailov in https://github.com/grafana/tempo/pull/6971
* live-store: skip lookback replay when partition is Inactive during scaling down by @zhxiaogg in https://github.com/grafana/tempo/pull/7101

## New Contributors

* @evan361425 made their first contribution in https://github.com/grafana/tempo/pull/5968
* @mihaelmiklec made their first contribution in https://github.com/grafana/tempo/pull/6442
* @Harry-kp made their first contribution in https://github.com/grafana/tempo/pull/6532
* @bejaratommy made their first contribution in https://github.com/grafana/tempo/pull/6576
* @jasuade made their first contribution in https://github.com/grafana/tempo/pull/6610
* @antonio-mazzini made their first contribution in https://github.com/grafana/tempo/pull/6609
* @orkhan-huseyn made their first contribution in https://github.com/grafana/tempo/pull/6403
* @ricardbejarano made their first contribution in https://github.com/grafana/tempo/pull/6612
* @rwhitty made their first contribution in https://github.com/grafana/tempo/pull/6561
* @WinterCabbage made their first contribution in https://github.com/grafana/tempo/pull/6578
* @csmarchbanks made their first contribution in https://github.com/grafana/tempo/pull/6684
* @gounthar made their first contribution in https://github.com/grafana/tempo/pull/6756
* @Nouuu made their first contribution in https://github.com/grafana/tempo/pull/6768
* @EoinTrial made their first contribution in https://github.com/grafana/tempo/pull/6905
* @sethmccombs made their first contribution in https://github.com/grafana/tempo/pull/7108

**Full Changelog**: https://github.com/grafana/tempo/compare/v2.10.0-rc.0...v3.0.0-rc.1
