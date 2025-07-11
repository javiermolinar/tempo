package ingester

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/dskit/user"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/atomic"

	"github.com/grafana/tempo/modules/overrides"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/model/trace"
	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	trace_v1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/grafana/tempo/tempodb/backend"
)

const (
	foo = "foo"
	bar = "bar"
	qux = "qux"
)

func TestInstanceSearch(t *testing.T) {
	i, ingester, tempDir := defaultInstanceAndTmpDir(t)

	tagKey := foo
	tagValue := bar
	ids, _, _, _ := writeTracesForSearch(t, i, "", tagKey, tagValue, false, false)

	req := &tempopb.SearchRequest{
		Query: fmt.Sprintf(`{ span.%s = "%s" }`, tagKey, tagValue),
	}
	req.Limit = uint32(len(ids)) + 1

	// Test after appending to WAL. writeTracesforSearch() makes sure all traces are in the wal
	sr, err := i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, len(ids))
	checkEqual(t, ids, sr)

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	assert.NotEqual(t, blockID, uuid.Nil)

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, len(ids))
	checkEqual(t, ids, sr)

	// Test after completing a block
	err = i.CompleteBlock(context.Background(), blockID)
	require.NoError(t, err)

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, len(ids))
	checkEqual(t, ids, sr)

	err = ingester.stopping(nil)
	require.NoError(t, err)

	// create new ingester.  this should replay wal!
	ingester, _, _ = defaultIngester(t, tempDir)

	i, ok := ingester.getInstanceByID("fake")
	require.True(t, ok)

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, len(ids))
	checkEqual(t, ids, sr)

	err = ingester.stopping(nil)
	require.NoError(t, err)
}

// TestInstanceSearchTraceQL is duplicate of TestInstanceSearch for now
func TestInstanceSearchTraceQL(t *testing.T) {
	queries := []string{
		`{ .service.name = "test-service" }`,
		`{ duration >= 1s }`,
		`{ duration >= 1s && .service.name = "test-service" }`,
	}

	for _, query := range queries {
		t.Run(fmt.Sprintf("Query:%s", query), func(t *testing.T) {
			i, ingester, tmpDir := defaultInstanceAndTmpDir(t)
			// pushTracesToInstance creates traces with:
			// `service.name = "test-service"` and duration >= 1s
			_, ids := pushTracesToInstance(t, i, 10)

			req := &tempopb.SearchRequest{Query: query, Limit: 20, SpansPerSpanSet: 10}

			// Test live traces
			sr, err := i.Search(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, sr.Traces, 0)

			// Test after appending to WAL
			require.NoError(t, i.CutCompleteTraces(0, 0, true))

			sr, err = i.Search(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, sr.Traces, len(ids))
			checkEqual(t, ids, sr)

			// Test after cutting new headBlock
			blockID, err := i.CutBlockIfReady(0, 0, true)
			require.NoError(t, err)
			assert.NotEqual(t, blockID, uuid.Nil)

			sr, err = i.Search(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, sr.Traces, len(ids))
			checkEqual(t, ids, sr)

			// Test after completing a block
			err = i.CompleteBlock(context.Background(), blockID)
			require.NoError(t, err)

			sr, err = i.Search(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, sr.Traces, len(ids))
			checkEqual(t, ids, sr)

			// Test after clearing the completing block
			err = i.ClearCompletingBlock(blockID)
			require.NoError(t, err)

			sr, err = i.Search(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, sr.Traces, len(ids))
			checkEqual(t, ids, sr)

			err = ingester.stopping(nil)
			require.NoError(t, err)

			// create new ingester.  this should replay wal!
			ingester, _, _ = defaultIngester(t, tmpDir)

			i, ok := ingester.getInstanceByID("fake")
			require.True(t, ok)

			sr, err = i.Search(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, sr.Traces, len(ids))
			checkEqual(t, ids, sr)

			err = ingester.stopping(nil)
			require.NoError(t, err)
		})
	}
}

