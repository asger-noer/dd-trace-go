// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/instrumentation/httpmem"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
	"github.com/DataDog/dd-trace-go/v2/internal/samplernames"

	"github.com/DataDog/datadog-go/v5/statsd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const otelHeaderPropagationStyle = "OTEL_PROPAGATORS"

func traceIDFrom64Bits(i uint64) traceID {
	t := traceID{}
	t.SetLower(i)
	return t
}

func traceIDFrom128Bits(u, l uint64) traceID {
	t := traceID{}
	t.SetLower(l)
	t.SetUpper(u)
	return t
}

func TestHTTPHeadersCarrierSet(t *testing.T) {
	h := http.Header{}
	c := HTTPHeadersCarrier(h)
	c.Set("A", "x")
	assert.Equal(t, "x", h.Get("A"))
}

func TestHTTPHeadersCarrierForeachKey(t *testing.T) {
	h := http.Header{}
	h.Add("A", "x")
	h.Add("B", "y")
	got := map[string]string{}
	err := HTTPHeadersCarrier(h).ForeachKey(func(k, v string) error {
		got[k] = v
		return nil
	})
	assert := assert.New(t)
	assert.Nil(err)
	assert.Equal("x", h.Get("A"))
	assert.Equal("y", h.Get("B"))
}

func TestHTTPHeadersCarrierForeachKeyError(t *testing.T) {
	want := errors.New("random error")
	h := http.Header{}
	h.Add("A", "x")
	h.Add("B", "y")
	got := HTTPHeadersCarrier(h).ForeachKey(func(k, _ string) error {
		if k == "B" {
			return want
		}
		return nil
	})
	assert.Equal(t, want, got)
}

func TestTextMapCarrierSet(t *testing.T) {
	m := map[string]string{}
	c := TextMapCarrier(m)
	c.Set("a", "b")
	assert.Equal(t, "b", m["a"])
}

func TestTextMapCarrierForeachKey(t *testing.T) {
	want := map[string]string{"A": "x", "B": "y"}
	got := map[string]string{}
	err := TextMapCarrier(want).ForeachKey(func(k, v string) error {
		got[k] = v
		return nil
	})
	assert := assert.New(t)
	assert.Nil(err)
	assert.Equal(got, want)
}

func TestTextMapCarrierForeachKeyError(t *testing.T) {
	m := map[string]string{"A": "x", "B": "y"}
	want := errors.New("random error")
	got := TextMapCarrier(m).ForeachKey(func(_, _ string) error {
		return want
	})
	assert.Equal(t, got, want)
}

func TestTextMapExtractTracestatePropagation(t *testing.T) {
	tests := []struct {
		name, propagationStyle, traceparent string
		onlyExtractFirst                    bool // value of DD_TRACE_PROPAGATION_EXTRACT_FIRST
		wantTracestatePropagation           bool
		conflictingParentID                 bool
	}{
		{
			/*
				With only Datadog propagation set, the tracestate header should
				be ignored, and not propagated to the returned trace context.
			*/
			name:             "datadog-only",
			propagationStyle: "datadog",
			traceparent:      "00-00000000000000000000000000000004-2222222222222222-01",
		},
		{
			/*
				With Datadog, B3, AND w3c propagation set, the tracestate header should
				be propagated to the returned trace context. This test also verifies that
				b3 extraction doesn't override the local context value.
			*/
			name:                      "datadog-b3-w3c",
			propagationStyle:          "datadog,b3,tracecontext",
			traceparent:               "00-00000000000000000000000000000004-2222222222222222-01",
			wantTracestatePropagation: true,
			conflictingParentID:       true,
		},
		{
			/*
				With Datadog AND w3c propagation set, the tracestate header should
				be propagated to the returned trace context.
			*/
			name:                      "datadog-and-w3c",
			propagationStyle:          "datadog,tracecontext",
			traceparent:               "00-00000000000000000000000000000004-2222222222222222-01",
			wantTracestatePropagation: true,
			conflictingParentID:       true,
		},
		{
			/*
				With Datadog AND w3c propagation set, but mismatching trace-ids,
				the tracestate header should be ignored and not propagated to
				the returned trace context.
			*/
			name:             "datadog-and-w3c-mismatching-ids",
			propagationStyle: "datadog,tracecontext",
			traceparent:      "00-00000000000000000000000000000088-2222222222222222-01",
		},
		{
			/*
				With Datadog AND w3c propagation set, but the traceparent is malformed,
				the tracestate header should be ignored and not propagated to
				the returned trace context.
			*/
			name:             "datadog-and-w3c-malformed",
			propagationStyle: "datadog,tracecontext",
			traceparent:      "00-00000000000000000000000000000004-22asdf!2-01",
		},
		{
			/*
				With Datadog AND w3c propagation set, but there is no traceparent,
				the tracestate header should be ignored and not propagated to
				the returned trace context.
			*/
			name:             "datadog-and-w3c-no-traceparent",
			propagationStyle: "datadog,tracecontext",
		},
		{
			/*
				With Datadog AND w3c propagation set, but DD_TRACE_PROPAGATION_EXTRACT_FIRST
				is true, the tracestate header should be ignored and not propagated to
				the returned trace context.
			*/
			name:             "datadog-and-w3c-only-extract-first",
			propagationStyle: "datadog,tracecontext",
			traceparent:      "00-00000000000000000000000000000004-2222222222222222-01",
			onlyExtractFirst: true,
		},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("TestTextMapExtractTracestatePropagation-%s", tc.name), func(t *testing.T) {
			t.Setenv(headerPropagationStyle, tc.propagationStyle)
			if tc.onlyExtractFirst {
				t.Setenv("DD_TRACE_PROPAGATION_EXTRACT_FIRST", "true")
			}
			tracer, err := newTracer()
			assert := assert.New(t)
			assert.NoError(err)
			headers := TextMapCarrier(map[string]string{
				DefaultTraceIDHeader:  "4",
				DefaultParentIDHeader: "1",
				originHeader:          "synthetics",
				b3TraceIDHeader:       "0021dc1807524785",
				traceparentHeader:     tc.traceparent,
				tracestateHeader:      "dd=s:2;o:rum;p:0000000000000001;t.tid:1230000000000000~~,othervendor=t61rcWkgMzE",
			})

			sctx, err := tracer.Extract(headers)
			assert.Nil(err)
			assert.Equal("00000000000000000000000000000004", sctx.traceID.HexEncoded())
			if tc.conflictingParentID == true {
				// tracecontext span id should be used
				assert.Equal(uint64(0x2222222222222222), sctx.spanID)
			} else {
				// should use x-datadog-parent-id, not the id in the tracestate
				assert.Equal(uint64(1), sctx.spanID)
			}
			assert.Equal("synthetics", sctx.origin) // should use x-datadog-origin, not the origin in the tracestate
			if tc.wantTracestatePropagation {
				assert.Equal("0000000000000001", sctx.reparentID)
				assert.Equal("dd=s:0;o:synthetics;p:0000000000000001,othervendor=t61rcWkgMzE", sctx.trace.propagatingTag(tracestateHeader))
			} else if sctx.trace != nil {
				assert.False(sctx.trace.hasPropagatingTag(tracestateHeader))
			}
		})
	}
}

func TestTextMapPropagatorErrors(t *testing.T) {
	t.Setenv(headerPropagationStyleExtract, "datadog")
	propagator := NewPropagator(nil)
	assert := assert.New(t)

	err := propagator.Inject(&SpanContext{}, 2)
	assert.Equal(ErrInvalidCarrier, err)
	err = propagator.Inject(nil, TextMapCarrier(map[string]string{}))
	assert.Equal(ErrInvalidSpanContext, err)
	err = propagator.Inject(&SpanContext{}, TextMapCarrier(map[string]string{}))
	assert.Equal(ErrInvalidSpanContext, err) // no traceID and spanID
	err = propagator.Inject(&SpanContext{traceID: traceIDFrom64Bits(1)}, TextMapCarrier(map[string]string{}))
	assert.Equal(ErrInvalidSpanContext, err) // no spanID

	_, err = propagator.Extract(2)
	assert.Equal(ErrInvalidCarrier, err)

	_, err = propagator.Extract(TextMapCarrier(map[string]string{
		DefaultTraceIDHeader:  "1",
		DefaultParentIDHeader: "A",
	}))
	assert.Equal(ErrSpanContextCorrupted, err)

	_, err = propagator.Extract(TextMapCarrier(map[string]string{
		DefaultTraceIDHeader:  "A",
		DefaultParentIDHeader: "2",
	}))
	assert.Equal(ErrSpanContextCorrupted, err)

	_, err = propagator.Extract(TextMapCarrier(map[string]string{
		DefaultTraceIDHeader:  "0",
		DefaultParentIDHeader: "0",
	}))
	assert.Equal(ErrSpanContextNotFound, err)

	_, err = propagator.Extract(TextMapCarrier(map[string]string{
		DefaultTraceIDHeader:  "3",
		DefaultParentIDHeader: "0",
	}))
	assert.Equal(ErrSpanContextNotFound, err)
}

func TestTextMapPropagatorInjectHeader(t *testing.T) {
	assert := assert.New(t)

	propagator := NewPropagator(&PropagatorConfig{
		BaggagePrefix: "bg-",
		TraceHeader:   "tid",
		ParentHeader:  "pid",
	})
	tracer, err := newTracer(WithPropagator(propagator))
	defer tracer.Stop()
	assert.NoError(err)

	root := tracer.StartSpan("web.request")
	root.SetBaggageItem("item", "x")
	root.setSamplingPriority(ext.PriorityAutoReject, samplernames.Default)
	ctx := root.Context()
	headers := http.Header{}

	carrier := HTTPHeadersCarrier(headers)
	err = tracer.Inject(ctx, carrier)
	assert.Nil(err)

	tid := strconv.FormatUint(root.traceID, 10)
	pid := strconv.FormatUint(root.spanID, 10)

	assert.Equal(headers.Get("tid"), tid)
	assert.Equal(headers.Get("pid"), pid)
	assert.Equal(headers.Get("bg-item"), "x")
	assert.Equal(headers.Get(DefaultPriorityHeader), "0")
}

func TestTextMapPropagatorOrigin(t *testing.T) {
	t.Setenv(headerPropagationStyleExtract, "datadog")
	t.Setenv(headerPropagationStyleInject, "datadog")
	src := TextMapCarrier(map[string]string{
		originHeader:          "synthetics",
		DefaultTraceIDHeader:  "1",
		DefaultParentIDHeader: "1",
	})
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(t, err)
	ctx, err := tracer.Extract(src)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.origin != "synthetics" {
		t.Fatalf("didn't propagate origin, got: %q", ctx.origin)
	}
	dst := map[string]string{}
	if err := tracer.Inject(ctx, TextMapCarrier(dst)); err != nil {
		t.Fatal(err)
	}
	if dst[originHeader] != "synthetics" {
		t.Fatal("didn't inject header")
	}
}

func TestTextMapPropagatorTraceTagsWithPriority(t *testing.T) {
	t.Setenv(headerPropagationStyleExtract, "datadog")
	t.Setenv(headerPropagationStyleInject, "datadog")
	src := TextMapCarrier(map[string]string{
		DefaultPriorityHeader: "1",
		DefaultTraceIDHeader:  "1",
		DefaultParentIDHeader: "1",
		traceTagsHeader:       "hello=world=,_dd.p.dm=934086a6-4",
	})
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(t, err)
	ctx, err := tracer.Extract(src)
	assert.Nil(t, err)
	child := tracer.StartSpan("test", ChildOf(ctx))
	childSpanID := child.Context().spanID
	assert.Equal(t, map[string]string{
		"hello":    "world=",
		"_dd.p.dm": "934086a6-4",
	}, ctx.trace.propagatingTags)
	dst := map[string]string{}
	err = tracer.Inject(child.Context(), TextMapCarrier(dst))
	assert.Nil(t, err)
	assert.Len(t, dst, 4)
	assert.Equal(t, strconv.Itoa(int(childSpanID)), dst["x-datadog-parent-id"])
	assert.Equal(t, "1", dst["x-datadog-trace-id"])
	assert.Equal(t, "1", dst["x-datadog-sampling-priority"])
	assertTraceTags(t, "hello=world=,_dd.p.dm=934086a6-4", dst["x-datadog-tags"])
}

