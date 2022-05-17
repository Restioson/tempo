package vparquet

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	tempo_io "github.com/grafana/tempo/pkg/io"
	"github.com/grafana/tempo/pkg/parquetquery"
	pq "github.com/grafana/tempo/pkg/parquetquery"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/encoding/common"
	"github.com/segmentio/parquet-go"
)

type backendReaderAt struct {
	ctx      context.Context
	r        backend.Reader
	name     string
	blockID  uuid.UUID
	tenantID string
}

var _ io.ReaderAt = (*backendReaderAt)(nil)

func (b *backendReaderAt) ReadAt(p []byte, off int64) (int, error) {
	err := b.r.ReadRange(b.ctx, b.name, b.blockID, b.tenantID, uint64(off), p)
	return len(p), err
}

func (b *backendBlock) Search(ctx context.Context, req *tempopb.SearchRequest, opts common.SearchOptions) (*tempopb.SearchResponse, error) {
	rr := &backendReaderAt{ctx, b.r, "data.parquet", b.meta.BlockID, b.meta.TenantID}

	// 16 MB memory buffering
	br := tempo_io.NewBufferedReaderAt(rr, int64(b.meta.Size), 512*1024, 32)

	pf, err := parquet.OpenFile(br, int64(b.meta.Size))
	if err != nil {
		return nil, err
	}

	// Get list of row groups to inspect. Ideally we use predicate pushdown
	// here to keep only row groups that can potentially satisfy the request
	// conditions, but don't have it figured out yet.
	rgs := pf.RowGroups()
	if opts.TotalPages > 0 {
		// Read UP TO TotalPages.  The sharding calculations
		// are just estimates, so it may not line up with the
		// actual number of pages in this file.
		if opts.StartPage+opts.TotalPages > len(rgs) {
			opts.TotalPages = len(rgs) - opts.StartPage
		}
		rgs = rgs[opts.StartPage:opts.TotalPages]
	}

	// TODO: error handling
	results := searchParquetFile(pf, req, rgs)

	fmt.Println("Searched parquet file:", b.meta.BlockID, opts.StartPage, opts.TotalPages, results)

	return results, nil
}