func TestInstanceSearchWithStartAndEnd(t *testing.T) {
	i, ingester, _ := defaultInstanceAndTmpDir(t)

	tagKey := foo
	tagValue := bar
	ids, _, _, _ := writeTracesForSearch(t, i, "", tagKey, tagValue, false, false)

	search := func(req *tempopb.SearchRequest, start, end uint32) *tempopb.SearchResponse {
		req.Start = start
		req.End = end
		sr, err := i.Search(context.Background(), req)
		assert.NoError(t, err)
		return sr
	}

	searchAndAssert := func(req *tempopb.SearchRequest, inspectedTraces uint32) {
		sr := search(req, 0, 0)
		assert.Len(t, sr.Traces, len(ids))
		checkEqual(t, ids, sr)

		// writeTracesForSearch will build spans that end 1 second from now
		// query 2 min range to have extra slack and always be within range
		sr = search(req, uint32(time.Now().Add(-5*time.Minute).Unix()), uint32(time.Now().Add(5*time.Minute).Unix()))
		assert.Len(t, sr.Traces, len(ids))
		checkEqual(t, ids, sr)

		// search with start=5m from now, end=10m from now
		sr = search(req, uint32(time.Now().Add(5*time.Minute).Unix()), uint32(time.Now().Add(10*time.Minute).Unix()))
		// no results and should inspect 100 traces in wal
		assert.Len(t, sr.Traces, 0)
	}

	req := &tempopb.SearchRequest{
		Query: fmt.Sprintf(`{ span.%s = "%s" }`, tagKey, tagValue),
	}
	req.Limit = uint32(len(ids)) + 1

	// Test after appending to WAL.
	// writeTracesforSearch() makes sure all traces are in the wal
	searchAndAssert(req, uint32(100))

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	assert.NotEqual(t, blockID, uuid.Nil)
	searchAndAssert(req, uint32(100))

	// Test after completing a block
	err = i.CompleteBlock(context.Background(), blockID)
	require.NoError(t, err)
	searchAndAssert(req, uint32(200))

	err = ingester.stopping(nil)
	require.NoError(t, err)
}

func checkEqual(t *testing.T, ids [][]byte, sr *tempopb.SearchResponse) {
	for _, meta := range sr.Traces {
		parsedTraceID, err := util.HexStringToTraceID(meta.TraceID)
		assert.NoError(t, err)

		present := false
		for _, id := range ids {
			if bytes.Equal(parsedTraceID, id) {
				present = true
			}
		}
		assert.True(t, present)
	}
}

func TestInstanceSearchTags(t *testing.T) {
	i, _ := defaultInstance(t)

	// add dummy search data
	tagKey := "foo"
	tagValue := bar

	_, expectedTagValues, _, _ := writeTracesForSearch(t, i, "", tagKey, tagValue, true, false)

	userCtx := user.InjectOrgID(context.Background(), "fake")

	// Test after appending to WAL
	testSearchTagsAndValues(t, userCtx, i, tagKey, expectedTagValues)

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	assert.NotEqual(t, blockID, uuid.Nil)

	testSearchTagsAndValues(t, userCtx, i, tagKey, expectedTagValues)

	// Test after completing a block
	err = i.CompleteBlock(context.Background(), blockID)
	require.NoError(t, err)

	testSearchTagsAndValues(t, userCtx, i, tagKey, expectedTagValues)
}

// nolint:revive,unparam
func testSearchTagsAndValues(t *testing.T, ctx context.Context, i *instance, tagName string, expectedTagValues []string) {
	checkSearchTags := func(scope string, contains bool) {
		sr, err := i.SearchTags(ctx, scope)
		require.NoError(t, err)
		require.Greater(t, sr.Metrics.InspectedBytes, uint64(100)) // at least 100 bytes are inspected
		if contains {
			require.Contains(t, sr.TagNames, tagName)
		} else {
			require.NotContains(t, sr.TagNames, tagName)
		}
	}

	checkSearchTags("", true)
	checkSearchTags("span", true)
	// tags are added to the spans and not resources so they should not be present on resource
	checkSearchTags("resource", false)
	checkSearchTags("event", true)
	checkSearchTags("link", true)

	srv, err := i.SearchTagValues(ctx, tagName, 0, 0)
	require.NoError(t, err)
	require.Greater(t, srv.Metrics.InspectedBytes, uint64(100)) // we scanned at-least 100 bytes

	sort.Strings(expectedTagValues)
	sort.Strings(srv.TagValues)
	require.Equal(t, expectedTagValues, srv.TagValues)
}