func TestTextMapPropagatorTraceTagsWithoutPriority(t *testing.T) {
	t.Setenv(headerPropagationStyleExtract, "datadog")
	t.Setenv(headerPropagationStyleInject, "datadog")
	src := TextMapCarrier(map[string]string{
		DefaultTraceIDHeader:  "1",
		DefaultParentIDHeader: "1",
		traceTagsHeader:       "hello=world,_dd.p.dm=934086a6-4",
	})
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(t, err)
	ctx, err := tracer.Extract(src)
	assert.Nil(t, err)
	child := tracer.StartSpan("test", ChildOf(ctx))
	childSpanID := child.Context().spanID
	assert.Equal(t, map[string]string{
		"hello":    "world",
		"_dd.p.dm": "-1",
	}, ctx.trace.propagatingTags)
	dst := map[string]string{}
	err = tracer.Inject(child.Context(), TextMapCarrier(dst))
	assert.Nil(t, err)
	assert.Len(t, dst, 4)
	assert.Equal(t, strconv.Itoa(int(childSpanID)), dst["x-datadog-parent-id"])
	assert.Equal(t, "1", dst["x-datadog-trace-id"])
	assert.Equal(t, "1", dst["x-datadog-sampling-priority"])
	assertTraceTags(t, "hello=world,_dd.p.dm=-1", dst["x-datadog-tags"])
}

func TestExtractOriginSynthetics(t *testing.T) {
	t.Setenv(headerPropagationStyleExtract, "datadog")
	src := TextMapCarrier(map[string]string{
		originHeader:          "synthetics",
		DefaultTraceIDHeader:  "3",
		DefaultParentIDHeader: "0",
	})
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(t, err)
	ctx, err := tracer.Extract(src)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, ctx.spanID, uint64(0))
	assert.Equal(t, ctx.traceID.Lower(), uint64(3))
	assert.Equal(t, ctx.origin, "synthetics")
}

func Test257CharacterDDTracestateLengh(t *testing.T) {
	t.Setenv(headerPropagationStyle, "tracecontext")

	tracer, err := newTracer()
	require.NoError(t, err)
	defer tracer.Stop()
	assert := assert.New(t)
	root := tracer.StartSpan("web.request")
	root.SetTag(ext.ManualKeep, true)
	ctx := root.Context()
	ctx.origin = "rum"
	ctx.traceID = traceIDFrom64Bits(1)
	ctx.spanID = 2
	ctx.trace.propagatingTags = map[string]string{
		"tracestate": "valid_vendor=a:1",
	}
	// need to create a tracestate where the dd portion will be 257 chars long
	// we currently have:
	// 3 chars ->  dd=
	// 4 chars ->  s:2;
	// 6 chars ->  o:rum;
	// 13 in total - so 244 characters left
	// shortest propagated key/val is `t.a:0` 5 chars
	// plus 1 for the `;` between tags
	// so 19 including a propagated tag, leaving 238 chars to hit 257
	// acount for the t._:0 characters, leaves us with 234 characters for the key
	// this will give us a tracestate 257 characters long
	// note that there is no ending `;`
	longKey := strings.Repeat("a", 234) // 234 is correct num for 257
	shortKey := "a"

	ctx.trace.propagatingTags[fmt.Sprintf("_dd.p.%s", shortKey)] = "0"
	ctx.trace.propagatingTags[fmt.Sprintf("_dd.p.%s", longKey)] = "0"

	headers := TextMapCarrier(map[string]string{})
	err = tracer.Inject(ctx, headers)
	assert.Nil(err)
	assert.Contains(headers[tracestateHeader], "valid_vendor=a:1")
	// iterating through propagatingTags map doesn't guarantee order in tracestate header
	ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
	assert.Contains(ddTag, "s:2")
	assert.Regexp(regexp.MustCompile(`dd=[\w:,]+`), ddTag)
	assert.LessOrEqual(len(ddTag), 256) // one of the propagated tags will not be propagated
}

