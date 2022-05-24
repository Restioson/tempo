package vparquet

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/backend/local"
	"github.com/grafana/tempo/tempodb/encoding/common"
	"github.com/stretchr/testify/require"
)

func TestBackendBlockFindTraceByID(t *testing.T) {
	rawR, rawW, _, err := local.New(&local.Config{
		Path: t.TempDir(),
	})
	require.NoError(t, err)

	r := backend.NewReader(rawR)
	w := backend.NewWriter(rawW)
	ctx := context.Background()

	cfg := &common.BlockConfig{
		BloomFP:             0.01,
		BloomShardSizeBytes: 100 * 1024,
	}

	meta := backend.NewBlockMeta("fake", uuid.New(), VersionString, backend.EncNone, "")
	meta.TotalObjects = 1

	id := test.ValidTraceID(nil)

	s, err := NewStreamingBlock(ctx, cfg, meta, r, w)
	require.NoError(t, err)

	bar := "bar"
	s.Add(&Trace{
		TraceID: util.TraceIDToHexString(test.ValidTraceID(nil)),
		ResourceSpans: []ResourceSpans{
			{
				Resource: Resource{
					ServiceName: "s",
				},
				InstrumentationLibrarySpans: []ILS{
					{
						Spans: []Span{
							{
								Name: "hello",
								Attrs: []Attribute{
									{Key: "foo", Value: &bar},
								},
								ID:           []byte{},
								ParentSpanID: []byte{},
							},
						},
					},
				},
			},
		},
	})

	wantTr := &Trace{
		TraceID: util.TraceIDToHexString(id),
		ResourceSpans: []ResourceSpans{
			{
				Resource: Resource{
					ServiceName: "s",
				},
				InstrumentationLibrarySpans: []ILS{
					{
						Spans: []Span{
							{
								Name: "hello",
								Attrs: []Attribute{
									{Key: "foo", Value: &bar},
								},
								ID:           []byte{},
								ParentSpanID: []byte{},
							},
						},
					},
				},
			},
		},
	}

	s.Add(wantTr)

	_, err = s.Complete()
	require.NoError(t, err)

	b, err := NewBackendBlock(s.meta, r)
	require.NoError(t, err)

	gotTr, err := b.FindTraceByID(ctx, id)
	require.NoError(t, err)

	wantProto, err := parquetTraceToTempopbTrace(wantTr)
	require.NoError(t, err)

	require.Equal(t, wantProto, gotTr)
}