func TestInstanceSearchTagAndValuesV2(t *testing.T) {
	t.Parallel()
	i, _ := defaultInstance(t)

	// add dummy search data
	var (
		spanName              = "span-name"
		tagKey                = foo
		tagValue              = bar
		otherTagValue         = qux
		queryThatMatches      = fmt.Sprintf(`{ name = "%s" }`, spanName)
		queryThatDoesNotMatch = `{ resource.service.name = "aaaaa" }`
		emptyQuery            = `{ }`
		invalidQuery          = `{ not_a_traceql = query }`
		partInvalidQuery      = fmt.Sprintf(`{ name = "%s" && not_a_traceql = query  }`, spanName)
	)

	_, expectedTagValues, expectedEventTagValues, expectedLinkTagValues := writeTracesForSearch(t, i, spanName, tagKey, tagValue, true, true)
	_, otherTagValues, otherEventTagValues, otherLinkTagValues := writeTracesForSearch(t, i, "other-"+spanName, tagKey, otherTagValue, true, true)

	userCtx := user.InjectOrgID(context.Background(), "fake")

	// Test after appending to WAL
	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, queryThatMatches, expectedTagValues, expectedEventTagValues, expectedLinkTagValues) // Matches the expected tag values
	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, queryThatDoesNotMatch, []string{}, []string{}, []string{})                          // Does not match the expected tag values

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	assert.NotEqual(t, blockID, uuid.Nil)

	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, queryThatMatches, expectedTagValues, expectedEventTagValues, expectedLinkTagValues)

	// Test after completing a block
	err = i.CompleteBlock(context.Background(), blockID)
	require.NoError(t, err)
	require.NoError(t, i.ClearCompletingBlock(blockID)) // Clear the completing block

	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, queryThatMatches, expectedTagValues, expectedEventTagValues, expectedLinkTagValues)

	// test that we are creating cache files for search tag values v2
	// check that we have cache files for all complete blocks for all the cache keys
	limit := i.limiter.Limits().MaxBytesPerTagValuesQuery("fake")
	cacheKeys := cacheKeysForTestSearchTagValuesV2(tagKey, queryThatMatches, limit)
	for _, cacheKey := range cacheKeys {
		for _, b := range i.completeBlocks {
			cache, err := b.GetDiskCache(context.Background(), cacheKey)
			require.NoError(t, err)
			require.NotEmpty(t, cache)
		}
	}

	// test search is returning same results with cache
	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, queryThatMatches, expectedTagValues, expectedEventTagValues, expectedLinkTagValues)

	// merge all tag values to test unfiltered query
	expectedTagValues = append(expectedTagValues, otherTagValues...)
	expectedEventTagValues = append(expectedEventTagValues, otherEventTagValues...)
	expectedLinkTagValues = append(expectedLinkTagValues, otherLinkTagValues...)

	// test un-filtered query and check that bad/invalid TraceQL query returns all tag values and is same as unfiltered query
	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, emptyQuery, expectedTagValues, expectedEventTagValues, expectedLinkTagValues)
	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, invalidQuery, expectedTagValues, expectedEventTagValues, expectedLinkTagValues)
	testSearchTagsAndValuesV2(t, userCtx, i, tagKey, partInvalidQuery, expectedTagValues, expectedEventTagValues, expectedLinkTagValues)
}

// nolint:revive,unparam
func testSearchTagsAndValuesV2(
	t *testing.T,
	ctx context.Context,
	i *instance,
	tagName, query string,
	expectedTagValues []string,
	expectedEventTagValues []string,
	expectedLinkTagValues []string,
) {
	tagsResp, err := i.SearchTags(ctx, "none")
	require.NoError(t, err)
	require.Greater(t, tagsResp.Metrics.InspectedBytes, uint64(100))

	checkTagValues := func(scope string, expectedValues []string) {
		tagValuesResp, err := i.SearchTagValuesV2(ctx, &tempopb.SearchTagValuesRequest{
			TagName: fmt.Sprintf("%s.%s", scope, tagName),
			Query:   query,
		})
		require.NoError(t, err)
		// we scanned at-least 100 bytes
		require.Greater(t, tagValuesResp.Metrics.InspectedBytes, uint64(100))

		tagValues := make([]string, 0, len(tagValuesResp.TagValues))
		for _, v := range tagValuesResp.TagValues {
			tagValues = append(tagValues, v.Value)
		}

		sort.Strings(tagValues)
		sort.Strings(expectedValues)
		require.Contains(t, tagsResp.TagNames, tagName)
		require.Equal(t, expectedValues, tagValues)
	}

	checkTagValues("span", expectedTagValues)
	checkTagValues("event", expectedEventTagValues)
	checkTagValues("link", expectedLinkTagValues)
	checkTagValues("instrumentation", expectedTagValues)
}