func TestTextMapPropagator(t *testing.T) {
	bigMap := make(map[string]string)
	for i := 0; i < 100; i++ {
		bigMap[fmt.Sprintf("someKey%d", i)] = fmt.Sprintf("someValue%d", i)
	}
	tests := []struct {
		name, injectStyle          string
		tags                       map[string]string
		xDatadogTagsHeader, errStr string
	}{
		{
			name:        "InjectTooManyTags",
			injectStyle: "datadog",
			tags:        bigMap,
			errStr:      "inject_max_size",
		}, {
			name:               "InvalidComma",
			injectStyle:        "datadog",
			tags:               map[string]string{"_dd.p.hello1": "world", "_dd.p.hello2": "malformed,"},
			xDatadogTagsHeader: "_dd.p.dm=-1,_dd.p.hello1=world",
			errStr:             "encoding_error",
		}, {
			name:               "InvalidChar",
			injectStyle:        "datadog",
			tags:               map[string]string{"_dd.p.hello": "ÜwÜ"},
			xDatadogTagsHeader: "_dd.p.dm=-1",
			errStr:             "encoding_error",
		}, {
			name:               "Tracestate-Datadog",
			injectStyle:        "datadog",
			tags:               map[string]string{"_dd.p.hello1": "world", tracestateHeader: "shouldbe=ignored"},
			xDatadogTagsHeader: "_dd.p.dm=-1,_dd.p.hello1=world",
		}, {
			name:               "Traceparent-Datadog",
			injectStyle:        "datadog",
			tags:               map[string]string{"_dd.p.hello1": "world", traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01"},
			xDatadogTagsHeader: "_dd.p.dm=-1,_dd.p.hello1=world",
		}, {
			name:               "Tracestate-Datadog",
			injectStyle:        "datadog,tracecontext",
			tags:               map[string]string{"_dd.p.hello1": "world", tracestateHeader: "shouldbe=kept"},
			xDatadogTagsHeader: "_dd.p.dm=-1,_dd.p.hello1=world",
		},
	}
	for _, tc := range tests {
		t.Run("Inject-"+tc.name, func(t *testing.T) {
			t.Setenv(headerPropagationStyleInject, tc.injectStyle)
			tracer, err := newTracer()
			defer tracer.Stop()
			assert.NoError(t, err)
			setGlobalTracer(tracer)
			child := tracer.StartSpan("test")
			for k, v := range tc.tags {
				child.Context().trace.setPropagatingTag(k, v)
			}
			childSpanID := child.Context().spanID
			dst := map[string]string{}
			err = tracer.Inject(child.Context(), TextMapCarrier(dst))
			assert.Nil(t, err)
			ddHeadersLen := 3 // x-datadog-parent-id, x-datadog-trace-id, x-datadog-sampling-priority
			if tc.xDatadogTagsHeader != "" {
				ddHeadersLen++ // x-datadog-tags
			}
			if strings.Contains(tc.injectStyle, "tracecontext") {
				ddHeadersLen += 2 // tracestate, traceparent
			}
			assert.Len(t, dst, ddHeadersLen) // ensure that no extra headers exist that shouldn't
			assert.Equal(t, strconv.Itoa(int(childSpanID)), dst["x-datadog-parent-id"])
			assert.Equal(t, strconv.Itoa(int(childSpanID)), dst["x-datadog-trace-id"])
			assert.Equal(t, "1", dst["x-datadog-sampling-priority"])
			if tc.xDatadogTagsHeader != "" {
				tc.xDatadogTagsHeader += fmt.Sprintf(",_dd.p.tid=%s", child.Context().TraceID()[:16])
			}
			assertTraceTags(t, tc.xDatadogTagsHeader, dst["x-datadog-tags"])
			if strings.Contains(tc.injectStyle, "tracecontext") {
				// other unit tests check the value of these W3C headers, so just make sure they're present
				assert.NotEmpty(t, dst[tracestateHeader])
				assert.NotEmpty(t, dst[traceparentHeader])
			}
			assert.Equal(t, tc.errStr, child.Context().trace.tags["_dd.propagation_error"])
		})
	}
	t.Run("Extract-InvalidTraceTagsHeader", func(t *testing.T) {
		t.Setenv(headerPropagationStyleExtract, "datadog")
		src := TextMapCarrier(map[string]string{
			DefaultTraceIDHeader:  "1",
			DefaultParentIDHeader: "1",
			traceTagsHeader:       "hello=world,=", // invalid value
		})
		tracer, err := newTracer()
		defer tracer.Stop()
		assert.NoError(t, err)
		ctx, err := tracer.Extract(src)
		assert.Nil(t, err)
		assert.Equal(t, "decoding_error", ctx.trace.tags["_dd.propagation_error"])
	})

	t.Run("Extract-TooManyTags", func(t *testing.T) {
		t.Setenv(headerPropagationStyleExtract, "datadog")
		src := TextMapCarrier(map[string]string{
			DefaultTraceIDHeader:  "1",
			DefaultParentIDHeader: "1",
			traceTagsHeader:       fmt.Sprintf("%s", bigMap),
		})
		tracer, err := newTracer()
		defer tracer.Stop()
		assert.NoError(t, err)
		ctx, err := tracer.Extract(src)
		assert.Nil(t, err)
		assert.Equal(t, "extract_max_size", ctx.trace.tags["_dd.propagation_error"])
	})

	t.Run("InjectExtract", func(t *testing.T) {
		t.Setenv("DD_TRACE_128_BIT_TRACEID_GENERATION_ENABLED", "true")
		t.Setenv(headerPropagationStyleExtract, "datadog")
		t.Setenv(headerPropagationStyleInject, "datadog")
		propagator := NewPropagator(&PropagatorConfig{
			BaggagePrefix:    "bg-",
			TraceHeader:      "tid",
			ParentHeader:     "pid",
			MaxTagsHeaderLen: defaultMaxTagsHeaderLen,
		})
		tracer, err := newTracer(WithPropagator(propagator))
		defer tracer.Stop()
		assert.NoError(t, err)
		root := tracer.StartSpan("web.request")
		root.SetTag(ext.ManualDrop, true)
		root.SetBaggageItem("item", "x")
		ctx := root.Context()
		headers := TextMapCarrier(map[string]string{})
		err = tracer.Inject(ctx, headers)

		assert := assert.New(t)
		assert.Nil(err)

		xctx, err := tracer.Extract(headers)
		assert.Nil(err)
		assert.Equal(xctx.traceID.HexEncoded(), ctx.traceID.HexEncoded())
		assert.Equal(xctx.spanID, ctx.spanID)
		assert.Equal(xctx.baggage, ctx.baggage)
		assert.Equal(xctx.trace.priority, ctx.trace.priority)
	})
}

func TestEnvVars(t *testing.T) {
	var testEnvs []map[string]string

	s, c := httpmem.ServerAndClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer s.Close()

	t.Run("b3/b3multi inject", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleInject: "b3"},
			{headerPropagationStyle: "b3"},
			{otelHeaderPropagationStyle: "b3multi"},
			{headerPropagationStyleInject: "b3multi", headerPropagationStyle: "none" /* none should have no affect */},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				tid    traceID
				spanID uint64
				out    map[string]string
			}{
				{
					tid:    traceIDFrom128Bits(9863134987902842, 1412508178991881),
					spanID: 1842642739201064,
					out: map[string]string{
						b3TraceIDHeader: "00230a7811535f7a000504ab30404b09",
						b3SpanIDHeader:  "00068bdfb1eb0428",
					},
				},
				{
					tid:    traceIDFrom64Bits(1412508178991881),
					spanID: 1842642739201064,
					out: map[string]string{
						b3TraceIDHeader: "000504ab30404b09",
						b3SpanIDHeader:  "00068bdfb1eb0428",
					},
				},
				{
					tid:    traceIDFrom64Bits(9530669991610245),
					spanID: 9455715668862222,
					out: map[string]string{
						b3TraceIDHeader: "0021dc1807524785",
						b3SpanIDHeader:  "002197ec5d8a250e",
					},
				},
				{
					tid:    traceIDFrom128Bits(1, 1),
					spanID: 1,
					out: map[string]string{
						b3TraceIDHeader: "00000000000000010000000000000001",
						b3SpanIDHeader:  "0000000000000001",
					},
				},
			}
			for _, test := range tests {
				t.Run(fmt.Sprintf("inject with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert.NoError(t, err)
					root := tracer.StartSpan("web.request")
					ctx := root.Context()
					ctx.traceID = test.tid
					ctx.spanID = test.spanID
					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)

					assert := assert.New(t)
					assert.Nil(err)
					assert.Equal(test.out[b3TraceIDHeader], headers[b3TraceIDHeader])
					assert.Equal(test.out[b3SpanIDHeader], headers[b3SpanIDHeader])
				})
			}
		}
	})

	t.Run("b3/b3multi extract", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "b3"},
			{headerPropagationStyle: "b3,none" /* none should have no affect */},
			{otelHeaderPropagationStyle: "b3multi"},
			{headerPropagationStyleExtract: "b3multi", headerPropagationStyle: "none" /* none should have no affect */},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in  TextMapCarrier
				tid traceID
				sid uint64
			}{
				{
					TextMapCarrier{
						b3TraceIDHeader: "1",
						b3SpanIDHeader:  "1",
					},
					traceIDFrom64Bits(1),
					1,
				},
				{
					TextMapCarrier{
						b3TraceIDHeader: "20000000000000001",
						b3SpanIDHeader:  "1",
					},
					traceIDFrom128Bits(2, 1),
					1,
				},
				{
					TextMapCarrier{
						b3TraceIDHeader: "feeb0599801f4700",
						b3SpanIDHeader:  "f8f5c76089ad8da5",
					},
					traceIDFrom64Bits(18368781661998368512),
					17939463908140879269,
				},
				{
					TextMapCarrier{
						b3TraceIDHeader: "feeb0599801f4700a21ba1551789e3f5",
						b3SpanIDHeader:  "a1eb5bf36e56e50e",
					},
					traceIDFrom128Bits(18368781661998368512, 11681107445354718197),
					11667520360719770894,
				},
			}
			for _, test := range tests {
				t.Run(fmt.Sprintf("extract with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					ctx, err := tracer.Extract(test.in)
					assert.Nil(err)
					assert.Equal(test.tid, ctx.traceID)
					assert.Equal(test.sid, ctx.spanID)
				})
			}
		}
	})

	t.Run("b3/b3multi extract invalid", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "b3"},
			{headerPropagationStyle: "b3,none" /* none should have no affect */},
			{otelHeaderPropagationStyle: "b3multi"},
			{headerPropagationStyleExtract: "b3multi", headerPropagationStyle: "none" /* none should have no affect */},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in TextMapCarrier
			}{
				{
					TextMapCarrier{
						b3TraceIDHeader: "0",
						b3SpanIDHeader:  "0",
					},
				},
			}
			for _, tc := range tests {
				t.Run(fmt.Sprintf("extract with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					_, err = tracer.Extract(tc.in)
					assert.NotNil(err)
				})
			}
		}
	})

	t.Run("b3 single header extract", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "B3 single header"},
			{headerPropagationStyle: "B3 single header,none" /* none should have no affect */},
			{otelHeaderPropagationStyle: "b3"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in         TextMapCarrier
				traceID128 string
				out        []uint64 // contains [<trace_id>, <span_id>, <sampling_decision>]
			}{
				{
					TextMapCarrier{
						b3SingleHeader: "1-2",
					},
					"",
					[]uint64{1, 2},
				},
				{
					TextMapCarrier{
						b3SingleHeader: "feeb0599801f4700-f8f5c76089ad8da5-1",
					},
					"",
					[]uint64{18368781661998368512, 17939463908140879269, 1},
				},
				{
					TextMapCarrier{
						b3SingleHeader: "6e96719ded9c1864a21ba1551789e3f5-a1eb5bf36e56e50e-0",
					},
					"",
					[]uint64{11681107445354718197, 11667520360719770894, 0},
				},
				{
					TextMapCarrier{
						b3SingleHeader: "6e96719ded9c1864a21ba1551789e3f5-a1eb5bf36e56e50e-d",
					},
					"",
					[]uint64{11681107445354718197, 11667520360719770894, 1},
				},
			}
			for _, tc := range tests {
				t.Run(fmt.Sprintf("extract with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					ctx, err := tracer.Extract(tc.in)
					require.Nil(t, err)

					assert.Equal(tc.out[0], ctx.traceID.Lower())
					assert.Equal(tc.out[1], ctx.spanID)
					// assert.Equal(tc.traceID128, id128FromSpan(assert, ctx)) // add when 128-bit trace id support is enabled
					if len(tc.out) > 2 {
						require.NotNil(t, ctx.trace)
						assert.Equal(float64(tc.out[2]), *ctx.trace.priority)
					}
				})
			}
		}
	})

	t.Run("b3 single header inject", func(t *testing.T) {
		t.Setenv(headerPropagationStyleInject, "b3 single header")
		var tests = []struct {
			in  []uint64 // contains [<trace_id_lower_bits>, <span_id>, <sampling_decision>]
			out string
		}{
			{
				[]uint64{18368781661998368512, 17939463908140879269, 1},
				"feeb0599801f4700-f8f5c76089ad8da5-1",
			},
			{
				[]uint64{11681107445354718197, 11667520360719770894, 0},
				"a21ba1551789e3f5-a1eb5bf36e56e50e-0",
			},
		}
		for i, tc := range tests {
			t.Run(fmt.Sprintf("b3 single header inject #%d", i), func(t *testing.T) {
				tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
				defer tracer.Stop()
				assert.NoError(t, err)
				root := tracer.StartSpan("myrequest")
				ctx := root.Context()
				ctx.traceID = traceIDFrom64Bits(tc.in[0])
				ctx.spanID = tc.in[1]
				ctx.setSamplingPriority(int(tc.in[2]), samplernames.Unknown)
				headers := TextMapCarrier(map[string]string{})
				err = tracer.Inject(ctx, headers)
				require.Nil(t, err)
				assert.Equal(t, tc.out, headers[b3SingleHeader])
			})
		}
	})

	t.Run("datadog inject", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleInject: "datadog"},
			{headerPropagationStyle: "datadog"},
			{otelHeaderPropagationStyle: "datadog"},
			{headerPropagationStyleInject: "datadog", headerPropagationStyle: "none" /* none should have no affect */},
		}

		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in  []uint64 // contains [<trace_id_lower_bits>, <span_id>]
				out map[string]string
			}{
				{
					[]uint64{1412508178991881, 1842642739201064},
					map[string]string{
						b3TraceIDHeader: "000504ab30404b09",
						b3SpanIDHeader:  "00068bdfb1eb0428",
					},
				},
				{
					[]uint64{9530669991610245, 9455715668862222},
					map[string]string{
						b3TraceIDHeader: "0021dc1807524785",
						b3SpanIDHeader:  "002197ec5d8a250e",
					},
				},
				{
					[]uint64{1, 1},
					map[string]string{
						b3TraceIDHeader: "0000000000000001",
						b3SpanIDHeader:  "0000000000000001",
					},
				},
			}
			for _, tc := range tests {
				t.Run(fmt.Sprintf("inject with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithPropagator(NewPropagator(&PropagatorConfig{B3: true})), WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					assert.NoError(t, err)
					defer tracer.Stop()
					root := tracer.StartSpan("web.request")
					ctx := root.Context()
					ctx.traceID = traceIDFrom64Bits(tc.in[0])
					ctx.spanID = tc.in[1]
					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)

					assert := assert.New(t)
					assert.Nil(err)
					assert.Equal(tc.out[b3TraceIDHeader], headers[b3TraceIDHeader])
					assert.Equal(tc.out[b3SpanIDHeader], headers[b3SpanIDHeader])
				})
			}
		}
	})

	t.Run("datadog/b3 extract", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "Datadog,b3"},
			{headerPropagationStyle: "Datadog,b3"},
			{headerPropagationStyle: "none,Datadog,b3" /* none should have no affect */},
			{otelHeaderPropagationStyle: "Datadog,b3multi"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in             TextMapCarrier
				traceID128Full string
				out            []uint64 // contains [<trace_id>, <span_id>, <sampling_decision>]
			}{
				{
					TextMapCarrier{
						b3TraceIDHeader: "1",
						b3SpanIDHeader:  "1",
						b3SampledHeader: "1",
					},
					"",
					[]uint64{1, 1, 1},
				},
				{
					TextMapCarrier{
						b3TraceIDHeader: "20000000000000001",
						b3SpanIDHeader:  "1",
						b3SampledHeader: "2",
					},
					"0000000000000002",
					[]uint64{1, 1, 2},
				},
				{
					TextMapCarrier{
						b3TraceIDHeader: "feeb0599801f4700",
						b3SpanIDHeader:  "f8f5c76089ad8da5",
						b3SampledHeader: "1",
					},
					"",
					[]uint64{18368781661998368512, 17939463908140879269, 1},
				},
				{
					TextMapCarrier{
						b3TraceIDHeader: "feeb0599801f4700a21ba1551789e3f5",
						b3SpanIDHeader:  "a1eb5bf36e56e50e",
						b3SampledHeader: "0",
					},
					"feeb0599801f4700",
					[]uint64{11681107445354718197, 11667520360719770894, 0},
				},
			}
			for _, tc := range tests {
				t.Run(fmt.Sprintf("extract with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)

					ctx, err := tracer.Extract(tc.in)
					assert.Nil(err)

					// assert.Equal(tc.traceID128Full, id128FromSpan(assert, ctx)) // add when 128-bit trace id support is enabled
					assert.Equal(tc.out[0], ctx.traceID.Lower())
					assert.Equal(tc.out[1], ctx.spanID)
					p, ok := ctx.SamplingPriority()
					assert.True(ok)
					assert.Equal(int(tc.out[2]), p)
				})
			}
		}
	})

	t.Run("datadog inject/extract", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleInject: "datadog", headerPropagationStyleExtract: "datadog"},
			{headerPropagationStyleInject: "datadog", headerPropagationStyle: "datadog"},
			{headerPropagationStyle: "datadog"},
			{otelHeaderPropagationStyle: "datadog"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in  []uint64 // contains [<trace_id_lower_bits>, <span_id>]
				out map[string]string
			}{
				{
					[]uint64{1412508178991881, 1842642739201064},
					map[string]string{
						b3TraceIDHeader: "000504ab30404b09",
						b3SpanIDHeader:  "00068bdfb1eb0428",
					},
				},
				{
					[]uint64{9530669991610245, 9455715668862222},
					map[string]string{
						b3TraceIDHeader: "0021dc1807524785",
						b3SpanIDHeader:  "002197ec5d8a250e",
					},
				},
				{
					[]uint64{1, 1},
					map[string]string{
						b3TraceIDHeader: "0000000000000001",
						b3SpanIDHeader:  "0000000000000001",
					},
				},
			}
			for _, tc := range tests {
				t.Run(fmt.Sprintf("inject and extract with env=%q", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert.NoError(t, err)
					root := tracer.StartSpan("web.request")
					root.SetTag(ext.ManualDrop, true)
					root.SetBaggageItem("item", "x")
					ctx := root.Context()
					ctx.traceID = traceIDFrom64Bits(tc.in[0])
					ctx.spanID = tc.in[1]
					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)

					assert := assert.New(t)
					assert.Nil(err)

					xctx, err := tracer.Extract(headers)
					require.Nil(t, err)

					assert.Equal(ctx.traceID, xctx.traceID)
					assert.Equal(ctx.spanID, xctx.spanID)
					assert.Equal(ctx.baggage, xctx.baggage)
					assert.Equal(ctx.trace.priority, xctx.trace.priority)
				})
			}
		}
	})

	t.Run("w3c extract", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "traceContext"},
			{headerPropagationStyle: "traceContext"},
			{otelHeaderPropagationStyle: "traceContext"},
			{headerPropagationStyleExtract: "traceContext", headerPropagationStyle: "none" /* none should have no affect */},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in              TextMapCarrier
				out             []uint64 // contains [<span_id>, <sampling_decision>]
				tid             traceID
				origin          string
				propagatingTags map[string]string
			}{
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						// no tracestate header, shouldn't put an empty tracestate in propagatingTags
					},
					tid:             traceIDFrom64Bits(1229782938247303441),
					out:             []uint64{2459565876494606882, 1},
					origin:          "",
					propagatingTags: *(new(map[string]string)),
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					tid:    traceIDFrom64Bits(1229782938247303441),
					out:    []uint64{2459565876494606882, 2},
					origin: "rum",
					propagatingTags: map[string]string{
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-10000000000000000000000000000000-2222222222222222-01",
						tracestateHeader:  "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					out:    []uint64{2459565876494606882, 2},
					tid:    traceIDFrom128Bits(1152921504606846976, 0),
					origin: "rum",
					propagatingTags: map[string]string{
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-03",
						tracestateHeader:  "dd=s:0;o:rum;t.dm:-2;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					out:    []uint64{2459565876494606882, 1},
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "rum",
					propagatingTags: map[string]string{
						"_dd.p.dm":     "-0",
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:0;o:rum;t.dm:-2;t.usr.id:baz64~~,othervendor=t61rcWkgMzE"},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-00",
						tracestateHeader:  "dd=s:1;o:rum;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					out:    []uint64{2459565876494606882, 0},
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "rum",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:1;o:rum;t.usr.id:baz64~~,othervendor=t61rcWkgMzE"},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-00",
						tracestateHeader:  "dd=s:1;o:rum;t.dm:-2;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					out:    []uint64{2459565876494606882, 0},
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "rum",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:1;o:rum;t.dm:-2;t.usr.id:baz64~~,othervendor=t61rcWkgMzE"},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:2;o:rum:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					out:    []uint64{2459565876494606882, 2}, // tracestate priority takes precedence
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "rum:rum",
					propagatingTags: map[string]string{
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:2;o:rum:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:;o:rum:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					out:    []uint64{2459565876494606882, 1}, // tracestate priority takes precedence
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "rum:rum",
					propagatingTags: map[string]string{
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:;o:rum:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: " \t-00-00000000000000001111111111111111-2222222222222222-01 \t-",
						tracestateHeader:  "othervendor=t61rcWkgMzE,dd=o:rum:rum;s:;t.dm:-4;t.usr.id:baz64~~",
					},
					out: []uint64{2459565876494606882, 1}, // tracestate priority takes precedence
					tid: traceIDFrom64Bits(1229782938247303441),

					origin: "rum:rum",
					propagatingTags: map[string]string{
						"tracestate":   "othervendor=t61rcWkgMzE,dd=o:rum:rum;s:;t.dm:-4;t.usr.id:baz64~~",
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "othervendor=t61rcWkgMzE,dd=o:2;s:fake_origin;t.dm:-4;t.usr.id:baz64~~,",
					},
					out:    []uint64{2459565876494606882, 1}, // tracestate priority takes precedence
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "2",
					propagatingTags: map[string]string{
						"tracestate":   "othervendor=t61rcWkgMzE,dd=o:2;s:fake_origin;t.dm:-4;t.usr.id:baz64~~,",
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "othervendor=t61rcWkgMzE,dd=o:~_~;s:fake_origin;t.dm:-4;t.usr.id:baz64~~,",
					},
					out:    []uint64{2459565876494606882, 1}, // tracestate priority takes precedence
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "=_=",
					propagatingTags: map[string]string{
						"tracestate":   "othervendor=t61rcWkgMzE,dd=o:~_~;s:fake_origin;t.dm:-4;t.usr.id:baz64~~,",
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
					},
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "cc-00000000000000001111111111111111-2222222222222222-01-what-the-future-will-be-like",
						tracestateHeader:  "othervendor=t61rcWkgMzE,dd=o:~_~;s:fake_origin;t.dm:-4;t.usr.id:baz64~~,",
					},
					out:    []uint64{2459565876494606882, 1}, // tracestate priority takes precedence
					tid:    traceIDFrom64Bits(1229782938247303441),
					origin: "=_=",
					propagatingTags: map[string]string{
						"tracestate":   "othervendor=t61rcWkgMzE,dd=o:~_~;s:fake_origin;t.dm:-4;t.usr.id:baz64~~,",
						"_dd.p.dm":     "-4",
						"_dd.p.usr.id": "baz64==",
					},
				},
			}
			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%v extract/valid  with env=%q", i, testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					ctx, err := tracer.Extract(tc.in)
					if err != nil {
						t.Fatal(err)
					}

					assert.Equal(tc.tid, ctx.traceID)
					assert.Equal(tc.out[0], ctx.spanID)
					assert.Equal(tc.origin, ctx.origin)
					p, ok := ctx.SamplingPriority()
					assert.True(ok)
					assert.Equal(int(tc.out[1]), p)

					assert.Equal(tc.propagatingTags, ctx.trace.propagatingTags)
				})
			}
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []TextMapCarrier{
				{tracestateHeader: "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE"},
				{traceparentHeader: "00-.2345678901234567890123456789012-1234567890123456-01"}, // invalid length
				{traceparentHeader: "00-1234567890123456789012345678901.-1234567890123456-01"}, // invalid length
				{traceparentHeader: "00-00000000000000001111111111111111-0000000000000000-01"}, // invalid length
				{traceparentHeader: "00-00000000000000000000000000000000-0001000000000000-01"}, // invalid length
				{traceparentHeader: "00-0000000000000.000000000000000000-0001000000000000-01"}, // invalid length
				{traceparentHeader: "00-1234567890123---ffffffffffffffff--fffffffffffffff-01"}, // invalid length
				{traceparentHeader: "00-_234567890123---ffffffffffffffff--fffffffffffffff-01"}, // invalid length
				{traceparentHeader: "00-12345678901234567890123456789011-1234567890123456-0."}, // invalid length
				{traceparentHeader: "00--2345678901234567890123456789011-1234567890123456-00"}, // invalid length
				{traceparentHeader: "00-2345678-901234567890123456789011-1234567890123456-00"}, // invalid length
				{traceparentHeader: "------------------------------------1234567890123456---"}, // invalid length
				{traceparentHeader: "0"},       // invalid length
				{traceparentHeader: "\t- -\t"}, // invalid length
				{
					traceparentHeader: "00-000000000000000011111111111121111-2222222222222222-01", // invalid length
					tracestateHeader:  "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
				},
				{
					traceparentHeader: "100-00000000000000001111111111111111-2222222222222222-01", // invalid length
					tracestateHeader:  "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
				},
				{
					traceparentHeader: "ff-00000000000000001111111111111111-2222222222222222-01", // invalid version
					tracestateHeader:  "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
				},
			}

			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%v extract/invalid  with env=%q", i, testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					ctx, err := tracer.Extract(tc)
					assert.NotNil(err)
					assert.Nil(ctx)
				})
			}
		}
	})

	t.Run("w3c extract / w3c,datadog inject", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "traceContext"},
			{headerPropagationStyle: "traceContext"},
			{otelHeaderPropagationStyle: "traceContext"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				inHeaders  TextMapCarrier
				outHeaders TextMapCarrier
				sid        uint64
				tid        traceID
				priority   int
				traceID128 string
				origin     string
			}{
				{
					inHeaders: TextMapCarrier{
						traceparentHeader: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00",
						tracestateHeader:  "foo=1,dd=s:-1;p:00f067aa0ba902b7",
					},
					outHeaders: TextMapCarrier{
						traceparentHeader:     "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00",
						tracestateHeader:      "dd=s:-1;o:synthetics;p:00f067aa0ba902b7;t.tid:4bf92f3577b34da6,foo=1",
						DefaultPriorityHeader: "-1",
						DefaultTraceIDHeader:  "4bf92f3577b34da6a3ce929d0e0e4736",
						DefaultParentIDHeader: "00f067aa0ba902b7",
					},
					sid:        67667974448284343,
					tid:        traceIDFrom128Bits(5474458728733560230, 11803532876627986230),
					priority:   -1,
					traceID128: "4bf92f3577b34da6",
					origin:     "synthetics",
				},
			}
			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%v extract/valid  with env=%q", i, testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					assert.NoError(t, err)
					defer tracer.Stop()
					assert := assert.New(t)
					ctx, err := tracer.Extract(tc.inHeaders)
					if err != nil {
						t.Fatal(err)
					}
					root := tracer.StartSpan("web.request", ChildOf(ctx))
					defer root.Finish()
					ctx.origin = tc.origin

					assert.Equal(tc.tid, ctx.traceID)
					assert.Equal(tc.sid, ctx.spanID)
					p, ok := ctx.SamplingPriority()
					assert.True(ok)
					assert.Equal(tc.priority, p)

					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)

					assert.True(ok)
					assert.Nil(err)
					checkSameElements(assert, tc.outHeaders[traceparentHeader], headers[traceparentHeader])
					checkSameElements(assert, tc.outHeaders[tracestateHeader], headers[tracestateHeader])
					ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
					assert.LessOrEqual(len(ddTag), 256)
				})
			}
		}
	})

	t.Run("w3c inject", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleInject: "tracecontext", headerPropagationStyleExtract: "tracecontext"},
			{headerPropagationStyleInject: "datadog,tracecontext", headerPropagationStyleExtract: "datadog,tracecontext"},
			{headerPropagationStyleInject: "datadog,tracecontext", headerPropagationStyle: "datadog,tracecontext"},
			{headerPropagationStyle: "datadog,tracecontext"},
			{otelHeaderPropagationStyle: "datadog,traceContext"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				tid             traceID
				sid             uint64
				out             TextMapCarrier
				priority        int
				origin          string
				lastParent      string
				propagatingTags map[string]string
			}{
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:2;o:rum;p:2222222222222222;t.usr.id: baz64 ~~,othervendor=t61rcWkgMzE",
					},
					tid:        traceIDFrom64Bits(1229782938247303441),
					sid:        2459565876494606882,
					priority:   2,
					origin:     "rum",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": " baz64 ==",
						"tracestate":   "othervendor=t61rcWkgMzE,dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:1;o:rum;p:2222222222222222;t.usr.id:baz64~~",
					},
					tid:        traceIDFrom64Bits(1229782938247303441),
					sid:        2459565876494606882,
					priority:   1,
					origin:     "rum",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz64==",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-12300000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:2;o:rum:rum;p:2222222222222222;t.tid:1230000000000000;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					tid:        traceIDFrom128Bits(1310547491564814336, 1229782938247303441),
					sid:        2459565876494606882,
					priority:   2, // tracestate priority takes precedence
					origin:     "rum:rum",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:2;o:rum_rum;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
						tracestateHeader:  "dd=s:1;o:rum:rum;p:2222222222222222;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
					tid:        traceIDFrom64Bits(1229782938247303441),
					sid:        2459565876494606882,
					priority:   1, // traceparent priority takes precedence
					origin:     "rum:rum",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz64==",
						"tracestate":   "dd=s:1;o:rum:rum;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-00",
						tracestateHeader:  "dd=s:-1;o:rum:rum;p:2222222222222222;t.usr.id:baz:64~~,othervendor=t61rcWkgMzE",
					},
					tid:        traceIDFrom64Bits(1229782938247303441),
					sid:        2459565876494606882,
					priority:   -1, // traceparent priority takes precedence
					origin:     "rum:rum",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz:64==",
						"tracestate":   "dd=s:1;o:rum:rum;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111112-2222222222222222-00",
						tracestateHeader:  "dd=s:0;o:old_tracestate;p:2222222222222222;t.usr.id:baz:64~~ ,a0=a:1,a1=a:1,a2=a:1,a3=a:1,a4=a:1,a5=a:1,a6=a:1,a7=a:1,a8=a:1,a9=a:1,a10=a:1,a11=a:1,a12=a:1,a13=a:1,a14=a:1,a15=a:1,a16=a:1,a17=a:1,a18=a:1,a19=a:1,a20=a:1,a21=a:1,a22=a:1,a23=a:1,a24=a:1,a25=a:1,a26=a:1,a27=a:1,a28=a:1,a29=a:1,a30=a:1",
					},
					tid:        traceIDFrom64Bits(1229782938247303442),
					sid:        2459565876494606882,
					origin:     "old_tracestate",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz:64== ",
						"tracestate":   "dd=o:very_long_origin_tag,a0=a:1,a1=a:1,a2=a:1,a3=a:1,a4=a:1,a5=a:1,a6=a:1,a7=a:1,a8=a:1,a9=a:1,a10=a:1,a11=a:1,a12=a:1,a13=a:1,a14=a:1,a15=a:1,a16=a:1,a17=a:1,a18=a:1,a19=a:1,a20=a:1,a21=a:1,a22=a:1,a23=a:1,a24=a:1,a25=a:1,a26=a:1,a27=a:1,a28=a:1,a29=a:1,a30=a:1,a31=a:1,a32=a:1",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111112-2222222222222222-00",
						tracestateHeader:  "dd=s:0;o:old_tracestate;p:2222222222222222;t.usr.id:baz:64~~,a0=a:1,a1=a:1,a2=a:1,a3=a:1,a4=a:1,a5=a:1,a6=a:1,a7=a:1,a8=a:1,a9=a:1,a10=a:1,a11=a:1,a12=a:1,a13=a:1,a14=a:1,a15=a:1,a16=a:1,a17=a:1,a18=a:1,a19=a:1,a20=a:1,a21=a:1,a22=a:1,a23=a:1,a24=a:1,a25=a:1,a26=a:1,a27=a:1,a28=a:1,a29=a:1,a30=a:1",
					},
					tid:        traceIDFrom64Bits(1229782938247303442),
					sid:        2459565876494606882,
					origin:     "old_tracestate",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz:64==",
						"tracestate":   "dd=o:very_long_origin_tag,a0=a:1,a1=a:1,a2=a:1,a3=a:1,a4=a:1,a5=a:1,a6=a:1,a7=a:1,a8=a:1,a9=a:1,a10=a:1,a11=a:1,a12=a:1,a13=a:1,a14=a:1,a15=a:1,a16=a:1,a17=a:1,a18=a:1,a19=a:1,a20=a:1,a21=a:1,a22=a:1,a23=a:1,a24=a:1,a25=a:1,a26=a:1,a27=a:1,a28=a:1,a29=a:1,a30=a:1,a31=a:1,a32=a:1",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111112-2222222222222222-00",
						tracestateHeader:  "dd=s:0;o:old_tracestate;p:2222222222222222;t.usr.id:baz:64~~,foo=bar",
					},
					tid:        traceIDFrom64Bits(1229782938247303442),
					sid:        2459565876494606882,
					origin:     "old_tracestate",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz:64==",
						"tracestate":   "foo=bar ",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111112-2222222222222222-00",
						tracestateHeader:  "dd=s:0;o:old_tracestate;p:2222222222222222;t.usr.id:baz:64__,foo=bar",
					},
					tid:        traceIDFrom64Bits(1229782938247303442),
					sid:        2459565876494606882,
					origin:     "old_tracestate",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz:64~~",
						"tracestate":   "\tfoo=bar\t",
					},
				},
				{
					out: TextMapCarrier{
						traceparentHeader: "00-00000000000000001111111111111112-2222222222222222-00",
						tracestateHeader:  "dd=s:0;o:~~_;p:2222222222222222;t.usr.id:baz:64__,foo=bar",
					},
					tid:        traceIDFrom64Bits(1229782938247303442),
					sid:        2459565876494606882,
					origin:     "==~",
					lastParent: "2222222222222222",
					propagatingTags: map[string]string{
						"_dd.p.usr.id": "baz:64~~",
						"tracestate":   "\tfoo=bar\t",
					},
				},
			}
			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%d w3c inject with env=%q", i, testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.Nil(err)
					root := tracer.StartSpan("web.request")
					root.setSamplingPriority(tc.priority, samplernames.Default)
					ctx := root.Context()
					ctx.origin = tc.origin
					ctx.traceID = tc.tid
					ctx.spanID = tc.sid
					ctx.trace.propagatingTags = tc.propagatingTags
					ctx.reparentID = "0123456789abcdef"
					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)

					assert.Nil(err)
					checkSameElements(assert, tc.out[traceparentHeader], headers[traceparentHeader])
					if strings.HasSuffix(tc.out[tracestateHeader], ",othervendor=t61rcWkgMzE") {
						assert.True(strings.HasSuffix(headers[tracestateHeader], ",othervendor=t61rcWkgMzE"))
						// Remove the suffixes for the following check
						headers[tracestateHeader] = strings.TrimSuffix(headers[tracestateHeader], ",othervendor=t61rcWkgMzE")
						tc.out[tracestateHeader] = strings.TrimSuffix(tc.out[tracestateHeader], ",othervendor=t61rcWkgMzE")
					}
					checkSameElements(assert, tc.out[tracestateHeader], headers[tracestateHeader])
					ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
					// -3 as we don't count dd= as part of the "value" length limit
					assert.LessOrEqual(len(ddTag)-3, 256)
				})

				t.Run(fmt.Sprintf("w3c inject with env=%q / testing tag list-member limit", testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.Nil(err)
					root := tracer.StartSpan("web.request")
					root.SetTag(ext.ManualKeep, true)
					ctx := root.Context()
					ctx.origin = "old_tracestate"
					ctx.traceID = traceIDFrom64Bits(1229782938247303442)
					ctx.spanID = 2459565876494606882
					ctx.trace.propagatingTags = map[string]string{
						"tracestate": "valid_vendor=a:1",
					}
					// dd part of the tracestate must not exceed 256 characters
					for i := 0; i < 32; i++ {
						ctx.trace.propagatingTags[fmt.Sprintf("_dd.p.a%v", i)] = "i"
					}
					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)

					assert.Nil(err)
					assert.Equal("00-00000000000000001111111111111112-2222222222222222-01", headers[traceparentHeader])
					assert.Contains(headers[tracestateHeader], "valid_vendor=a:1")
					// iterating through propagatingTags map doesn't guarantee order in tracestate header
					ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
					assert.Contains(ddTag, "s:2")
					assert.Contains(ddTag, "s:2")
					assert.Regexp(regexp.MustCompile(`dd=[\w:,]+`), ddTag)
					assert.LessOrEqual(len(ddTag), 256)
				})
			}
		}
	})

	t.Run("datadog extract / w3c,datadog inject", func(t *testing.T) {
		t.Setenv(headerPropagationStyleInject, "datadog,tracecontext")
		t.Setenv(headerPropagationStyleExtract, "datadog")
		var tests = []struct {
			outHeaders TextMapCarrier
			inHeaders  TextMapCarrier
		}{
			{
				outHeaders: TextMapCarrier{
					traceparentHeader: "00-000000000000000000000000075bcd15-000000003ade68b1-00",
					tracestateHeader:  "dd=s:-2;o:test.origin;p:000000003ade68b1",
				},
				inHeaders: TextMapCarrier{
					DefaultTraceIDHeader:  "123456789",
					DefaultParentIDHeader: "987654321",
					DefaultPriorityHeader: "-2",
					originHeader:          "test.origin",
				},
			},
			{
				outHeaders: TextMapCarrier{
					traceparentHeader: "00-000000000000000000000000075bcd15-000000003ade68b1-00",
					tracestateHeader:  "dd=s:-2;o:synthetics___web;p:000000003ade68b1",
				},
				inHeaders: TextMapCarrier{
					DefaultTraceIDHeader:  "123456789",
					DefaultParentIDHeader: "987654321",
					DefaultPriorityHeader: "-2",
					originHeader:          "synthetics;,~web",
				},
			},
		}
		for i, tc := range tests {
			t.Run(fmt.Sprintf("#%d", i), func(t *testing.T) {
				tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
				defer tracer.Stop()
				assert := assert.New(t)
				assert.NoError(err)
				ctx, err := tracer.Extract(tc.inHeaders)
				assert.Nil(err)

				root := tracer.StartSpan("web.request", ChildOf(ctx))
				defer root.Finish()
				headers := TextMapCarrier(map[string]string{})
				err = tracer.Inject(ctx, headers)

				assert.Nil(err)
				checkSameElements(assert, tc.outHeaders[traceparentHeader], headers[traceparentHeader])
				checkSameElements(assert, tc.outHeaders[tracestateHeader], headers[tracestateHeader])

				// NOTE: this will be set for phase 3
				assert.Empty(root.meta["_dd.parent_id"], "extraction happened from DD headers, so _dd.parent_id mustn't be set")

				ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
				// -3 as we don't count dd= as part of the "value" length limit
				assert.LessOrEqual(len(ddTag)-3, 256)
			})
		}
	})

	t.Run("w3c inject/extract", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleInject: "tracecontext", headerPropagationStyleExtract: "tracecontext"},
			{headerPropagationStyleInject: "datadog,tracecontext", headerPropagationStyleExtract: "datadog,tracecontext"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in         TextMapCarrier
				outMap     TextMapCarrier
				out        []uint64 // contains [<trace_id>, <span_id>]
				priority   float64
				origin     string
				lastParent string
			}{
				{
					in: TextMapCarrier{
						traceparentHeader: "00-12345678901234567890123456789012-1234567890123456-01",
						tracestateHeader:  "dd=s:2;o:rum;p:0123456789abcdef;t.tid:1234567890123456;t.usr.id:baz64~~",
					},
					outMap: TextMapCarrier{
						traceparentHeader: "00-12345678901234567890123456789012-1234567890123456-01",
						tracestateHeader:  "dd=s:2;o:rum;p:0123456789abcdef;t.tid:1234567890123456;t.usr.id:baz64~~",
					},
					out:        []uint64{8687463697196027922, 1311768467284833366},
					priority:   2,
					origin:     "rum",
					lastParent: "0123456789abcdef",
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-12345678901234567890123456789012-1234567890123456-01",
						tracestateHeader:  "foo=1",
					},
					outMap: TextMapCarrier{
						traceparentHeader: "00-12345678901234567890123456789012-1234567890123456-01",
						tracestateHeader:  "dd=s:1;t.tid:1234567890123456,foo=1",
					},
					out:        []uint64{8687463697196027922, 1311768467284833366},
					priority:   1,
					lastParent: "",
				},
			}
			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%d w3c inject/extract with env=%q", i, testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					ctx, err := tracer.Extract(tc.in)
					if err != nil {
						t.FailNow()
					}

					assert.Equal(tc.out[0], ctx.traceID.Lower())
					assert.Equal(tc.out[1], ctx.spanID)
					assert.Equal(tc.origin, ctx.origin)
					assert.Equal(tc.priority, *ctx.trace.priority)

					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(ctx, headers)
					assert.Nil(err)

					checkSameElements(assert, tc.outMap[traceparentHeader], headers[traceparentHeader])
					checkSameElements(assert, tc.outMap[tracestateHeader], headers[tracestateHeader])
					ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
					assert.LessOrEqual(len(ddTag), 256)
				})
			}
		}
	})

	t.Run("w3c extract,update span, inject", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleInject: "tracecontext", headerPropagationStyleExtract: "tracecontext"},
			{headerPropagationStyleInject: "datadog,tracecontext", headerPropagationStyleExtract: "datadog,tracecontext"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in         TextMapCarrier
				outMap     TextMapCarrier
				out        []uint64 // contains [<parent_id>, <span_id>]
				tid        traceID
				priority   float64
				origin     string
				lastParent string
			}{
				{
					in: TextMapCarrier{
						traceparentHeader: "00-12345678901234567890123456789012-1234567890123456-01",
						tracestateHeader:  "dd=s:2;p:0123456789abcdef;o:rum;t.usr.id:baz64~~",
					},
					outMap: TextMapCarrier{
						traceparentHeader: "00-12345678901234567890123456789012-0000000000000001-01",
						tracestateHeader:  "dd=s:1;o:rum;p:0000000000000001;t.usr.id:baz64~~;t.tid:1234567890123456",
					},
					out:        []uint64{1311768467284833366, 1},
					tid:        traceIDFrom128Bits(1311768467284833366, 8687463697196027922),
					priority:   1,
					lastParent: "0123456789abcdef",
				},
			}
			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%d w3c inject/extract with env=%q", i, testEnv), func(t *testing.T) {
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					defer tracer.Stop()
					assert := assert.New(t)
					assert.NoError(err)
					pCtx, err := tracer.Extract(tc.in)
					if err != nil {
						t.FailNow()
					}
					s := tracer.StartSpan("op", ChildOf(pCtx), WithSpanID(1))
					sctx := s.Context()
					// changing priority must set ctx.updated = true
					if tc.priority != 0 {
						sctx.setSamplingPriority(int(tc.priority), samplernames.Unknown)
					}

					if tc.lastParent == "" {
						assert.Empty(s.meta["_dd.parent_id"])
					} else {
						assert.Equal(s.meta["_dd.parent_id"], tc.lastParent)
					}

					assert.Equal(true, sctx.updated)

					headers := TextMapCarrier(map[string]string{})
					err = tracer.Inject(s.Context(), headers)
					assert.NoError(err)
					assert.Equal(tc.tid, sctx.traceID)
					assert.Equal(tc.out[0], sctx.span.parentID)
					assert.Equal(tc.out[1], sctx.spanID)

					checkSameElements(assert, tc.outMap[traceparentHeader], headers[traceparentHeader])
					checkSameElements(assert, tc.outMap[tracestateHeader], headers[tracestateHeader])
					ddTag := strings.SplitN(headers[tracestateHeader], ",", 2)[0]
					// -3 as we don't count dd= as part of the "value" length limit
					assert.LessOrEqual(len(ddTag)-3, 256)
				})
			}
		}
	})

	t.Run("datadog extract precedence", func(t *testing.T) {
		testEnvs = []map[string]string{
			{headerPropagationStyleExtract: "datadog,tracecontext"},
			{headerPropagationStyleExtract: "datadog,b3"},
			{headerPropagationStyleExtract: "datadog,b3multi"},
		}
		for _, testEnv := range testEnvs {
			for k, v := range testEnv {
				t.Setenv(k, v)
			}
			var tests = []struct {
				in  TextMapCarrier
				out []uint64 // contains [<span_id>, <sampling_decision>]
				tid traceID
			}{
				{
					in: TextMapCarrier{
						DefaultTraceIDHeader:  "1",
						DefaultParentIDHeader: "1",
						DefaultPriorityHeader: "1",
						traceparentHeader:     "00-00000000000000000000000000000002-0000000000000002-00",
						b3SingleHeader:        "3-3",
						b3TraceIDHeader:       "0000000000000004",
						b3SpanIDHeader:        "0000000000000004",
						b3SampledHeader:       "4",
					},
					out: []uint64{1, 1},
					tid: traceIDFrom64Bits(1),
				},
				{
					in: TextMapCarrier{
						traceparentHeader: "00-00000000000000000000000000000001-0000000000000001-01",
						b3SingleHeader:    "1-1",
						b3TraceIDHeader:   "0000000000000001",
						b3SpanIDHeader:    "0000000000000001",
						b3SampledHeader:   "1",
					},
					out: []uint64{1, 1},
					tid: traceIDFrom64Bits(1),
				},
			}
			for i, tc := range tests {
				t.Run(fmt.Sprintf("#%v extract with env=%q", i, testEnv), func(t *testing.T) {
					assert := assert.New(t)
					tracer, err := newTracer(WithHTTPClient(c), withStatsdClient(&statsd.NoOpClientDirect{}))
					assert.NoError(err)
					defer tracer.Stop()
					ctx, err := tracer.Extract(tc.in)
					if err != nil {
						t.Fatal(err)
					}

					assert.Equal(tc.tid, ctx.traceID)
					assert.Equal(tc.out[0], ctx.spanID)
					p, ok := ctx.SamplingPriority()
					assert.True(ok)
					assert.Equal(int(tc.out[1]), p)
				})
			}
		}
	})
}