func makePipelineWithRowGroups(req *tempopb.SearchRequest, pf *parquet.File, rgs []parquet.RowGroup) (pq.Iterator, parquetSearchMetrics) {

	makeIter := func(name string, predicate pq.Predicate, selectAs string) pq.Iterator {
		index, _ := pq.GetColumnIndexByPath(pf, name)
		if index == -1 {
			// TODO - don't panic, error instead
			panic("column not found in parquet file:" + name)
		}
		return pq.NewColumnIterator(rgs, index, 1000, predicate, selectAs)
	}

	// Wire up iterators
	var resourceIters []pq.Iterator
	var traceIters []pq.Iterator

	otherAttrConditions := map[string]string{}

	for k, v := range req.Tags {
		switch k {
		case "cluster":
			resourceIters = append(resourceIters, makeIter("rs.Resource.Cluster", pq.NewSubstringPredicate(v), ""))
		case "service.name":
			resourceIters = append(resourceIters, makeIter("rs.Resource.ServiceName", pq.NewSubstringPredicate(v), ""))
		case "namespace":
			resourceIters = append(resourceIters, makeIter("rs.Resource.Namespace", pq.NewSubstringPredicate(v), ""))
		case "pod":
			resourceIters = append(resourceIters, makeIter("rs.Resource.Pod", pq.NewSubstringPredicate(v), ""))
		case "container":
			resourceIters = append(resourceIters, makeIter("rs.Resource.Container", pq.NewSubstringPredicate(v), ""))
		case "name":
			resourceIters = append(resourceIters, makeIter("rs.ils.Spans.Name", pq.NewSubstringPredicate(v), ""))
		default:
			otherAttrConditions[k] = v
		}
	}

	// Generic attribute conditions?
	if len(otherAttrConditions) > 0 {
		// We are looking for one or more foo=bar attributes that aren't
		// projected to their own columns, they are in the generic Key/Value
		// columns at the resource or span levels.  We want to search
		// both locations. But we also only want to read the columns once.

		var keys []string
		var vals []string
		for k, v := range otherAttrConditions {
			keys = append(keys, k)
			vals = append(vals, v)
		}

		keyPred := pq.NewStringInPredicate(keys)
		valPred := pq.NewStringInPredicate(vals)

		// This iterator combines the results from the resource
		// and span searches, and checks if all conditions were satisfied
		// on each ResourceSpans.  This is a single-pass over the attribute columns.
		j := pq.NewUnionIterator(DefinitionLevelResourceSpans, []pq.Iterator{
			// This iterator finds all keys/values at the resource level
			pq.NewJoinIterator(DefinitionLevelResourceAttrs, []pq.Iterator{
				makeIter("rs.Resource.Attrs.Key", keyPred, "keys"),
				makeIter("rs.Resource.Attrs.Value", valPred, "values"),
			}, nil),
			// This iterator finds all keys/values at the span level
			pq.NewJoinIterator(DefinitionLevelResourceSpansILSSpan, []pq.Iterator{
				makeIter("rs.ils.Spans.Attrs.Key", keyPred, "keys"),
				makeIter("rs.ils.Spans.Attrs.Value", valPred, "values"),
			}, nil),
		}, pq.NewKeyValueGroupPredicate(keys, vals))

		resourceIters = append(resourceIters, j)
	}

	// Multiple resource-level filters get joined and wrapped
	// up to trace-level. A single filter can be used as-is
	if len(resourceIters) == 1 {
		traceIters = append(traceIters, resourceIters[0])
	}
	if len(resourceIters) > 1 {
		traceIters = append(traceIters, pq.NewJoinIterator(DefinitionLevelTrace, resourceIters, nil))
	}

	// We always pull back duration for the search results, but it also
	// has a predicate when bounded by the request
	var durFilter pq.Predicate
	if req.MinDurationMs > 0 {
		durFilter = pq.NewIntGreaterThanOrEqualToPredicate((time.Millisecond * time.Duration(req.MinDurationMs)).Nanoseconds())
	}
	traceIters = append(traceIters, makeIter("DurationNanos", durFilter, "Duration"))

	// Join in values for search results. These have
	// no filters so they will always be in the results.
	traceIDMetrics := &parquetquery.InstrumentedPredicate{}
	traceIters = append(traceIters, makeIter("TraceID", traceIDMetrics, "TraceID"))
	traceIters = append(traceIters, makeIter("RootServiceName", nil, "RootServiceName"))
	traceIters = append(traceIters, makeIter("RootSpanName", nil, "RootSpanName"))
	traceIters = append(traceIters, makeIter("StartTimeUnixNano", nil, "StartTime"))

	return pq.NewJoinIterator(DefinitionLevelTrace, traceIters, nil), parquetSearchMetrics{
		pTraceID: traceIDMetrics,
	}
}

func searchParquetFile(pf *parquet.File, req *tempopb.SearchRequest, rgs []parquet.RowGroup) *tempopb.SearchResponse {
	results := []*tempopb.TraceSearchMetadata{}

	iter, metrics := makePipelineWithRowGroups(req, pf, rgs)
	if iter == nil {
		panic("make pipeline failed")
	}
	defer iter.Close()

	for {
		match := iter.Next()
		if match == nil {
			break
		}

		matchMap := match.ToMap()

		result := &tempopb.TraceSearchMetadata{
			TraceID:           matchMap["TraceID"][0].String(),
			RootServiceName:   matchMap["RootServiceName"][0].String(),
			RootTraceName:     matchMap["RootSpanName"][0].String(),
			StartTimeUnixNano: uint64(matchMap["StartTime"][0].Int64()),
			DurationMs:        uint32(matchMap["Duration"][0].Int64() / int64(time.Millisecond)),
		}
		results = append(results, result)

		if req.Limit > 0 && len(results) >= int(req.Limit) {
			break
		}
	}

	return &tempopb.SearchResponse{
		Traces:  results,
		Metrics: metrics.ToProto(),
	}
}

type parquetSearchMetrics struct {
	pTraceID *parquetquery.InstrumentedPredicate
}

func (p *parquetSearchMetrics) ToProto() *tempopb.SearchMetrics {
	return &tempopb.SearchMetrics{
		InspectedTraces: uint32(p.pTraceID.InspectedValues),
	}
}