func cacheKeysForTestSearchTagValuesV2(tagKey, query string, limit int) []string {
	scopes := []string{"span", "event", "link", "instrumentation"}
	cacheKeys := make([]string, 0, len(scopes))

	for _, prefix := range scopes {
		req := &tempopb.SearchTagValuesRequest{
			TagName: fmt.Sprintf("%s.%s", prefix, tagKey),
			Query:   query,
		}
		cacheKey := searchTagValuesV2CacheKey(req, limit, "cache_search_tagvaluesv2")
		cacheKeys = append(cacheKeys, cacheKey)
	}

	return cacheKeys
}

// TestInstanceSearchTagsSpecialCases tess that SearchTags errors on an unknown scope and
// returns known instrinics for the "intrinsic" scope
func TestInstanceSearchUnknownScope(t *testing.T) {
	i, _ := defaultInstance(t)
	userCtx := user.InjectOrgID(context.Background(), "fake")

	resp, err := i.SearchTags(userCtx, "foo")
	require.Error(t, err)
	require.Nil(t, resp)
}

// TestInstanceSearchMaxBytesPerTagValuesQueryReturnsPartial confirms that SearchTagValues returns
// partial results if the bytes of the found tag value exceeds the MaxBytesPerTagValuesQuery limit
func TestInstanceSearchMaxBytesPerTagValuesQueryReturnsPartial(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Config{
		Defaults: overrides.Overrides{
			Read: overrides.ReadOverrides{
				MaxBytesPerTagValuesQuery: 12,
			},
		},
	}, nil, prometheus.DefaultRegisterer)
	assert.NoError(t, err, "unexpected error creating limits")
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	tempDir := t.TempDir()

	ingester, _, _ := defaultIngester(t, tempDir)
	ingester.limiter = limiter
	i, err := ingester.getOrCreateInstance("fake")
	assert.NoError(t, err, "unexpected error creating new instance")

	tagKey := foo
	tagValue := bar

	_, _, _, _ = writeTracesForSearch(t, i, "", tagKey, tagValue, true, false)

	userCtx := user.InjectOrgID(context.Background(), "fake")
	resp, err := i.SearchTagValues(userCtx, tagKey, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 2, len(resp.TagValues)) // Only two values of the form "bar123" fit in the 12 byte limit above.
}

// TestInstanceSearchMaxBytesPerTagValuesQueryReturnsPartial confirms that SearchTagValues returns
// partial results if the bytes of the found tag value exceeds the MaxBytesPerTagValuesQuery limit
func TestInstanceSearchMaxBlocksPerTagValuesQueryReturnsPartial(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Config{
		Defaults: overrides.Overrides{
			Read: overrides.ReadOverrides{
				MaxBlocksPerTagValuesQuery: 1,
			},
		},
	}, nil, prometheus.DefaultRegisterer)
	assert.NoError(t, err, "unexpected error creating limits")
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	tempDir := t.TempDir()

	ingester, _, _ := defaultIngester(t, tempDir)
	ingester.limiter = limiter
	i, err := ingester.getOrCreateInstance("fake")
	assert.NoError(t, err, "unexpected error creating new instance")

	tagKey := foo
	tagValue := bar

	_, _, _, _ = writeTracesForSearch(t, i, "", tagKey, tagValue, true, false)

	// Cut the headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	assert.NotEqual(t, blockID, uuid.Nil)

	// Write more traces
	_, _, _, _ = writeTracesForSearch(t, i, "", tagKey, "another-"+bar, true, false)

	userCtx := user.InjectOrgID(context.Background(), "fake")

	respV1, err := i.SearchTagValues(userCtx, tagKey, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 100, len(respV1.TagValues))

	respV2, err := i.SearchTagValuesV2(userCtx, &tempopb.SearchTagValuesRequest{TagName: fmt.Sprintf(".%s", tagKey)})
	require.NoError(t, err)
	assert.Equal(t, 100, len(respV2.TagValues))

	// Now test with unlimited blocks
	limits, err = overrides.NewOverrides(overrides.Config{}, nil, prometheus.DefaultRegisterer)
	assert.NoError(t, err, "unexpected error creating limits")

	i.limiter = NewLimiter(limits, &ringCountMock{count: 1}, 1)

	respV1, err = i.SearchTagValues(userCtx, tagKey, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 200, len(respV1.TagValues))

	respV2, err = i.SearchTagValuesV2(userCtx, &tempopb.SearchTagValuesRequest{TagName: fmt.Sprintf(".%s", tagKey)})
	require.NoError(t, err)
	assert.Equal(t, 200, len(respV2.TagValues))
}