func checkSameElements(assert *assert.Assertions, want, got string) {
	gotInner, wantInner := strings.TrimPrefix(got, "dd="), strings.TrimPrefix(want, "dd=")
	gotInnerList, wantInnerList := strings.Split(gotInner, ";"), strings.Split(wantInner, ";")
	assert.ElementsMatch(gotInnerList, wantInnerList)
}

func TestTraceContextPrecedence(t *testing.T) {
	t.Setenv(headerPropagationStyleExtract, "datadog,b3,tracecontext")
	tracer, err := newTracer()
	assert.NoError(t, err)
	defer tracer.Stop()
	sctx, err := tracer.Extract(TextMapCarrier{
		traceparentHeader:     "00-00000000000000000000000000000001-0000000000000001-01",
		DefaultTraceIDHeader:  "1",
		DefaultParentIDHeader: "22",
		DefaultPriorityHeader: "2",
		b3SingleHeader:        "1-333",
	})
	assert.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(traceIDFrom64Bits(1), sctx.traceID)
	assert.Equal(uint64(0x1), sctx.spanID)
	p, _ := sctx.SamplingPriority()
	assert.Equal(2, p)
}

// Assert that span links are generated only when trace headers contain divergent trace IDs
func TestSpanLinks(t *testing.T) {
	s, c := httpmem.ServerAndClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer s.Close()
	t.Run("Links on divergent trace IDs", func(t *testing.T) {
		carrier := TextMapCarrier{
			DefaultTraceIDHeader:  "1",
			DefaultParentIDHeader: "1",
			DefaultPriorityHeader: "3",
			traceparentHeader:     "00-00000000000000000000000000000002-0000000000000002-01",
			tracestateHeader:      "dd=s:1;o:rum;t.usr.id:baz64~~",
			b3TraceIDHeader:       "3",
			b3SpanIDHeader:        "3",
		}
		w3cLink := SpanLink{TraceID: 2, TraceIDHigh: 0, SpanID: 2, Tracestate: "dd=s:1;o:rum;t.usr.id:baz64~~", Flags: 1, Attributes: map[string]string{"reason": "terminated_context", "context_headers": "tracecontext"}}
		ddLink := SpanLink{TraceID: 1, TraceIDHigh: 0, SpanID: 1, Flags: 1, Attributes: map[string]string{"reason": "terminated_context", "context_headers": "datadog"}}
		b3Link := SpanLink{TraceID: 3, TraceIDHigh: 0, SpanID: 3, Tracestate: "", Flags: 0, Attributes: map[string]string{"reason": "terminated_context", "context_headers": "b3multi"}}
		tests := []struct {
			name   string
			envVal string
			out    []SpanLink
			tid    traceID
		}{
			{
				name:   "datadog first",
				envVal: "datadog,tracecontext,b3",
				out:    []SpanLink{w3cLink, b3Link},
				tid:    traceIDFrom64Bits(1),
			},
			{
				name:   "tracecontext first",
				envVal: "tracecontext,datadog,b3",
				out:    []SpanLink{ddLink, b3Link},
				tid:    traceIDFrom64Bits(2),
			},
			{
				name:   "b3 first",
				envVal: "b3,tracecontext,datadog",
				out:    []SpanLink{w3cLink, ddLink},
				tid:    traceIDFrom64Bits(3),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if tt.envVal != "" {
					t.Setenv(headerPropagationStyleExtract, tt.envVal)
				}
				tracer, err := newTracer(WithHTTPClient(c))
				assert.NoError(t, err)
				defer tracer.Stop()
				assert := assert.New(t)
				sctx, err := tracer.Extract(carrier)
				if err != nil {
					t.Fatal(err)
				}

				assert.Equal(tt.tid, sctx.traceID)
				assert.Len(sctx.spanLinks, 2)
				assert.Contains(sctx.spanLinks, tt.out[0])
				assert.Contains(sctx.spanLinks, tt.out[1])
			})
		}
	})
	t.Run("No links on equal trace IDs", func(t *testing.T) {
		carrier := TextMapCarrier{
			DefaultTraceIDHeader:  "1",
			DefaultParentIDHeader: "1",
			DefaultPriorityHeader: "3",
			traceparentHeader:     "00-00000000000000000000000000000001-0000000000000002-01",
			tracestateHeader:      "dd=s:1;o:rum;t.usr.id:baz64~~",
		}
		tracer, err := newTracer(WithHTTPClient(c))
		assert.NoError(t, err)
		defer tracer.Stop()
		assert := assert.New(t)
		sctx, err := tracer.Extract(carrier)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(traceIDFrom64Bits(1), sctx.traceID)
		assert.Len(sctx.spanLinks, 0)
	})
}