// writes traces to the given instance along with search data. returns
// ids expected to be returned from a tag search and strings expected to
// be returned from a tag value search
// nolint:revive,unparam
func writeTracesForSearch(t *testing.T, i *instance, spanName, tagKey, tagValue string, postFixValue bool, includeEventLink bool) ([][]byte, []string, []string, []string) {
	// This matches the encoding for live traces, since
	// we are pushing to the instance directly it must match.
	dec := model.MustNewSegmentDecoder(model.CurrentEncoding)

	numTraces := 100
	ids := make([][]byte, 0, numTraces)
	expectedTagValues := make([]string, 0, numTraces)
	expectedEventTagValues := make([]string, 0, numTraces)
	expectedLinkTagValues := make([]string, 0, numTraces)

	now := time.Now()
	for j := 0; j < numTraces; j++ {
		id := make([]byte, 16)
		_, err := crand.Read(id)
		require.NoError(t, err)

		tv := tagValue
		if postFixValue {
			tv = tv + strconv.Itoa(j)
		}
		kv := &v1.KeyValue{Key: tagKey, Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: tv}}}
		eTv := "event-" + tv
		lTv := "link-" + tv
		eventKv := &v1.KeyValue{Key: tagKey, Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: eTv}}}
		linkKv := &v1.KeyValue{Key: tagKey, Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: lTv}}}
		expectedTagValues = append(expectedTagValues, tv)
		if includeEventLink {
			expectedEventTagValues = append(expectedEventTagValues, eTv)
			expectedLinkTagValues = append(expectedLinkTagValues, lTv)
		}
		ids = append(ids, id)

		testTrace := test.MakeTrace(10, id)
		// add the time
		for _, batch := range testTrace.ResourceSpans {
			for _, ils := range batch.ScopeSpans {
				ils.Scope = &v1.InstrumentationScope{
					Name:       "scope-name",
					Version:    "scope-version",
					Attributes: []*v1.KeyValue{kv},
				}
				for _, span := range ils.Spans {
					span.Name = spanName
					span.StartTimeUnixNano = uint64(now.UnixNano())
					span.EndTimeUnixNano = uint64(now.UnixNano())
				}
			}
		}
		testTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes = append(testTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes, kv)
		// add link and event
		event := &trace_v1.Span_Event{Name: "event-name", Attributes: []*v1.KeyValue{eventKv}}
		link := &trace_v1.Span_Link{TraceId: id, SpanId: id, Attributes: []*v1.KeyValue{linkKv}}
		testTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Events = append(testTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Events, event)
		testTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Links = append(testTrace.ResourceSpans[0].ScopeSpans[0].Spans[0].Links, link)

		trace.SortTrace(testTrace)

		// // Print trace as json string
		// buf := &bytes.Buffer{}
		// require.NoError(t, (&jsonpb.Marshaler{}).Marshal(buf, testTrace))

		traceBytes, err := dec.PrepareForWrite(testTrace, uint32(now.Unix()), uint32(now.Unix()))
		require.NoError(t, err)

		// searchData will be nil if not
		err = i.PushBytes(context.Background(), id, traceBytes)
		require.NoError(t, err)
	}

	// traces have to be cut to show up in searches
	err := i.CutCompleteTraces(0, 0, true)
	require.NoError(t, err)

	return ids, expectedTagValues, expectedEventTagValues, expectedLinkTagValues
}

func TestInstanceSearchNoData(t *testing.T) {
	i, _ := defaultInstance(t)

	req := &tempopb.SearchRequest{
		Query: "{}",
	}

	sr, err := i.Search(context.Background(), req)
	assert.NoError(t, err)
	require.Len(t, sr.Traces, 0)
}

func TestInstanceSearchDoesNotRace(t *testing.T) {
	ingester, _, _ := defaultIngester(t, t.TempDir())
	i, err := ingester.getOrCreateInstance("fake")
	require.NoError(t, err)

	// This matches the encoding for live traces, since
	// we are pushing to the instance directly it must match.
	dec := model.MustNewSegmentDecoder(model.CurrentEncoding)

	// add dummy search data
	tagKey := foo
	tagValue := "bar"

	req := &tempopb.SearchRequest{
		Query: fmt.Sprintf(`{ span.%s = "%s" }`, tagKey, tagValue),
	}

	end := make(chan struct{})
	wg := sync.WaitGroup{}

	concurrent := func(f func()) {
		wg.Add(1)
		defer wg.Done()
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}

	go concurrent(func() {
		id := make([]byte, 16)
		_, err := crand.Read(id)
		require.NoError(t, err)

		trace := test.MakeTrace(10, id)
		traceBytes, err := dec.PrepareForWrite(trace, 0, 0)
		require.NoError(t, err)

		// searchData will be nil if not
		err = i.PushBytes(context.Background(), id, traceBytes)
		require.NoError(t, err)
	})

	go concurrent(func() {
		err := i.CutCompleteTraces(0, 0, true)
		require.NoError(t, err, "error cutting complete traces")
	})

	go concurrent(func() {
		_, err := i.FindTraceByID(context.Background(), []byte{0x01}, false)
		assert.NoError(t, err, "error finding trace by id")
	})

	go concurrent(func() {
		// Cut wal, complete, delete wal, then flush
		blockID, _ := i.CutBlockIfReady(0, 0, true)
		if blockID != uuid.Nil {
			err := i.CompleteBlock(context.Background(), blockID)
			require.NoError(t, err)
			err = i.ClearCompletingBlock(blockID)
			require.NoError(t, err)
			block := i.GetBlockToBeFlushed(blockID)
			require.NotNil(t, block)
			err = ingester.store.WriteBlock(context.Background(), block)
			require.NoError(t, err)
		}
	})

	go concurrent(func() {
		err = i.ClearOldBlocks(ingester.cfg.FlushObjectStorage, 0)
		require.NoError(t, err)
	})

	go concurrent(func() {
		_, err := i.Search(context.Background(), req)
		require.NoError(t, err, "error searching")
	})

	go concurrent(func() {
		// SearchTags queries now require userID in ctx
		ctx := user.InjectOrgID(context.Background(), "test")
		_, err := i.SearchTags(ctx, "")
		require.NoError(t, err, "error getting search tags")
	})

	go concurrent(func() {
		// SearchTagValues queries now require userID in ctx
		ctx := user.InjectOrgID(context.Background(), "test")
		_, err := i.SearchTagValues(ctx, tagKey, 0, 0)
		require.NoError(t, err, "error getting search tag values")
	})

	time.Sleep(2000 * time.Millisecond)
	close(end)
	// Wait for go funcs to quit before
	// exiting and cleaning up
	wg.Wait()
}

func TestWALBlockDeletedDuringSearch(t *testing.T) {
	i, _ := defaultInstance(t)

	// This matches the encoding for live traces, since
	// we are pushing to the instance directly it must match.
	dec := model.MustNewSegmentDecoder(model.CurrentEncoding)

	end := make(chan struct{})

	concurrent := func(f func()) {
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}

	for j := 0; j < 500; j++ {
		id := make([]byte, 16)
		_, err := crand.Read(id)
		require.NoError(t, err)

		trace := test.MakeTrace(10, id)
		traceBytes, err := dec.PrepareForWrite(trace, 0, 0)
		require.NoError(t, err)

		err = i.PushBytes(context.Background(), id, traceBytes)
		require.NoError(t, err)
	}

	err := i.CutCompleteTraces(0, 0, true)
	require.NoError(t, err)

	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)

	go concurrent(func() {
		_, err := i.Search(context.Background(), &tempopb.SearchRequest{
			Query: `{ span.wuv = "xyz" }`,
		})
		require.NoError(t, err)
	})

	// Let search get going
	time.Sleep(100 * time.Millisecond)

	err = i.ClearCompletingBlock(blockID)
	require.NoError(t, err)

	// Wait for go funcs to quit before
	// exiting and cleaning up
	close(end)
	time.Sleep(2 * time.Second)
}