func TestW3CExtractsBaggage(t *testing.T) {
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(t, err)
	headers := TextMapCarrier{
		traceparentHeader:      "00-12345678901234567890123456789012-1234567890123456-01",
		tracestateHeader:       "dd=s:2;o:rum;t.usr.id:baz64~~",
		"ot-baggage-something": "someVal",
	}
	s, err := tracer.Extract(headers)
	assert.NoError(t, err)
	found := false
	s.ForeachBaggageItem(func(k, _ string) bool {
		if k == "something" {
			found = true
			return false
		}
		return true
	})
	assert.True(t, found)
}

func TestNonePropagator(t *testing.T) {
	t.Run("inject/none", func(t *testing.T) {
		t.Setenv(headerPropagationStyleInject, "none")
		tracer, err := newTracer()
		defer tracer.Stop()
		assert.NoError(t, err)
		root := tracer.StartSpan("web.request")
		root.SetTag(ext.ManualDrop, true)
		root.SetBaggageItem("item", "x")
		ctx := root.Context()
		ctx.traceID = traceIDFrom64Bits(1)
		ctx.spanID = 1
		headers := TextMapCarrier(map[string]string{})
		err = tracer.Inject(ctx, headers)

		assert := assert.New(t)
		assert.Nil(err)
		assert.Len(headers, 0)
	})

	t.Run("inject/none,b3", func(t *testing.T) {
		t.Setenv(headerPropagationStyleInject, "none,b3")
		tp := new(log.RecordLogger)
		tp.Ignore("appsec: ", "telemetry")
		tracer, err := newTracer(WithLogger(tp), WithEnv("test"))
		assert.Nil(t, err)
		defer tracer.Stop()
		assert.NoError(t, err)
		// reinitializing to capture log output, since propagators are parsed before logger is set
		tracer.config.propagator = NewPropagator(&PropagatorConfig{})
		root := tracer.StartSpan("web.request")
		root.SetTag(ext.ManualDrop, true)
		root.SetBaggageItem("item", "x")
		ctx := root.Context()
		ctx.traceID = traceIDFrom64Bits(1)
		ctx.spanID = 1
		headers := TextMapCarrier(map[string]string{})
		err = tracer.Inject(ctx, headers)

		assert := assert.New(t)
		assert.Nil(err)
		assert.Equal("0000000000000001", headers[b3TraceIDHeader])
		assert.Equal("0000000000000001", headers[b3SpanIDHeader])
		assert.Contains(tp.Logs()[0], "Propagator \"none\" has no effect when combined with other propagators. "+
			"To disable the propagator, set to `none`")
	})

	t.Run("extract/none", func(t *testing.T) {
		t.Setenv(headerPropagationStyleExtract, "none")
		assert := assert.New(t)
		tracer, err := newTracer()
		defer tracer.Stop()
		assert.NoError(err)
		root := tracer.StartSpan("web.request")
		root.SetTag(ext.ManualDrop, true)
		root.SetBaggageItem("item", "x")
		headers := TextMapCarrier(map[string]string{})

		_, err = tracer.Extract(headers)

		assert.Equal(err, ErrSpanContextNotFound)
		assert.Len(headers, 0)
	})

	t.Run("inject,extract/none", func(t *testing.T) {
		t.Run("", func(t *testing.T) {
			t.Setenv(headerPropagationStyle, "NoNe")
			tracer, err := newTracer()
			defer tracer.Stop()
			assert.NoError(t, err)
			root := tracer.StartSpan("web.request")
			root.SetTag(ext.ManualDrop, true)
			root.SetBaggageItem("item", "x")
			ctx := root.Context()
			ctx.traceID = traceIDFrom64Bits(1)
			ctx.spanID = 1
			headers := TextMapCarrier(map[string]string{})
			err = tracer.Inject(ctx, headers)

			assert := assert.New(t)
			assert.Nil(err)
			assert.Len(headers, 0)

			_, err = tracer.Extract(headers)
			assert.Equal(err, ErrSpanContextNotFound)
		})
		t.Run("", func(t *testing.T) {
			t.Setenv(otelHeaderPropagationStyle, "NoNe")
			tracer, err := newTracer()
			assert.NoError(t, err)
			defer tracer.Stop()
			root := tracer.StartSpan("web.request")
			root.SetTag(ext.ManualDrop, true)
			root.SetBaggageItem("item", "x")
			ctx := root.Context()
			ctx.traceID = traceIDFrom64Bits(1)
			ctx.spanID = 1
			headers := TextMapCarrier(map[string]string{})
			err = tracer.Inject(ctx, headers)

			assert := assert.New(t)
			assert.Nil(err)
			assert.Len(headers, 0)

			_, err = tracer.Extract(headers)
			assert.Equal(err, ErrSpanContextNotFound)
		})
		t.Run("", func(t *testing.T) {
			t.Setenv(headerPropagationStyleExtract, "NoNe")
			t.Setenv(headerPropagationStyleInject, "NoNe")
			tracer, err := newTracer()
			defer tracer.Stop()
			assert.NoError(t, err)
			root := tracer.StartSpan("web.request")
			root.SetTag(ext.ManualDrop, true)
			root.SetBaggageItem("item", "x")
			ctx := root.Context()
			ctx.traceID = traceIDFrom64Bits(1)
			ctx.spanID = 1
			headers := TextMapCarrier(map[string]string{})
			err = tracer.Inject(ctx, headers)

			assert := assert.New(t)
			assert.Nil(err)
			assert.Len(headers, 0)

			_, err = tracer.Extract(headers)
			assert.Equal(err, ErrSpanContextNotFound)
		})
	})
}

func assertTraceTags(t *testing.T, expected, actual string) {
	assert.ElementsMatch(t, strings.Split(expected, ","), strings.Split(actual, ","))
}

func TestOtelPropagator(t *testing.T) {
	tests := []struct {
		env    string
		result string
	}{
		{
			env:    "tracecontext, b3",
			result: "tracecontext,b3 single header",
		},
		{
			env:    "b3multi , jaegar , datadog ",
			result: "b3multi,datadog",
		},
		{
			env:    "none",
			result: "",
		},
		{
			env:    "nonesense",
			result: "datadog,tracecontext,baggage",
		},
		{
			env:    "jaegar",
			result: "datadog,tracecontext,baggage",
		},
	}
	for _, test := range tests {
		t.Setenv(otelHeaderPropagationStyle, test.env)
		t.Run(fmt.Sprintf("inject with %v=%v", otelHeaderPropagationStyle, test.env), func(t *testing.T) {
			assert := assert.New(t)
			c, err := newConfig()
			assert.NoError(err)
			cp, ok := c.propagator.(*chainedPropagator)
			assert.True(ok)
			assert.Equal(test.result, cp.injectorNames)
			assert.Equal(test.result, cp.extractorsNames)
		})
	}
}

// Assert that extraction returns a ErrSpanContextNotFound error when no trace context headers are found
func TestExtractNoHeaders(t *testing.T) {
	tests := []struct {
		name         string
		extractEnv   string
		extractFirst bool
	}{
		{
			name:         "single header",
			extractEnv:   "datadog",
			extractFirst: false,
		},
		{
			name:         "single header - extractFirst",
			extractEnv:   "datadog",
			extractFirst: true,
		},
		{
			name:         "multi header",
			extractEnv:   "datadog,tracecontext",
			extractFirst: false,
		},
		{
			name:         "multi header - extractFirst",
			extractEnv:   "datadog,tracecontext",
			extractFirst: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(headerPropagationStyleExtract, tt.extractEnv)
			if tt.extractFirst {
				t.Setenv("DD_TRACE_PROPAGATION_EXTRACT_FIRST", "true")
			}
			tracer, err := newTracer()
			assert.NoError(t, err)
			defer tracer.Stop()
			ctx, err := tracer.Extract(TextMapCarrier{})
			assert.Equal(t, ErrSpanContextNotFound, err)
			assert.Nil(t, ctx)
		})
	}
}

func BenchmarkInjectDatadog(b *testing.B) {
	b.Setenv(headerPropagationStyleInject, "datadog")
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(b, err)
	root := tracer.StartSpan("test")
	defer root.Finish()
	for i := 0; i < 20; i++ {
		setPropagatingTag(root.Context(), fmt.Sprintf("%d", i), fmt.Sprintf("%d", i))
	}
	dst := map[string]string{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracer.Inject(root.Context(), TextMapCarrier(dst))
	}
}

func BenchmarkInjectW3C(b *testing.B) {
	b.Setenv(headerPropagationStyleInject, "tracecontext")
	tracer, err := newTracer()
	defer tracer.Stop()
	assert.NoError(b, err)
	root := tracer.StartSpan("test")
	defer root.Finish()

	ctx := root.Context()

	setPropagatingTag(ctx, tracestateHeader,
		"othervendor=t61rcWkgMzE,dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~")

	for i := 0; i < 100; i++ {
		// _dd.p. prefix is needed for w3c
		k := fmt.Sprintf("_dd.p.k%d", i)
		v := fmt.Sprintf("v%d", i)
		setPropagatingTag(ctx, k, v)
	}
	dst := map[string]string{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracer.Inject(root.Context(), TextMapCarrier(dst))
	}
}