func TestInstanceSearchMetrics(t *testing.T) {
	t.Parallel()
	i, _ := defaultInstance(t)

	// This matches the encoding for live traces, since
	// we are pushing to the instance directly it must match.
	dec := model.MustNewSegmentDecoder(model.CurrentEncoding)

	numTraces := uint32(500)
	numBytes := uint64(0)
	for j := uint32(0); j < numTraces; j++ {
		id := test.ValidTraceID(nil)

		// Trace bytes have to be pushed in the expected data encoding
		trace := test.MakeTrace(10, id)

		traceBytes, err := dec.PrepareForWrite(trace, 0, 0)
		require.NoError(t, err)

		err = i.PushBytes(context.Background(), id, traceBytes)
		require.NoError(t, err)
	}

	search := func() *tempopb.SearchMetrics {
		sr, err := i.Search(context.Background(), &tempopb.SearchRequest{
			Query: fmt.Sprintf(`{ span.%s = "%s" }`, "foo", "bar"),
		})
		require.NoError(t, err)
		return sr.Metrics
	}

	// Live traces
	m := search()
	require.Equal(t, uint32(0), m.InspectedTraces) // we don't search live traces
	require.Equal(t, uint64(0), m.InspectedBytes)  // we don't search live traces

	// Test after appending to WAL
	err := i.CutCompleteTraces(0, 0, true)
	require.NoError(t, err)
	m = search()
	require.Less(t, numBytes, m.InspectedBytes)

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	m = search()
	require.Less(t, numBytes, m.InspectedBytes)

	// Test after completing a block
	err = i.CompleteBlock(context.Background(), blockID)
	require.NoError(t, err)
	err = i.ClearCompletingBlock(blockID)
	require.NoError(t, err)
	m = search()
	require.Less(t, numBytes, m.InspectedBytes)
}

func BenchmarkInstanceSearchUnderLoad(b *testing.B) {
	ctx := context.TODO()

	i, _ := defaultInstance(b)

	// This matches the encoding for live traces, since
	// we are pushing to the instance directly it must match.
	dec := model.MustNewSegmentDecoder(model.CurrentEncoding)

	end := make(chan struct{})

	concurrent := func(f func()) {
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}

	// Push data
	var tracesPushed atomic.Int32
	for j := 0; j < 2; j++ {
		go concurrent(func() {
			id := test.ValidTraceID(nil)

			trace := test.MakeTrace(10, id)
			traceBytes, err := dec.PrepareForWrite(trace, 0, 0)
			require.NoError(b, err)

			// searchData will be nil if not
			err = i.PushBytes(context.Background(), id, traceBytes)
			require.NoError(b, err)

			tracesPushed.Inc()
		})
	}

	cuts := 0
	go concurrent(func() {
		time.Sleep(250 * time.Millisecond)
		err := i.CutCompleteTraces(0, 0, true)
		require.NoError(b, err, "error cutting complete traces")
		cuts++
	})

	go concurrent(func() {
		// Slow this down to prevent "too many open files" error
		time.Sleep(100 * time.Millisecond)
		_, err := i.CutBlockIfReady(0, 0, true)
		require.NoError(b, err)
	})

	var searches atomic.Int32
	var bytesInspected atomic.Uint64
	var tracesInspected atomic.Uint32

	for j := 0; j < 2; j++ {
		go concurrent(func() {
			// time.Sleep(1 * time.Millisecond)
			req := &tempopb.SearchRequest{}
			resp, err := i.Search(ctx, req)
			require.NoError(b, err)
			searches.Inc()
			bytesInspected.Add(resp.Metrics.InspectedBytes)
			tracesInspected.Add(resp.Metrics.InspectedTraces)
		})
	}

	b.ResetTimer()
	start := time.Now()
	time.Sleep(time.Duration(b.N) * time.Millisecond)
	elapsed := time.Since(start)

	fmt.Printf(
		"Instance search throughput under load: %v elapsed %.2f MB = %.2f MiB/s throughput inspected %.2f traces/s pushed %.2f traces/s %.2f searches/s %.2f cuts/s\n",
		elapsed,
		float64(bytesInspected.Load())/(1024*1024),
		float64(bytesInspected.Load())/(elapsed.Seconds())/(1024*1024),
		float64(tracesInspected.Load())/(elapsed.Seconds()),
		float64(tracesPushed.Load())/(elapsed.Seconds()),
		float64(searches.Load())/(elapsed.Seconds()),
		float64(cuts)/(elapsed.Seconds()),
	)

	b.StopTimer()
	close(end)
	// Wait for go funcs to quit before
	// exiting and cleaning up
	time.Sleep(1 * time.Second)
}