func BenchmarkExtractDatadog(b *testing.B) {
	b.Setenv(headerPropagationStyleExtract, "datadog")
	propagator := NewPropagator(nil)
	carrier := TextMapCarrier(map[string]string{
		DefaultTraceIDHeader:  "1123123132131312313123123",
		DefaultParentIDHeader: "1212321131231312312312312",
		DefaultPriorityHeader: "-1",
		traceTagsHeader: `adad=ada2,adad=ada2,ad1d=ada2,adad=ada2,adad=ada2,
								adad=ada2,adad=aad2,adad=ada2,adad=ada2,adad=ada2,adad=ada2`,
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		propagator.Extract(carrier)
	}
}

func BenchmarkExtractW3C(b *testing.B) {
	b.Setenv(headerPropagationStyleExtract, "tracecontext")
	propagator := NewPropagator(nil)
	carrier := TextMapCarrier(map[string]string{
		traceparentHeader: "00-00000000000000001111111111111111-2222222222222222-01",
		tracestateHeader:  "dd=s:2;o:rum;t.dm:-4;t.usr.id:baz64~~,othervendor=t61rcWkgMzE",
	})
	b.ResetTimer()
	log.SetLevel(log.LevelError)
	for i := 0; i < b.N; i++ {
		propagator.Extract(carrier)
	}
}

func FuzzMarshalPropagatingTags(f *testing.F) {
	f.Add("testA", "testB", "testC", "testD", "testG", "testF")
	f.Fuzz(func(t *testing.T, key1 string, val1 string,
		key2 string, val2 string, key3 string, val3 string) {

		sendCtx := new(SpanContext)
		sendCtx.trace = newTrace()
		recvCtx := new(SpanContext)
		recvCtx.trace = newTrace()

		pConfig := PropagatorConfig{MaxTagsHeaderLen: 128}
		propagator := propagator{&pConfig}
		tags := map[string]string{key1: val1, key2: val2, key3: val3}
		for key, val := range tags {
			sendCtx.trace.setPropagatingTag(key, val)
		}
		marshal := propagator.marshalPropagatingTags(sendCtx)
		if _, ok := sendCtx.trace.tags[keyPropagationError]; ok {
			t.Skipf("Skipping invalid tags")
		}
		unmarshalPropagatingTags(recvCtx, marshal)
		marshaled := sendCtx.trace.propagatingTags
		unmarshaled := recvCtx.trace.propagatingTags
		if !reflect.DeepEqual(sendCtx.trace.propagatingTags, recvCtx.trace.propagatingTags) {
			t.Fatalf("Inconsistent marshaling/unmarshaling: (%q) is different from (%q)", marshaled, unmarshaled)
		}
	})
}

func FuzzComposeTracestate(f *testing.F) {
	testCases := []struct {
		priority                         int
		k1, v1, k2, v2, k3, v3, oldState string
	}{
		{priority: 1,
			k1: "keyOne", v1: "json",
			k2: "KeyTwo", v2: "123123",
			k3: "table", v3: "chair",
			oldState: "dd=s:-2;o:synthetics___web"},
	}
	for _, tc := range testCases {
		f.Add(tc.priority, tc.k1, tc.v1, tc.k2, tc.v2, tc.k3, tc.v3, tc.oldState)
	}
	f.Fuzz(func(t *testing.T, priority int, key1 string, val1 string,
		key2 string, val2 string, key3 string, val3 string, oldState string) {

		sendCtx := new(SpanContext)
		sendCtx.trace = newTrace()
		recvCtx := new(SpanContext)
		recvCtx.trace = newTrace()

		sm := &stringMutator{}
		tags := map[string]string{key1: val1, key2: val2, key3: val3}
		totalLen := 0
		for key, val := range tags {
			k := "_dd.p." + sm.Mutate(keyDisallowedFn, key)
			v := sm.Mutate(valueDisallowedFn, val)
			if strings.ContainsAny(k, ":;") {
				t.Skipf("Skipping invalid tags")
			}
			if strings.HasSuffix(v, " ") {
				t.Skipf("Skipping invalid tags")
			}
			totalLen += len(k) + len(v)
			if totalLen > 128 {
				break
			}
			sendCtx.trace.setPropagatingTag(k, v)
		}
		if len(strings.Split(strings.Trim(oldState, " \t"), ",")) > 31 {
			t.Skipf("Skipping invalid tags")
		}
		traceState := composeTracestate(sendCtx, priority, oldState)
		parseTracestate(recvCtx, traceState)
		setPropagatingTag(sendCtx, tracestateHeader, traceState)
		if !reflect.DeepEqual(sendCtx.trace.propagatingTags, recvCtx.trace.propagatingTags) {
			t.Fatalf(`Inconsistent composing/parsing:
			pre compose: (%q)
			is different from
			parsed: (%q)
			for tracestate of: (%s)`, sendCtx.trace.propagatingTags,
				recvCtx.trace.propagatingTags,
				traceState)
		}
	})
}

func FuzzParseTraceparent(f *testing.F) {
	testCases := []struct {
		version, traceID, spanID, flags string
	}{
		{"00", "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7", "01"},
		{"01", "00000000000000001111111111111111", "9565876494606882", "02"},
	}
	for _, tc := range testCases {
		f.Add(tc.version, tc.traceID, tc.spanID, tc.flags)
	}
	f.Fuzz(func(t *testing.T, version string, traceID string,
		spanID string, flags string) {

		ctx := new(SpanContext)
		ctx.trace = newTrace()

		header := strings.Join([]string{version, traceID, spanID, flags}, "-")

		if parseTraceparent(ctx, header) != nil {
			t.Skipf("Error parsing parent")
		}
		parsedSamplingPriority, ok := ctx.SamplingPriority()
		if !ok {
			t.Skipf("Error retrieving sampling priority")
		}
		expectedSpanID, err := strconv.ParseUint(spanID, 16, 64)
		if err != nil {
			t.Skipf("Error parsing span ID")
		}
		expectedFlag, err := strconv.ParseInt(flags, 16, 8)
		if err != nil {
			t.Skipf("Error parsing flag")
		}
		if gotTraceID := ctx.TraceID(); gotTraceID != strings.ToLower(traceID) {
			t.Fatalf(`Inconsistent trace id parsing:
					got: %s
					wanted: %s
					for header of: %s`, gotTraceID, traceID, header)
		}
		if ctx.spanID != expectedSpanID {
			t.Fatalf(`Inconsistent span id parsing:
				got: %d
				wanted: %d
				for header of: %s`, ctx.spanID, expectedSpanID, header)
		}
		if parsedSamplingPriority != int(expectedFlag)&0x1 {
			t.Fatalf(`Inconsistent flag parsing:
					got: %d
					wanted: %d
					for header of: %s`, parsedSamplingPriority, int(expectedFlag)&0x1, header)
		}
	})
}

func FuzzExtractTraceID128(f *testing.F) {
	f.Fuzz(func(_ *testing.T, v string) {
		ctx := new(SpanContext)
		extractTraceID128(ctx, v) // make sure it doesn't panic
	})
}

// Regression test for https://github.com/DataDog/dd-trace-go/issues/1944
func TestPropagatingTagsConcurrency(t *testing.T) {
	// This test ensures Injection can be done concurrently.
	trc, err := newTracer()
	defer trc.Stop()
	assert.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 1_000; i++ {
		root := trc.StartSpan("test")
		wg.Add(5)
		for i := 0; i < 5; i++ {
			go func() {
				defer wg.Done()
				trc.Inject(root.Context(), TextMapCarrier(make(map[string]string)))
			}()
		}
		wg.Wait()
	}
}

func TestMalformedTID(t *testing.T) {
	assert := assert.New(t)
	tracer, err := newTracer()
	assert.Nil(err)
	setGlobalTracer(tracer)
	defer tracer.Stop()
	defer setGlobalTracer(&NoopTracer{})

	t.Run("datadog, short tid", func(_ *testing.T) {
		headers := TextMapCarrier(map[string]string{
			DefaultTraceIDHeader:  "1234567890123456789",
			DefaultParentIDHeader: "987654321",
			traceTagsHeader:       "_dd.p.tid=1234567890abcde",
		})
		sctx, err := tracer.Extract(headers)
		assert.Nil(err)
		root := tracer.StartSpan("web.request", ChildOf(sctx))
		root.Finish()
		assert.NotContains(root.meta, keyTraceID128)
	})

	t.Run("datadog, malformed tid", func(_ *testing.T) {
		headers := TextMapCarrier(map[string]string{
			DefaultTraceIDHeader:  "1234567890123456789",
			DefaultParentIDHeader: "987654321",
			traceTagsHeader:       "_dd.p.tid=XXXXXXXXXXXXXXXX",
		})
		sctx, err := tracer.Extract(headers)
		assert.Nil(err)
		root := tracer.StartSpan("web.request", ChildOf(sctx))
		root.Finish()
		assert.NotContains(root.meta, keyTraceID128)
	})

	t.Run("datadog, valid tid", func(_ *testing.T) {
		headers := TextMapCarrier(map[string]string{
			DefaultTraceIDHeader:  "1234567890123456789",
			DefaultParentIDHeader: "987654321",
			traceTagsHeader:       "_dd.p.tid=640cfd8d00000000",
		})
		sctx, err := tracer.Extract(headers)
		assert.Nil(err)
		root := tracer.StartSpan("web.request", ChildOf(sctx))
		root.Finish()
		assert.Equal("640cfd8d00000000", root.meta[keyTraceID128])
	})
}

func BenchmarkComposeTracestate(b *testing.B) {
	ctx := new(SpanContext)
	ctx.trace = newTrace()
	ctx.origin = "synthetics"
	ctx.trace.setPropagatingTag("_dd.p.keyOne", "json")
	ctx.trace.setPropagatingTag("_dd.p.KeyTwo", "123123")
	ctx.trace.setPropagatingTag("_dd.p.table", "chair")
	ctx.isRemote = false
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		composeTracestate(ctx, 1, "s:-2;o:synthetics___web")
	}
}

func TestStringMutator(t *testing.T) {
	sm := &stringMutator{}
	rx := regexp.MustCompile(`,|~|;|[^\x21-\x7E]+`)
	tc := []struct {
		name  string
		input string
	}{
		{
			name:  "empty",
			input: "",
		},
		{
			name:  "no special characters",
			input: "abcdef",
		},
		{
			name:  "special characters",
			input: "a,b;c~~~~d;",
		},
		{
			name:  "special characters and non-ascii",
			input: "a,b👍👍👍;c~d👍;",
		},
	}
	for _, tt := range tc {
		t.Run(tt.name, func(t *testing.T) {
			expected := rx.ReplaceAllString(tt.input, "_")
			actual := sm.Mutate(originDisallowedFn, tt.input)
			assert.Equal(t, expected, actual)
		})
	}
	t.Run("raw string", func(t *testing.T) {
		expected := "a_b_c____d_~"
		actual := sm.Mutate(originDisallowedFn, "a,b;c~~~~d;=")
		assert.Equal(t, expected, actual)
	})
}

func FuzzStringMutator(f *testing.F) {
	rx := regexp.MustCompile(`,|~|;|[^\x21-\x7E]+`)
	f.Add("a,b;c~~~~d;")
	f.Add("a,b👍👍👍;c~d👍;")
	f.Add("=")
	f.Fuzz(func(t *testing.T, input string) {
		sm := &stringMutator{}
		expected := strings.ReplaceAll(rx.ReplaceAllString(input, "_"), "=", "~")
		actual := sm.Mutate(originDisallowedFn, input)
		if expected != actual {
			t.Fatalf("expected: %s, actual: %s", expected, actual)
		}
	})
}

func TestInjectBaggagePropagator(t *testing.T) {

	assert := assert.New(t)

	propagator := NewPropagator(&PropagatorConfig{
		BaggageHeader: "baggage",
		TraceHeader:   "tid",
		ParentHeader:  "pid",
	})
	tracer, err := newTracer(WithPropagator(propagator))
	assert.NoError(err)
	defer tracer.Stop()

	root := tracer.StartSpan("web.request")
	root.SetBaggageItem("foo", "bar")
	ctx := root.Context()
	headers := http.Header{}

	carrier := HTTPHeadersCarrier(headers)
	err = tracer.Inject(ctx, carrier)
	assert.Nil(err)

	assert.Equal(headers.Get("baggage"), "foo=bar")
}

func TestExtractBaggagePropagator(t *testing.T) {
	tracer, err := newTracer()
	assert.NoError(t, err)
	defer tracer.Stop()
	headers := TextMapCarrier{
		DefaultTraceIDHeader:  "4",
		DefaultParentIDHeader: "1",
		DefaultBaggageHeader:  "foo=bar",
	}
	s, err := tracer.Extract(headers)
	assert.NoError(t, err)
	got := make(map[string]string)
	s.ForeachBaggageItem(func(k, v string) bool {
		got[k] = v
		return true
	})
	assert.Len(t, got, 1)
	assert.Equal(t, "bar", got["foo"])
}

func TestInjectBaggagePropagatorEncoding(t *testing.T) {
	assert := assert.New(t)

	propagator := NewPropagator(&PropagatorConfig{
		BaggageHeader: "baggage",
		TraceHeader:   "tid",
		ParentHeader:  "pid",
	})
	tracer, err := newTracer(WithPropagator(propagator))
	assert.NoError(err)
	defer tracer.Stop()

	root := tracer.StartSpan("web.request")
	root.SetBaggageItem("userId", "Amélie")
	root.SetBaggageItem("serverNode", "DF 28")
	headers := http.Header{}

	carrier := HTTPHeadersCarrier(headers)
	err = tracer.Inject(root.Context(), carrier)
	assert.Nil(err)
	actualBaggage := headers.Get("baggage")
	// Instead of checking equality of the whole string, assert that both key/value pairs are present.
	assert.Contains(actualBaggage, "userId=Am%C3%A9lie")
	assert.Contains(actualBaggage, "serverNode=DF+28")
}

func TestInjectBaggagePropagatorEncodingSpecialCharacters(t *testing.T) {
	assert := assert.New(t)

	propagator := NewPropagator(&PropagatorConfig{
		BaggageHeader: "baggage",
		TraceHeader:   "tid",
		ParentHeader:  "pid",
	})
	tracer, err := newTracer(WithPropagator(propagator))
	assert.NoError(err)
	defer tracer.Stop()

	root := tracer.StartSpan("web.request")
	ctx := root.Context()
	root.SetBaggageItem(",;\\()/:<=>?@[]{}", ",;\\")
	headers := http.Header{}

	carrier := HTTPHeadersCarrier(headers)
	err = tracer.Inject(ctx, carrier)
	assert.Nil(err)

	assert.Equal(headers.Get("baggage"), "%2C%3B%5C%28%29%2F%3A%3C%3D%3E%3F%40%5B%5D%7B%7D=%2C%3B%5C")
}

func TestExtractBaggagePropagatorDecoding(t *testing.T) {
	tracer, err := newTracer()
	assert.NoError(t, err)
	defer tracer.Stop()
	headers := TextMapCarrier{
		DefaultTraceIDHeader:  "4",
		DefaultParentIDHeader: "1",
		DefaultBaggageHeader:  "userId=Am%C3%A9lie,serverNode=DF+28",
	}
	s, err := tracer.Extract(headers)
	assert.NoError(t, err)
	got := make(map[string]string)
	s.ForeachBaggageItem(func(k, v string) bool {
		got[k] = v
		return true
	})
	assert.Len(t, got, 2)
	assert.Equal(t, "Amélie", got["userId"])
	assert.Equal(t, "DF 28", got["serverNode"])
}

func TestInjectBaggageMaxItems(t *testing.T) {
	assert := assert.New(t)

	propagator := NewPropagator(&PropagatorConfig{
		BaggageHeader: "baggage",
	})
	tracer, err := newTracer(WithPropagator(propagator))
	assert.NoError(err)
	defer tracer.Stop()

	root := tracer.StartSpan("web.request")
	ctx := root.Context()

	for i := 0; i < baggageMaxItems+2; i++ {
		iString := strconv.Itoa(i)
		ctx.setBaggageItem("key"+iString, "val"+iString)
	}

	headers := http.Header{}

	carrier := HTTPHeadersCarrier(headers)
	err = tracer.Inject(ctx, carrier)
	assert.Nil(err)

	headerValue := headers.Get("baggage")
	items := strings.Split(headerValue, ",")
	assert.Equal(baggageMaxItems, len(items))
}

func TestInjectBaggageMaxBytes(t *testing.T) {
	assert := assert.New(t)

	propagator := NewPropagator(&PropagatorConfig{
		BaggageHeader: "baggage",
	})
	tracer, err := newTracer(WithPropagator(propagator))
	assert.NoError(err)
	defer tracer.Stop()

	root := tracer.StartSpan("web.request")
	ctx := root.Context()

	baggageItems := map[string]string{
		"key0": "o",
		"key1": strings.Repeat("a", baggageMaxBytes/3),
		"key2": strings.Repeat("b", baggageMaxBytes/3),
		"key3": strings.Repeat("c", baggageMaxBytes/3),
	}

	ctx.baggage = baggageItems
	headers := http.Header{}

	carrier := HTTPHeadersCarrier(headers)
	err = tracer.Inject(ctx, carrier)
	assert.Nil(err)

	headerValue := headers.Get("baggage")
	headerSize := len([]byte(headerValue))
	assert.LessOrEqual(headerSize, baggageMaxBytes)
}

func TestExtractBaggagePropagatorMalformedHeader(t *testing.T) {
	t.Run("missing equal sign", func(t *testing.T) {
		tracer, err := newTracer()
		assert.NoError(t, err)
		defer tracer.Stop()
		headers := TextMapCarrier{
			DefaultTraceIDHeader:  "4",
			DefaultParentIDHeader: "1",
			DefaultBaggageHeader:  "key1,key2=value2",
		}
		s, err := tracer.Extract(headers)
		assert.NoError(t, err)
		// since the header is malformed, we should not have any baggage items
		got := make(map[string]string)
		s.ForeachBaggageItem(func(k, v string) bool {
			got[k] = v
			return true
		})
		assert.Len(t, got, 0)
	})
	t.Run("missing value", func(t *testing.T) {
		tracer, err := newTracer()
		assert.NoError(t, err)
		defer tracer.Stop()
		headers := TextMapCarrier{
			DefaultTraceIDHeader:  "4",
			DefaultParentIDHeader: "1",
			DefaultBaggageHeader:  "key1=value1,key2=",
		}
		s, err := tracer.Extract(headers)
		assert.NoError(t, err)
		// since the header is malformed, we should not have any baggage items
		got := make(map[string]string)
		s.ForeachBaggageItem(func(k, v string) bool {
			got[k] = v
			return true
		})
		assert.Len(t, got, 0)
	})
	t.Run("missing key", func(t *testing.T) {
		tracer, err := newTracer()
		assert.NoError(t, err)
		defer tracer.Stop()
		headers := TextMapCarrier{
			DefaultTraceIDHeader:  "4",
			DefaultParentIDHeader: "1",
			DefaultBaggageHeader:  "key1=value1,=value2",
		}
		s, err := tracer.Extract(headers)
		assert.NoError(t, err)
		// since the header is malformed, we should not have any baggage items
		got := make(map[string]string)
		s.ForeachBaggageItem(func(k, v string) bool {
			got[k] = v
			return true
		})
		assert.Len(t, got, 0)
	})
	t.Run("missing key and value", func(t *testing.T) {
		tracer, err := newTracer()
		assert.NoError(t, err)
		defer tracer.Stop()
		headers := TextMapCarrier{
			DefaultTraceIDHeader:  "4",
			DefaultParentIDHeader: "1",
			DefaultBaggageHeader:  "=,key1=value1",
		}
		s, err := tracer.Extract(headers)
		assert.NoError(t, err)
		// since the header is malformed, we should not have any baggage items
		got := make(map[string]string)
		s.ForeachBaggageItem(func(k, v string) bool {
			got[k] = v
			return true
		})
		assert.Len(t, got, 0)
	})
	t.Run("missing key-value pair", func(t *testing.T) {
		tracer, err := newTracer()
		assert.NoError(t, err)
		defer tracer.Stop()
		headers := TextMapCarrier{
			DefaultTraceIDHeader:  "4",
			DefaultParentIDHeader: "1",
			DefaultBaggageHeader:  "key1=value1,",
		}
		s, err := tracer.Extract(headers)
		assert.NoError(t, err)
		// since the header is malformed, we should not have any baggage items
		got := make(map[string]string)
		s.ForeachBaggageItem(func(k, v string) bool {
			got[k] = v
			return true
		})
		assert.Len(t, got, 0)
	})
}

func TestExtractOnlyBaggage(t *testing.T) {
	t.Setenv("DD_TRACE_PROPAGATION_STYLE", "baggage")
	headers := TextMapCarrier(map[string]string{
		"baggage": "foo=bar,baz=qux",
	})

	tracer, err := newTracer()
	assert.NoError(t, err)
	defer tracer.Stop()

	ctx, err := tracer.Extract(headers)
	assert.Nil(t, err)

	got := make(map[string]string)
	ctx.ForeachBaggageItem(func(k, v string) bool {
		got[k] = v
		return true
	})
	assert.Len(t, got, 2)
	assert.Equal(t, "bar", got["foo"])
	assert.Equal(t, "qux", got["baz"])
}

// TestExtractBaggageFirstThenDatadog verifies that when both baggage and trace headers are present,
// the trace context (trace ID, parent ID, etc.) is extracted from trace headers, and the baggage items are properly inherited,
// specifically when baggage has a higher precedence than trace headers in the propagation style.
func TestExtractBaggageFirstThenDatadog(t *testing.T) {
	t.Setenv("DD_TRACE_PROPAGATION_STYLE", "baggage,datadog")

	// Set up headers with both baggage and Datadog trace context
	headers := TextMapCarrier(map[string]string{
		"baggage":             "item=xyz",
		DefaultTraceIDHeader:  "12345",
		DefaultParentIDHeader: "67890",
		DefaultPriorityHeader: "1",
	})

	tracer, err := newTracer()
	assert.NoError(t, err)
	defer tracer.Stop()

	ctx, err := tracer.Extract(headers)
	assert.NoError(t, err)

	// Verify that trace context is taken from Datadog headers, despite baggage being listed first in propagation style
	expectedTraceID := traceIDFrom64Bits(12345)
	assert.Equal(t, expectedTraceID, ctx.traceID)
	assert.Equal(t, uint64(67890), ctx.spanID)

	got := make(map[string]string)
	ctx.ForeachBaggageItem(func(k, v string) bool {
		got[k] = v
		return true
	})
	assert.Len(t, got, 1)
	assert.Equal(t, "xyz", got["item"])
}

// TestSpanContextDebugLoggingSecurity verifies that debug logging of span context
// does not expose sensitive data from baggage or other fields.
func TestSpanContextDebugLoggingSecurity(t *testing.T) {
	// Set up a record logger to capture debug output
	tp := new(log.RecordLogger)

	// Enable debug mode to trigger the debug logging
	tracer, err := newTracer(WithLogger(tp), WithDebugMode(true))
	assert.NoError(t, err)
	defer tracer.Stop()

	// Create headers with sensitive data in baggage
	headers := TextMapCarrier(map[string]string{
		"baggage":             "api_key=secret123,password=sensitive_password,token=bearer_token_abc",
		DefaultTraceIDHeader:  "12345",
		DefaultParentIDHeader: "67890",
		DefaultPriorityHeader: "1",
	})

	// Clear any existing logs before extraction
	tp.Reset()

	// Extract span context - this should trigger the debug log
	ctx, err := tracer.Extract(headers)
	assert.NoError(t, err)
	assert.NotNil(t, ctx)

	// Verify that baggage was extracted
	got := make(map[string]string)
	ctx.ForeachBaggageItem(func(k, v string) bool {
		got[k] = v
		return true
	})
	assert.Len(t, got, 3)
	assert.Equal(t, "secret123", got["api_key"])
	assert.Equal(t, "sensitive_password", got["password"])
	assert.Equal(t, "bearer_token_abc", got["token"])

	// Check the debug logs - they should NOT contain sensitive data
	logs := tp.Logs()

	// Find the span context debug log
	var contextLog string
	for _, logEntry := range logs {
		if strings.Contains(logEntry, "Extracted span context:") {
			contextLog = logEntry
			break
		}
	}

	// The log should exist
	assert.NotEmpty(t, contextLog, "Expected to find span context debug log")

	// The log should NOT contain sensitive baggage values
	assert.NotContains(t, contextLog, "secret123", "Debug log should not expose API key")
	assert.NotContains(t, contextLog, "sensitive_password", "Debug log should not expose password")
	assert.NotContains(t, contextLog, "bearer_token_abc", "Debug log should not expose token")

	// The log should still contain useful debug information (trace ID, span ID)
	assert.Contains(t, contextLog, "67890", "Debug log should contain span ID")
	assert.Contains(t, contextLog, "traceID=", "Debug log should contain trace ID field")
	assert.Contains(t, contextLog, "hasBaggage=true", "Debug log should indicate baggage presence")
	assert.Contains(t, contextLog, "baggageCount=3", "Debug log should show baggage count")

	// This test ensures that the SafeDebugString() method is used instead of %#v
	// to prevent sensitive baggage data from being exposed in debug logs.
}