func TestIncludeBlock(t *testing.T) {
	tests := []struct {
		blocKStart int64
		blockEnd   int64
		reqStart   uint32
		reqEnd     uint32
		expected   bool
	}{
		// if request is 0s, block start/end don't matter
		{
			blocKStart: 100,
			blockEnd:   200,
			reqStart:   0,
			reqEnd:     0,
			expected:   true,
		},
		// req before
		{
			blocKStart: 100,
			blockEnd:   200,
			reqStart:   50,
			reqEnd:     99,
			expected:   false,
		},
		// overlap front
		{
			blocKStart: 100,
			blockEnd:   200,
			reqStart:   50,
			reqEnd:     150,
			expected:   true,
		},
		// inside block
		{
			blocKStart: 100,
			blockEnd:   200,
			reqStart:   110,
			reqEnd:     150,
			expected:   true,
		},
		// overlap end
		{
			blocKStart: 100,
			blockEnd:   200,
			reqStart:   150,
			reqEnd:     250,
			expected:   true,
		},
		// after block
		{
			blocKStart: 100,
			blockEnd:   200,
			reqStart:   201,
			reqEnd:     250,
			expected:   false,
		},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d-%d-%d-%d", tc.blocKStart, tc.blockEnd, tc.reqStart, tc.reqEnd), func(t *testing.T) {
			actual := includeBlock(&backend.BlockMeta{
				StartTime: time.Unix(tc.blocKStart, 0),
				EndTime:   time.Unix(tc.blockEnd, 0),
			}, &tempopb.SearchRequest{
				Start: tc.reqStart,
				End:   tc.reqEnd,
			})

			require.Equal(t, tc.expected, actual)
		})
	}
}

func Test_searchTagValuesV2CacheKey(t *testing.T) {
	tests := []struct {
		name             string
		req              *tempopb.SearchTagValuesRequest
		limit            int
		prefix           string
		expectedCacheKey string
	}{
		{
			name:             "prefix empty",
			req:              &tempopb.SearchTagValuesRequest{TagName: "span.foo", Query: "{}"},
			limit:            100,
			prefix:           "",
			expectedCacheKey: "_10963035328899851375.buf",
		},
		{
			name:   "prefix not empty but same query",
			req:    &tempopb.SearchTagValuesRequest{TagName: "span.foo", Query: "{}"},
			limit:  100,
			prefix: "my_amazing_prefix",
			// hash should be same, only prefix should change
			expectedCacheKey: "my_amazing_prefix_10963035328899851375.buf",
		},
		{
			name:             "changing limit changes the cache key for same query",
			req:              &tempopb.SearchTagValuesRequest{TagName: "span.foo", Query: "{}"},
			limit:            500,
			prefix:           "my_amazing_prefix",
			expectedCacheKey: "my_amazing_prefix_10962052365504419966.buf",
		},
		{
			name:             "different query generates different cache key",
			req:              &tempopb.SearchTagValuesRequest{TagName: "span.foo", Query: "{ name = \"foo\" }"},
			limit:            500,
			prefix:           "my_amazing_prefix",
			expectedCacheKey: "my_amazing_prefix_9241051696576633442.buf",
		},
		{
			name:             "invalid query generates a valid cache key",
			req:              &tempopb.SearchTagValuesRequest{TagName: "span.foo", Query: "{span.env=dev}"},
			limit:            500,
			prefix:           "my_amazing_prefix",
			expectedCacheKey: "my_amazing_prefix_7849238702443650194.buf",
		},
		{
			name:             "different invalid query generates the same valid cache key",
			req:              &tempopb.SearchTagValuesRequest{TagName: "span.foo", Query: "{ <not valid traceql> && span.foo = \"bar\" }"},
			limit:            500,
			prefix:           "my_amazing_prefix",
			expectedCacheKey: "my_amazing_prefix_7849238702443650194.buf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheKey := searchTagValuesV2CacheKey(tt.req, tt.limit, tt.prefix)
			require.Equal(t, tt.expectedCacheKey, cacheKey)
		})
	}
}
