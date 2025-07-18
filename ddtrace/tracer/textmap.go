// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"maps"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/internal"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
	"github.com/DataDog/dd-trace-go/v2/internal/samplernames"
)

// HTTPHeadersCarrier wraps an http.Header as a TextMapWriter and TextMapReader, allowing
// it to be used using the provided Propagator implementation.
type HTTPHeadersCarrier http.Header

var _ TextMapWriter = (*HTTPHeadersCarrier)(nil)
var _ TextMapReader = (*HTTPHeadersCarrier)(nil)

// Set implements TextMapWriter.
func (c HTTPHeadersCarrier) Set(key, val string) {
	http.Header(c).Set(key, val)
}

// ForeachKey implements TextMapReader.
func (c HTTPHeadersCarrier) ForeachKey(handler func(key, val string) error) error {
	for k, vals := range c {
		for _, v := range vals {
			if err := handler(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// TextMapCarrier allows the use of a regular map[string]string as both TextMapWriter
// and TextMapReader, making it compatible with the provided Propagator.
type TextMapCarrier map[string]string

var _ TextMapWriter = (*TextMapCarrier)(nil)
var _ TextMapReader = (*TextMapCarrier)(nil)

// Set implements TextMapWriter.
func (c TextMapCarrier) Set(key, val string) {
	c[key] = val
}

// ForeachKey conforms to the TextMapReader interface.
func (c TextMapCarrier) ForeachKey(handler func(key, val string) error) error {
	for k, v := range c {
		if err := handler(k, v); err != nil {
			return err
		}
	}
	return nil
}

const (
	headerPropagationStyleInject  = "DD_TRACE_PROPAGATION_STYLE_INJECT"
	headerPropagationStyleExtract = "DD_TRACE_PROPAGATION_STYLE_EXTRACT"
	headerPropagationStyle        = "DD_TRACE_PROPAGATION_STYLE"
)

const (
	// DefaultBaggageHeaderPrefix specifies the prefix that will be used in
	// HTTP headers or text maps to prefix baggage keys.
	DefaultBaggageHeaderPrefix = "ot-baggage-"

	// DefaultTraceIDHeader specifies the key that will be used in HTTP headers
	// or text maps to store the trace ID.
	DefaultTraceIDHeader = "x-datadog-trace-id"

	// DefaultParentIDHeader specifies the key that will be used in HTTP headers
	// or text maps to store the parent ID.
	DefaultParentIDHeader = "x-datadog-parent-id"

	// DefaultPriorityHeader specifies the key that will be used in HTTP headers
	// or text maps to store the sampling priority value.
	DefaultPriorityHeader = "x-datadog-sampling-priority"

	// DefaultBaggageHeader specifies the key that will be used in HTTP headers
	// or text maps to store the baggage value.
	DefaultBaggageHeader = "baggage"
)

// originHeader specifies the name of the header indicating the origin of the trace.
// It is used with the Synthetics product and usually has the value "synthetics".
const originHeader = "x-datadog-origin"

// traceTagsHeader holds the propagated trace tags
const traceTagsHeader = "x-datadog-tags"

// propagationExtractMaxSize limits the total size of incoming propagated tags to parse
const propagationExtractMaxSize = 512

// PropagatorConfig defines the configuration for initializing a propagator.
type PropagatorConfig struct {
	// BaggagePrefix specifies the prefix that will be used to store baggage
	// items in a map. It defaults to DefaultBaggageHeaderPrefix.
	BaggagePrefix string

	// TraceHeader specifies the map key that will be used to store the trace ID.
	// It defaults to DefaultTraceIDHeader.
	TraceHeader string

	// ParentHeader specifies the map key that will be used to store the parent ID.
	// It defaults to DefaultParentIDHeader.
	ParentHeader string

	// PriorityHeader specifies the map key that will be used to store the sampling priority.
	// It defaults to DefaultPriorityHeader.
	PriorityHeader string

	// MaxTagsHeaderLen specifies the maximum length of trace tags header value.
	// It defaults to defaultMaxTagsHeaderLen, a value of 0 disables propagation of tags.
	MaxTagsHeaderLen int

	// B3 specifies if B3 headers should be added for trace propagation.
	// See https://github.com/openzipkin/b3-propagation
	B3 bool

	// BaggageHeader specifies the map key that will be used to store the baggage key-value pairs.
	// It defaults to DefaultBaggageHeader.
	BaggageHeader string
}

// NewPropagator returns a new propagator which uses TextMap to inject
// and extract values. It propagates trace and span IDs and baggage.
// To use the defaults, nil may be provided in place of the config.
//
// The inject and extract propagators are determined using environment variables
// with the following order of precedence:
//  1. DD_TRACE_PROPAGATION_STYLE_INJECT
//  2. DD_TRACE_PROPAGATION_STYLE (applies to both inject and extract)
//  3. If none of the above, use default values
func NewPropagator(cfg *PropagatorConfig, propagators ...Propagator) Propagator {
	if cfg == nil {
		cfg = new(PropagatorConfig)
	}
	if cfg.BaggagePrefix == "" {
		cfg.BaggagePrefix = DefaultBaggageHeaderPrefix
	}
	if cfg.TraceHeader == "" {
		cfg.TraceHeader = DefaultTraceIDHeader
	}
	if cfg.ParentHeader == "" {
		cfg.ParentHeader = DefaultParentIDHeader
	}
	if cfg.PriorityHeader == "" {
		cfg.PriorityHeader = DefaultPriorityHeader
	}
	if cfg.BaggageHeader == "" {
		cfg.BaggageHeader = DefaultBaggageHeader
	}
	cp := new(chainedPropagator)
	cp.onlyExtractFirst = internal.BoolEnv("DD_TRACE_PROPAGATION_EXTRACT_FIRST", false)
	if len(propagators) > 0 {
		cp.injectors = propagators
		cp.extractors = propagators
		return cp
	}
	injectorsPs := os.Getenv(headerPropagationStyleInject)
	extractorsPs := os.Getenv(headerPropagationStyleExtract)
	cp.injectors, cp.injectorNames = getPropagators(cfg, injectorsPs)
	cp.extractors, cp.extractorsNames = getPropagators(cfg, extractorsPs)
	return cp
}

// chainedPropagator implements Propagator and applies a list of injectors and extractors.
// When injecting, all injectors are called to propagate the span context.
// When extracting, it tries each extractor, selecting the first successful one.
type chainedPropagator struct {
	injectors        []Propagator
	extractors       []Propagator
	injectorNames    string
	extractorsNames  string
	onlyExtractFirst bool // value of DD_TRACE_PROPAGATION_EXTRACT_FIRST
}

// getPropagators returns a list of propagators based on ps, which is a comma seperated
// list of propagators. If the list doesn't contain any valid values, the
// default propagator will be returned. Any invalid values in the list will log
// a warning and be ignored.
func getPropagators(cfg *PropagatorConfig, ps string) ([]Propagator, string) {
	dd := &propagator{cfg}
	defaultPs := []Propagator{dd, &propagatorW3c{}, &propagatorBaggage{}}
	defaultPsName := "datadog,tracecontext,baggage"
	if cfg.B3 {
		defaultPs = append(defaultPs, &propagatorB3{})
		defaultPsName += ",b3"
	}
	if ps == "" {
		if prop := getDDorOtelConfig("propagationStyle"); prop != "" {
			ps = prop // use the generic DD_TRACE_PROPAGATION_STYLE if set
		} else {
			return defaultPs, defaultPsName // no env set, so use default from configuration
		}
	}
	ps = strings.ToLower(ps)
	if ps == "none" {
		return nil, ""
	}
	var list []Propagator
	var listNames []string
	if cfg.B3 {
		list = append(list, &propagatorB3{})
		listNames = append(listNames, "b3")
	}
	for _, v := range strings.Split(ps, ",") {
		switch v := strings.ToLower(v); v {
		case "datadog":
			list = append(list, dd)
			listNames = append(listNames, v)
		case "tracecontext":
			list = append(list, &propagatorW3c{})
			listNames = append(listNames, v)
		case "baggage":
			list = append(list, &propagatorBaggage{})
			listNames = append(listNames, v)
		case "b3", "b3multi":
			if !cfg.B3 {
				// propagatorB3 hasn't already been added, add a new one.
				list = append(list, &propagatorB3{})
				listNames = append(listNames, v)
			}
		case "b3 single header":
			list = append(list, &propagatorB3SingleHeader{})
			listNames = append(listNames, v)
		case "none":
			log.Warn("Propagator \"none\" has no effect when combined with other propagators. " +
				"To disable the propagator, set to `none`")
		default:
			log.Warn("unrecognized propagator: %s\n", v)
		}
	}
	if len(list) == 0 {
		return defaultPs, defaultPsName // no valid propagators, so return default
	}
	return list, strings.Join(listNames, ",")
}

// Inject defines the Propagator to propagate SpanContext data
// out of the current process. The implementation propagates the
// TraceID and the current active SpanID, as well as the Span baggage.
func (p *chainedPropagator) Inject(spanCtx *SpanContext, carrier interface{}) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	for _, v := range p.injectors {
		err := v.Inject(spanCtx, carrier)
		if err != nil {
			return err
		}
	}
	return nil
}

// Extract implements Propagator. This method will attempt to extract a span context
// based on the precedence order of the propagators. Generally, the first valid
// trace context that could be extracted will be returned. However, the W3C tracestate
// header value will always be extracted and stored in the local trace context even if
// a previous propagator has succeeded so long as the trace-ids match.
// Furthermore, if we have already successfully extracted a trace context and a
// subsequent trace context has conflicting trace information, such information will
// be relayed in the returned SpanContext with a SpanLink.
func (p *chainedPropagator) Extract(carrier interface{}) (*SpanContext, error) {
	var ctx *SpanContext
	var links []SpanLink
	pendingBaggage := make(map[string]string) // used to store baggage items temporarily

	for _, v := range p.extractors {
		firstExtract := (ctx == nil) // ctx stores the most recently extracted ctx across iterations; if it's nil, no extractor has run yet
		extractedCtx, err := v.Extract(carrier)

		// If this is the baggage propagator, just stash its items into pendingBaggage
		if _, isBaggage := v.(*propagatorBaggage); isBaggage {
			if extractedCtx != nil && len(extractedCtx.baggage) > 0 {
				for k, v := range extractedCtx.baggage {
					pendingBaggage[k] = v
				}
			}
			continue
		}

		if firstExtract {
			if err != nil {
				if p.onlyExtractFirst { // Every error is relevant when we are relying on the first extractor
					return nil, err
				}
				if err != ErrSpanContextNotFound { // We don't care about ErrSpanContextNotFound because we could find a span context in a subsequent extractor
					return nil, err
				}
			}
			if p.onlyExtractFirst {
				return extractedCtx, nil
			}
			ctx = extractedCtx
		} else { // A local trace context has already been extracted
			extractedCtx2 := extractedCtx
			ctx2 := ctx

			// If we can't cast to spanContext, we can't propgate tracestate or create span links
			if extractedCtx2.TraceID() == ctx2.TraceID() {
				if pW3C, ok := v.(*propagatorW3c); ok {
					pW3C.propagateTracestate(ctx2, extractedCtx2)
					// If trace IDs match but span IDs do not, use spanID from `*propagatorW3c` extractedCtx for parenting
					if extractedCtx2.SpanID() != ctx2.SpanID() {
						var ddCtx *SpanContext
						// Grab the datadog-propagated spancontext again
						if ddp := getDatadogPropagator(p); ddp != nil {
							if ddSpanCtx, err := ddp.Extract(carrier); err == nil {
								ddCtx = ddSpanCtx
							}
						}
						overrideDatadogParentID(ctx2, extractedCtx2, ddCtx)
					}
				}
			} else if extractedCtx2 != nil { // Trace IDs do not match - create span links
				link := SpanLink{TraceID: extractedCtx2.TraceIDLower(), SpanID: extractedCtx2.SpanID(), TraceIDHigh: extractedCtx2.TraceIDUpper(), Attributes: map[string]string{"reason": "terminated_context", "context_headers": getPropagatorName(v)}}
				if trace := extractedCtx2.trace; trace != nil {
					if flags := uint32(*trace.priority); flags > 0 { // Set the flags based on the sampling priority
						link.Flags = 1
					} else {
						link.Flags = 0
					}
					link.Tracestate = extractedCtx2.trace.propagatingTag(tracestateHeader)
				}
				links = append(links, link)
			}
		}
	}

	if ctx == nil {
		if len(pendingBaggage) > 0 {
			ctx := &SpanContext{
				baggage:     make(map[string]string, len(pendingBaggage)),
				baggageOnly: true,
			}
			maps.Copy(ctx.baggage, pendingBaggage)
			atomic.StoreUint32(&ctx.hasBaggage, 1)
			return ctx, nil
		}
		// 0 successful extractions
		return nil, ErrSpanContextNotFound
	}
	if len(pendingBaggage) > 0 {
		if ctx.baggage == nil {
			ctx.baggage = make(map[string]string, len(pendingBaggage))
		}
		for k, v := range pendingBaggage {
			ctx.baggage[k] = v
		}
		atomic.StoreUint32(&ctx.hasBaggage, 1)
	}

	if len(links) > 0 {
		ctx.spanLinks = links
	}
	log.Debug("Extracted span context: %s", ctx.safeDebugString())
	return ctx, nil
}

func getPropagatorName(p Propagator) string {
	switch p.(type) {
	case *propagator:
		return "datadog"
	case *propagatorB3:
		return "b3multi"
	case *propagatorB3SingleHeader:
		return "b3"
	case *propagatorW3c:
		return "tracecontext"
	case *propagatorBaggage:
		return "baggage"
	default:
		return ""
	}
}

// propagateTracestate will add the tracestate propagating tag to the given
// *spanContext. The W3C trace context will be extracted from the provided
// carrier. The trace id of this W3C trace context must match the trace id
// provided by the given *spanContext. If it matches, then the tracestate
// will be re-composed based on the composition of the given *spanContext,
// but will include the non-DD vendors in the W3C trace context's tracestate.
func (p *propagatorW3c) propagateTracestate(ctx *SpanContext, w3cCtx *SpanContext) {
	if w3cCtx == nil {
		return // It's not valid, so ignore it.
	}
	if ctx.TraceID() != w3cCtx.TraceID() {
		return // The trace-ids must match.
	}
	if w3cCtx.trace == nil {
		return // this shouldn't happen, since it should have a propagating tag already
	}
	if ctx.trace == nil {
		ctx.trace = newTrace()
	}
	// Get the tracestate header from extracted w3C context, and propagate
	// it to the span context that will be returned.
	// Note: Other trace context fields like sampling priority, propagated tags,
	// and origin will remain unchanged.
	ts := w3cCtx.trace.propagatingTag(tracestateHeader)
	priority, _ := ctx.SamplingPriority()
	setPropagatingTag(ctx, tracestateHeader, composeTracestate(ctx, priority, ts))
	ctx.isRemote = (w3cCtx.isRemote)
}

// propagator implements Propagator and injects/extracts span contexts
// using datadog headers. Only TextMap carriers are supported.
type propagator struct {
	cfg *PropagatorConfig
}

func (p *propagator) Inject(spanCtx *SpanContext, carrier interface{}) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	switch c := carrier.(type) {
	case TextMapWriter:
		return p.injectTextMap(spanCtx, c)
	default:
		return ErrInvalidCarrier
	}
}

func (p *propagator) injectTextMap(spanCtx *SpanContext, writer TextMapWriter) error {
	ctx := spanCtx
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return ErrInvalidSpanContext
	}
	// propagate the TraceID and the current active SpanID
	if ctx.traceID.HasUpper() {
		setPropagatingTag(ctx, keyTraceID128, ctx.traceID.UpperHex())
	} else if ctx.trace != nil {
		ctx.trace.unsetPropagatingTag(keyTraceID128)
	}
	writer.Set(p.cfg.TraceHeader, strconv.FormatUint(ctx.traceID.Lower(), 10))
	writer.Set(p.cfg.ParentHeader, strconv.FormatUint(ctx.spanID, 10))
	if sp, ok := ctx.SamplingPriority(); ok {
		writer.Set(p.cfg.PriorityHeader, strconv.Itoa(sp))
	}
	if ctx.origin != "" {
		writer.Set(originHeader, ctx.origin)
	}
	ctx.ForeachBaggageItem(func(k, v string) bool {
		// Propagate OpenTracing baggage.
		writer.Set(p.cfg.BaggagePrefix+k, v)
		return true
	})
	if p.cfg.MaxTagsHeaderLen <= 0 {
		return nil
	}
	if s := p.marshalPropagatingTags(ctx); len(s) > 0 {
		writer.Set(traceTagsHeader, s)
	}
	return nil
}

// marshalPropagatingTags marshals all propagating tags included in ctx to a comma separated string
func (p *propagator) marshalPropagatingTags(ctx *SpanContext) string {
	var sb strings.Builder
	if ctx.trace == nil {
		return ""
	}

	var properr string
	ctx.trace.iteratePropagatingTags(func(k, v string) bool {
		if k == tracestateHeader || k == traceparentHeader {
			return true // don't propagate W3C headers with the DD propagator
		}
		if err := isValidPropagatableTag(k, v); err != nil {
			log.Warn("Won't propagate tag %q: %s", k, err.Error())
			properr = "encoding_error"
			return true
		}
		if tagLen := sb.Len() + len(k) + len(v); tagLen > p.cfg.MaxTagsHeaderLen {
			sb.Reset()
			log.Warn("Won't propagate tag: length is (%d) which exceeds the maximum len of (%d).", tagLen, p.cfg.MaxTagsHeaderLen)
			properr = "inject_max_size"
			return false
		}
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(v)
		return true
	})
	if properr != "" {
		ctx.trace.setTag(keyPropagationError, properr)
	}
	return sb.String()
}

func (p *propagator) Extract(carrier interface{}) (*SpanContext, error) {
	switch c := carrier.(type) {
	case TextMapReader:
		return p.extractTextMap(c)
	default:
		return nil, ErrInvalidCarrier
	}
}

func (p *propagator) extractTextMap(reader TextMapReader) (*SpanContext, error) {
	var ctx SpanContext
	err := reader.ForeachKey(func(k, v string) error {
		var err error
		key := strings.ToLower(k)
		switch key {
		case p.cfg.TraceHeader:
			var lowerTid uint64
			lowerTid, err = parseUint64(v)
			if err != nil {
				return ErrSpanContextCorrupted
			}
			ctx.traceID.SetLower(lowerTid)
		case p.cfg.ParentHeader:
			ctx.spanID, err = parseUint64(v)
			if err != nil {
				return ErrSpanContextCorrupted
			}
		case p.cfg.PriorityHeader:
			priority, err := strconv.Atoi(v)
			if err != nil {
				return ErrSpanContextCorrupted
			}
			ctx.setSamplingPriority(priority, samplernames.Unknown)
		case originHeader:
			ctx.origin = v
		case traceTagsHeader:
			unmarshalPropagatingTags(&ctx, v)
		default:
			if strings.HasPrefix(key, p.cfg.BaggagePrefix) {
				ctx.setBaggageItem(strings.TrimPrefix(key, p.cfg.BaggagePrefix), v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ctx.trace != nil {
		tid := ctx.trace.propagatingTag(keyTraceID128)
		if err := validateTID(tid); err != nil {
			log.Debug("Invalid hex traceID: %s", err.Error())
			ctx.trace.unsetPropagatingTag(keyTraceID128)
		} else if err := ctx.traceID.SetUpperFromHex(tid); err != nil {
			log.Debug("Attempted to set an invalid hex traceID: %s", err.Error())
			ctx.trace.unsetPropagatingTag(keyTraceID128)
		}
	}
	if ctx.traceID.Empty() || (ctx.spanID == 0 && ctx.origin != "synthetics") {
		return nil, ErrSpanContextNotFound
	}
	return &ctx, nil
}

func validateTID(tid string) error {
	if len(tid) != 16 {
		return fmt.Errorf("invalid length: %q", tid)
	}
	if !isValidID(tid) {
		return fmt.Errorf("malformed: %q", tid)
	}
	return nil
}

// getDatadogPropagator returns the Datadog Propagator
func getDatadogPropagator(cp *chainedPropagator) *propagator {
	for _, e := range cp.extractors {
		p, isDatadog := (e).(*propagator)
		if isDatadog {
			return p
		}
	}
	return nil
}

// overrideDatadogParentID overrides the span ID of a context with the ID extracted from tracecontext headers.
// If the reparenting ID is not set on the context, the span ID from datadog headers is used.
// spanContexts are passed by reference to avoid copying lock value in spanContext type
func overrideDatadogParentID(ctx, w3cCtx, ddCtx *SpanContext) {
	if ctx == nil || w3cCtx == nil || ddCtx == nil {
		return
	}
	ctx.spanID = w3cCtx.spanID
	if w3cCtx.reparentID != "" {
		ctx.reparentID = w3cCtx.reparentID
	} else {
		// NIT: could be done without using fmt.Sprintf? Is it worth it?
		ctx.reparentID = fmt.Sprintf("%016x", ddCtx.SpanID())
	}
}

// unmarshalPropagatingTags unmarshals tags from v into ctx
func unmarshalPropagatingTags(ctx *SpanContext, v string) {
	if ctx.trace == nil {
		ctx.trace = newTrace()
	}
	if len(v) > propagationExtractMaxSize {
		log.Warn("Did not extract %s, size limit exceeded: %d. Incoming tags will not be propagated further.", traceTagsHeader, propagationExtractMaxSize)
		ctx.trace.setTag(keyPropagationError, "extract_max_size")
		return
	}
	tags, err := parsePropagatableTraceTags(v)
	if err != nil {
		log.Warn("Did not extract %q: %s. Incoming tags will not be propagated further.", traceTagsHeader, err.Error())
		ctx.trace.setTag(keyPropagationError, "decoding_error")
	}
	ctx.trace.replacePropagatingTags(tags)
}

// setPropagatingTag adds the key value pair to the map of propagating tags on the trace,
// creating the map if one is not initialized.
func setPropagatingTag(ctx *SpanContext, k, v string) {
	if ctx.trace == nil {
		// extractors initialize a new spanContext, so the trace might be nil
		ctx.trace = newTrace()
	}
	ctx.trace.setPropagatingTag(k, v)
}

const (
	b3TraceIDHeader = "x-b3-traceid"
	b3SpanIDHeader  = "x-b3-spanid"
	b3SampledHeader = "x-b3-sampled"
	b3SingleHeader  = "b3"
)

// propagatorB3 implements Propagator and injects/extracts span contexts
// using B3 headers. Only TextMap carriers are supported.
type propagatorB3 struct{}

func (p *propagatorB3) Inject(spanCtx *SpanContext, carrier interface{}) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	switch c := carrier.(type) {
	case TextMapWriter:
		return p.injectTextMap(spanCtx, c)
	default:
		return ErrInvalidCarrier
	}
}

func (*propagatorB3) injectTextMap(spanCtx *SpanContext, writer TextMapWriter) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	ctx := spanCtx
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return ErrInvalidSpanContext
	}
	if !ctx.traceID.HasUpper() { // 64-bit trace id
		writer.Set(b3TraceIDHeader, fmt.Sprintf("%016x", ctx.traceID.Lower()))
	} else { // 128-bit trace id
		writer.Set(b3TraceIDHeader, ctx.TraceID())
	}
	writer.Set(b3SpanIDHeader, fmt.Sprintf("%016x", ctx.spanID))
	if p, ok := ctx.SamplingPriority(); ok {
		if p >= ext.PriorityAutoKeep {
			writer.Set(b3SampledHeader, "1")
		} else {
			writer.Set(b3SampledHeader, "0")
		}
	}
	return nil
}

func (p *propagatorB3) Extract(carrier interface{}) (*SpanContext, error) {
	switch c := carrier.(type) {
	case TextMapReader:
		return p.extractTextMap(c)
	default:
		return nil, ErrInvalidCarrier
	}
}

func (*propagatorB3) extractTextMap(reader TextMapReader) (*SpanContext, error) {
	var ctx SpanContext
	err := reader.ForeachKey(func(k, v string) error {
		var err error
		key := strings.ToLower(k)
		switch key {
		case b3TraceIDHeader:
			if err := extractTraceID128(&ctx, v); err != nil {
				return nil
			}
		case b3SpanIDHeader:
			ctx.spanID, err = strconv.ParseUint(v, 16, 64)
			if err != nil {
				return ErrSpanContextCorrupted
			}
		case b3SampledHeader:
			priority, err := strconv.Atoi(v)
			if err != nil {
				return ErrSpanContextCorrupted
			}
			ctx.setSamplingPriority(priority, samplernames.Unknown)
		default:
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return nil, ErrSpanContextNotFound
	}
	return &ctx, nil
}

// propagatorB3 implements Propagator and injects/extracts span contexts
// using B3 headers. Only TextMap carriers are supported.
type propagatorB3SingleHeader struct{}

func (p *propagatorB3SingleHeader) Inject(spanCtx *SpanContext, carrier interface{}) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	switch c := carrier.(type) {
	case TextMapWriter:
		return p.injectTextMap(spanCtx, c)
	default:
		return ErrInvalidCarrier
	}
}

func (*propagatorB3SingleHeader) injectTextMap(spanCtx *SpanContext, writer TextMapWriter) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	ctx := spanCtx
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return ErrInvalidSpanContext
	}
	sb := strings.Builder{}
	var traceID string
	if !ctx.traceID.HasUpper() { // 64-bit trace id
		traceID = fmt.Sprintf("%016x", ctx.traceID.Lower())
	} else { // 128-bit trace id
		traceID = ctx.TraceID()
	}
	sb.WriteString(fmt.Sprintf("%s-%016x", traceID, ctx.spanID))
	if p, ok := ctx.SamplingPriority(); ok {
		if p >= ext.PriorityAutoKeep {
			sb.WriteString("-1")
		} else {
			sb.WriteString("-0")
		}
	}
	writer.Set(b3SingleHeader, sb.String())
	return nil
}

func (p *propagatorB3SingleHeader) Extract(carrier interface{}) (*SpanContext, error) {
	switch c := carrier.(type) {
	case TextMapReader:
		return p.extractTextMap(c)
	default:
		return nil, ErrInvalidCarrier
	}
}

func (*propagatorB3SingleHeader) extractTextMap(reader TextMapReader) (*SpanContext, error) {
	var ctx SpanContext
	err := reader.ForeachKey(func(k, v string) error {
		var err error
		key := strings.ToLower(k)
		switch key {
		case b3SingleHeader:
			b3Parts := strings.Split(v, "-")
			if len(b3Parts) >= 2 {
				if err = extractTraceID128(&ctx, b3Parts[0]); err != nil {
					return err
				}
				ctx.spanID, err = strconv.ParseUint(b3Parts[1], 16, 64)
				if err != nil {
					return ErrSpanContextCorrupted
				}
				if len(b3Parts) >= 3 {
					switch b3Parts[2] {
					case "":
						break
					case "1", "d": // Treat 'debug' traces as priority 1
						ctx.setSamplingPriority(ext.PriorityAutoKeep, samplernames.Unknown)
					case "0":
						ctx.setSamplingPriority(ext.PriorityAutoReject, samplernames.Unknown)
					default:
						return ErrSpanContextCorrupted
					}
				}
			} else {
				return ErrSpanContextCorrupted
			}
		default:
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return nil, ErrSpanContextNotFound
	}
	return &ctx, nil
}

const (
	traceparentHeader = "traceparent"
	tracestateHeader  = "tracestate"
)

// propagatorW3c implements Propagator and injects/extracts span contexts
// using W3C tracecontext/traceparent headers. Only TextMap carriers are supported.
type propagatorW3c struct{}

func (p *propagatorW3c) Inject(spanCtx *SpanContext, carrier interface{}) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	switch c := carrier.(type) {
	case TextMapWriter:
		return p.injectTextMap(spanCtx, c)
	default:
		return ErrInvalidCarrier
	}
}

// injectTextMap propagates span context attributes into the writer,
// in the format of the traceparentHeader and tracestateHeader.
// traceparentHeader encodes W3C Trace Propagation version, 128-bit traceID,
// spanID, and a flags field, which supports 8 unique flags.
// The current specification only supports a single flag called sampled,
// which is equal to 00000001 when no other flag is present.
// tracestateHeader is a comma-separated list of list-members with a <key>=<value> format,
// where each list-member is managed by a vendor or instrumentation library.
func (*propagatorW3c) injectTextMap(spanCtx *SpanContext, writer TextMapWriter) error {
	if spanCtx == nil {
		return ErrInvalidSpanContext
	}
	ctx := spanCtx
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return ErrInvalidSpanContext
	}
	flags := ""
	p, ok := ctx.SamplingPriority()
	if ok && p >= ext.PriorityAutoKeep {
		flags = "01"
	} else {
		flags = "00"
	}

	var traceID string
	if ctx.traceID.HasUpper() {
		setPropagatingTag(ctx, keyTraceID128, ctx.traceID.UpperHex())
		traceID = ctx.TraceID()
	} else {
		traceID = fmt.Sprintf("%032x", ctx.traceID)
		if ctx.trace != nil {
			ctx.trace.unsetPropagatingTag(keyTraceID128)
		}
	}
	writer.Set(traceparentHeader, fmt.Sprintf("00-%s-%016x-%v", traceID, ctx.spanID, flags))
	// if context priority / origin / tags were updated after extraction,
	// or if there is a span on the trace
	// or the tracestateHeader doesn't start with `dd=`
	// we need to recreate tracestate
	if ctx.updated ||
		(!ctx.isRemote || ctx.isRemote && ctx.trace != nil && ctx.trace.root != nil) ||
		(ctx.trace != nil && !strings.HasPrefix(ctx.trace.propagatingTag(tracestateHeader), "dd=")) ||
		ctx.trace.propagatingTagsLen() == 0 {
		// compose a new value for the tracestate
		writer.Set(tracestateHeader, composeTracestate(ctx, p, ctx.trace.propagatingTag(tracestateHeader)))
	} else {
		// use a cached value for the tracestate (e.g., no updating p: key)
		writer.Set(tracestateHeader, ctx.trace.propagatingTag(tracestateHeader))
	}
	return nil
}

// stringMutator maps characters in a string to new characters. It is a state machine intended
// to replace regex patterns for simple character replacement, including collapsing runs of a
// specific range.
//
// It's designed after the `hash#Hash` interface, and to work with `strings.Map`.
type stringMutator struct {
	// n is the current state of the mutator. It is used to track runs of characters that should
	// be collapsed.
	n bool
	// fn is the function that implements the character replacement logic.
	// It returns the rune to use as replacement and a bool to tell if next consecutive
	// characters must be dropped if they fall in the currently matched character set.
	// It's possible to return `-1` to immediately drop the current rune.
	//
	// This logic allows for:
	// - Replace only the current rune: return <new value>, false
	// - Drop only the current rune: return -1, false
	// - Replace the current rune and drop the next consecutive runes if they match the same case: return <new value>, true
	// - Drop all the consecutive runes matching the same case as the current one: return -1, true
	//
	// A known limitation is that we can only support a single case of consecutive runes.
	fn func(rune) (rune, bool)
}

// Mutate the mapped string using `strings.Map` and the provided function implementing the character
// replacement logic.
func (sm *stringMutator) Mutate(fn func(rune) (rune, bool), s string) string {
	sm.fn = fn
	rs := strings.Map(sm.mapping, s)
	sm.reset()

	return rs
}

func (sm *stringMutator) mapping(r rune) rune {
	v, dropConsecutiveMatches := sm.fn(r)
	if v < 0 {
		// We reset the state machine in any match that is not related to a consecutive run
		sm.reset()
		return -1
	}
	if dropConsecutiveMatches {
		if !sm.n {
			sm.n = true
			return v
		}
		return -1
	}
	// We reset the state machine in any match that is not related to a consecutive run
	sm.reset()
	return v
}

// reset resets the state of the mutator.
func (sm *stringMutator) reset() {
	sm.n = false
}

var (
	// keyDisallowedFn is used to sanitize the keys of the datadog propagating tags.
	// Disallowed characters are comma (reserved as a list-member separator),
	// equals (reserved for list-member key-value separator),
	// space and characters outside the ASCII range 0x20 to 0x7E.
	// Disallowed characters must be replaced with the underscore.
	// Equivalent to regexp.MustCompile(",|=|[^\\x20-\\x7E]+")
	keyDisallowedFn = func(r rune) (rune, bool) {
		switch {
		case r == ',' || r == '=':
			return '_', false
		case r < 0x20 || r > 0x7E:
			return '_', true
		}
		return r, false
	}

	// valueDisallowedFn is used to sanitize the values of the datadog propagating tags.
	// Disallowed characters are comma (reserved as a list-member separator),
	// semi-colon (reserved for separator between entries in the dd list-member),
	// tilde (reserved, will represent 0x3D (equals) in the encoded tag value,
	// and characters outside the ASCII range 0x20 to 0x7E.
	// Equals character must be encoded with a tilde.
	// Other disallowed characters must be replaced with the underscore.
	// Equivalent to regexp.MustCompile(",|;|~|[^\\x20-\\x7E]+")
	valueDisallowedFn = func(r rune) (rune, bool) {
		switch {
		case r == '=':
			return '~', false
		case r == ',' || r == '~' || r == ';':
			return '_', false
		case r < 0x20 || r > 0x7E:
			return '_', true
		}
		return r, false
	}

	// originDisallowedFn is used to sanitize the value of the datadog origin tag.
	// Disallowed characters are comma (reserved as a list-member separator),
	// semi-colon (reserved for separator between entries in the dd list-member),
	// equals (reserved for list-member key-value separator),
	// and characters outside the ASCII range 0x21 to 0x7E.
	// Equals character must be encoded with a tilde.
	// Other disallowed characters must be replaced with the underscore.
	// Equivalent to regexp.MustCompile(",|~|;|[^\\x21-\\x7E]+")
	originDisallowedFn = func(r rune) (rune, bool) {
		switch {
		case r == '=':
			return '~', false
		case r == ',' || r == '~' || r == ';':
			return '_', false
		case r < 0x21 || r > 0x7E:
			return '_', true
		}
		return r, false
	}
)

const (
	asciiLowerA = 97
	asciiLowerF = 102
	asciiZero   = 48
	asciiNine   = 57
)

// isValidID is used to verify that the input is a valid hex string.
// This is an equivalent check to the regexp ^[a-f0-9]+$
// In benchmarks, this function is roughly 10x faster than the equivalent
// regexp, which is why we split it out.
// isValidID is applicable for both trace and span IDs.
func isValidID(id string) bool {
	if len(id) == 0 {
		return false
	}

	for _, c := range id {
		ascii := int(c)
		if ascii < asciiZero || ascii > asciiLowerF || (ascii > asciiNine && ascii < asciiLowerA) {
			return false
		}
	}

	return true
}

// composeTracestate creates a tracestateHeader from the spancontext.
// The Datadog tracing library is only responsible for managing the list member with key dd,
// which holds the values of the sampling decision(`s:<value>`), origin(`o:<origin>`),
// the last parent ID of a Datadog span (`p:<parent_id>`),
// and propagated tags prefixed with `t.`(e.g. _dd.p.usr.id:usr_id tag will become `t.usr.id:usr_id`).
func composeTracestate(ctx *SpanContext, priority int, oldState string) string {
	var (
		b  strings.Builder
		sm = &stringMutator{}
	)

	b.Grow(128)
	b.WriteString("dd=s:")
	b.WriteString(strconv.Itoa(priority))
	listLength := 1

	if ctx.origin != "" {
		oWithSub := sm.Mutate(originDisallowedFn, ctx.origin)
		b.WriteString(";o:")
		b.WriteString(oWithSub)
	}

	// if the context is remote and there is a reparentID, set p as reparentId
	// if the context is remote and there is no reparentID, don't set p
	// if the context is not remote, set p as context.spanId
	// this ID can be used by downstream tracers to set a _dd.parent_id tag
	// to allow the backend to reparent orphaned spans if necessary
	if !ctx.isRemote {
		b.WriteString(";p:")
		b.WriteString(spanIDHexEncoded(ctx.SpanID(), 16))
	} else if ctx.reparentID != "" {
		b.WriteString(";p:")
		b.WriteString(ctx.reparentID)
	}

	ctx.trace.iteratePropagatingTags(func(k, v string) bool {
		if !strings.HasPrefix(k, "_dd.p.") {
			return true
		}
		// Datadog propagating tags must be appended to the tracestateHeader
		// with the `t.` prefix. Tag value must have all `=` signs replaced with a tilde (`~`).
		key := sm.Mutate(keyDisallowedFn, k[len("_dd.p."):])
		value := sm.Mutate(valueDisallowedFn, v)
		if b.Len()+len(key)+len(value)+4 > 256 { // the +4 here is to account for the `t.` prefix, the `;` needed between the tags, and the `:` between the key and value
			return false
		}
		b.WriteString(";t.")
		b.WriteString(key)
		b.WriteString(":")
		b.WriteString(value)
		return true
	})
	// the old state is split by vendors, must be concatenated with a `,`
	if len(oldState) == 0 {
		return b.String()
	}
	for _, s := range strings.Split(strings.Trim(oldState, " \t"), ",") {
		if strings.HasPrefix(s, "dd=") {
			continue
		}
		listLength++
		// if the resulting tracestateHeader exceeds 32 list-members,
		// remove the rightmost list-member(s)
		if listLength > 32 {
			break
		}
		b.WriteString(",")
		b.WriteString(strings.Trim(s, " \t"))
	}
	return b.String()
}

func (p *propagatorW3c) Extract(carrier interface{}) (*SpanContext, error) {
	switch c := carrier.(type) {
	case TextMapReader:
		return p.extractTextMap(c)
	default:
		return nil, ErrInvalidCarrier
	}
}

func (*propagatorW3c) extractTextMap(reader TextMapReader) (*SpanContext, error) {
	var parentHeader string
	var stateHeader string
	var ctx SpanContext
	ctx.isRemote = true
	// to avoid parsing tracestate header(s) if traceparent is invalid
	if err := reader.ForeachKey(func(k, v string) error {
		key := strings.ToLower(k)
		switch key {
		case traceparentHeader:
			if parentHeader != "" {
				return ErrSpanContextCorrupted
			}
			parentHeader = v
		case tracestateHeader:
			stateHeader = v
		default:
			if strings.HasPrefix(key, DefaultBaggageHeaderPrefix) {
				ctx.setBaggageItem(strings.TrimPrefix(key, DefaultBaggageHeaderPrefix), v)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := parseTraceparent(&ctx, parentHeader); err != nil {
		return nil, err
	}
	parseTracestate(&ctx, stateHeader)
	return &ctx, nil
}

// parseTraceparent attempts to parse traceparentHeader which describes the position
// of the incoming request in its trace graph in a portable, fixed-length format.
// The format of the traceparentHeader is `-` separated string with in the
// following format: `version-traceId-spanID-flags`, with an optional `-<prefix>` if version > 0.
// where:
// - version - represents the version of the W3C Tracecontext Propagation format in hex format.
// - traceId - represents the propagated traceID in the format of 32 hex-encoded digits.
// - spanID - represents the propagated spanID (parentID) in the format of 16 hex-encoded digits.
// - flags - represents the propagated flags in the format of 2 hex-encoded digits, and supports 8 unique flags.
// Example value of HTTP `traceparent` header: `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`,
// Currently, Go tracer doesn't support 128-bit traceIDs, so the full traceID (32 hex-encoded digits) must be
// stored into a field that is accessible from the span's context. TraceId will be parsed from the least significant 16
// hex-encoded digits into a 64-bit number.
func parseTraceparent(ctx *SpanContext, header string) error {
	nonWordCutset := "_-\t \n"
	header = strings.ToLower(strings.Trim(header, "\t -"))
	headerLen := len(header)
	if headerLen == 0 {
		return ErrSpanContextNotFound
	}
	if headerLen < 55 {
		return ErrSpanContextCorrupted
	}
	parts := strings.SplitN(header, "-", 5) // 5 because we expect 4 required + 1 optional substrings
	if len(parts) < 4 {
		return ErrSpanContextCorrupted
	}
	version := strings.Trim(parts[0], nonWordCutset)
	if len(version) != 2 {
		return ErrSpanContextCorrupted
	}
	v, err := strconv.ParseUint(version, 16, 64)
	if err != nil || v == 255 {
		// version 255 (0xff) is invalid
		return ErrSpanContextCorrupted
	}
	if v == 0 && headerLen != 55 {
		// The header length in v0 has to be 55.
		// It's allowed to be longer in other versions.
		return ErrSpanContextCorrupted
	}
	// parsing traceID
	fullTraceID := strings.Trim(parts[1], nonWordCutset)
	if len(fullTraceID) != 32 {
		return ErrSpanContextCorrupted
	}
	// checking that the entire TraceID is a valid hex string
	if !isValidID(fullTraceID) {
		return ErrSpanContextCorrupted
	}
	if ctx.trace != nil {
		// Ensure that the 128-bit trace id tag doesn't propagate
		ctx.trace.unsetPropagatingTag(keyTraceID128)
	}
	if err := extractTraceID128(ctx, fullTraceID); err != nil {
		return err
	}
	// parsing spanID
	spanID := strings.Trim(parts[2], nonWordCutset)
	if len(spanID) != 16 {
		return ErrSpanContextCorrupted
	}
	if !isValidID(spanID) {
		return ErrSpanContextCorrupted
	}
	if ctx.spanID, err = strconv.ParseUint(spanID, 16, 64); err != nil {
		return ErrSpanContextCorrupted
	}
	if ctx.spanID == 0 {
		return ErrSpanContextNotFound
	}
	// parsing flags
	flags := parts[3]
	f, err := strconv.ParseInt(flags, 16, 8)
	if err != nil {
		return ErrSpanContextCorrupted
	}
	ctx.setSamplingPriority(int(f)&0x1, samplernames.Unknown)
	return nil
}

// parseTracestate attempts to parse tracestateHeader which is a list
// with up to 32 comma-separated (,) list-members.
// An example value would be: `vendorname1=opaqueValue1,vendorname2=opaqueValue2,dd=s:1;o:synthetics`,
// Where `dd` list contains values that would be in x-datadog-tags as well as those needed for propagation information.
// The keys to the "dd" values have been shortened as follows to save space:
// `sampling_priority` = `s`
// `origin` = `o`
// `last parent` = `p`
// `_dd.p.` prefix = `t.`
func parseTracestate(ctx *SpanContext, header string) {
	if header == "" {
		// The W3C spec says tracestate can be empty but should avoid sending it.
		// https://www.w3.org/TR/trace-context-1/#tracestate-header-field-values
		return
	}
	// if multiple headers are present, they must be combined and stored
	setPropagatingTag(ctx, tracestateHeader, header)
	combined := strings.Split(strings.Trim(header, "\t "), ",")
	for _, group := range combined {
		if !strings.HasPrefix(group, "dd=") {
			continue
		}
		ddMembers := strings.Split(group[len("dd="):], ";")
		dropDM := false
		// indicate that backend could reparent this as a root
		for _, member := range ddMembers {
			keyVal := strings.SplitN(member, ":", 2)
			if len(keyVal) != 2 {
				continue
			}
			key, val := keyVal[0], keyVal[1]
			if key == "o" {
				ctx.origin = strings.ReplaceAll(val, "~", "=")
			} else if key == "s" {
				stateP, err := strconv.Atoi(val)
				if err != nil {
					// If the tracestate priority is absent,
					// we rely on the traceparent sampled flag
					// set in the parseTraceparent function.
					continue
				}
				// The sampling priority and decision maker values are set based on
				// the specification in the internal W3C context propagation RFC.
				// See the document for more details.
				parentP, _ := ctx.SamplingPriority()
				if (parentP == 1 && stateP > 0) || (parentP == 0 && stateP <= 0) {
					// As extracted from tracestate
					ctx.setSamplingPriority(stateP, samplernames.Unknown)
				}
				if parentP == 1 && stateP <= 0 {
					// Auto keep (1) and set the decision maker to default
					ctx.setSamplingPriority(ext.PriorityAutoKeep, samplernames.Default)
				}
				if parentP == 0 && stateP > 0 {
					// Auto drop (0) and drop the decision maker
					ctx.setSamplingPriority(ext.PriorityAutoReject, samplernames.Unknown)
					dropDM = true
				}
			} else if key == "p" {
				ctx.reparentID = val
			} else if strings.HasPrefix(key, "t.dm") {
				if ctx.trace.hasPropagatingTag(keyDecisionMaker) || dropDM {
					continue
				}
				setPropagatingTag(ctx, keyDecisionMaker, val)
			} else if strings.HasPrefix(key, "t.") {
				keySuffix := key[len("t."):]
				val = strings.ReplaceAll(val, "~", "=")
				setPropagatingTag(ctx, "_dd.p."+keySuffix, val)
			}
		}
	}
}

// extractTraceID128 extracts the trace id from v and populates the traceID
// field, and the traceID128 field (if applicable) of the provided ctx,
// returning an error if v is invalid.
func extractTraceID128(ctx *SpanContext, v string) error {
	if len(v) > 32 {
		v = v[len(v)-32:]
	}
	v = strings.TrimLeft(v, "0")
	var err error
	if len(v) <= 16 { // 64-bit trace id
		var tid uint64
		tid, err = strconv.ParseUint(v, 16, 64)
		ctx.traceID.SetLower(tid)
	} else { // 128-bit trace id
		idUpper := v[:len(v)-16]
		ctx.traceID.SetUpperFromHex(idUpper)
		var l uint64
		l, err = strconv.ParseUint(v[len(idUpper):], 16, 64)
		ctx.traceID.SetLower(l)
	}
	if err != nil {
		return ErrSpanContextCorrupted
	}
	return nil
}

const (
	baggageMaxItems     = 64
	baggageMaxBytes     = 8192
	safeCharactersKey   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#$%&'*+-.^_`|~"
	safeCharactersValue = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#$%&'()*+-./:<>?@[]^_`{|}~"
)

// encodeKey encodes a key with the specified safe characters
func encodeKey(key string) string {
	return urlEncode(strings.TrimSpace(key), safeCharactersKey)
}

// encodeValue encodes a value with the specified safe characters
func encodeValue(value string) string {
	return urlEncode(strings.TrimSpace(value), safeCharactersValue)
}

// urlEncode performs percent-encoding while respecting the safe characters
func urlEncode(input string, safeCharacters string) string {
	var encoded strings.Builder
	for _, c := range input {
		if strings.ContainsRune(safeCharacters, c) {
			encoded.WriteRune(c)
		} else {
			encoded.WriteString(url.QueryEscape(string(c)))
		}
	}
	return encoded.String()
}

// propagatorBaggage implements Propagator and injects/extracts span contexts
// using baggage headers.
type propagatorBaggage struct{}

func (p *propagatorBaggage) Inject(spanCtx *SpanContext, carrier interface{}) error {
	switch c := carrier.(type) {
	case TextMapWriter:
		return p.injectTextMap(spanCtx, c)
	default:
		return ErrInvalidCarrier
	}
}

// injectTextMap propagates baggage items from the span context into the writer,
// in the format of a single HTTP "baggage" header. Baggage consists of key=value pairs,
// separated by commas. This function enforces a maximum number of baggage items and a maximum overall size.
// If either limit is exceeded, excess items or bytes are dropped, and a warning is logged.
//
// Example of a single "baggage" header:
// baggage: foo=bar,baz=qux
//
// Each key and value pair is encoded and added to the existing baggage header in <key>=<value> format,
// joined together by commas,
func (*propagatorBaggage) injectTextMap(ctx *SpanContext, writer TextMapWriter) error {
	if ctx == nil {
		return nil
	}

	ctr := 0
	var baggageBuilder strings.Builder
	ctx.ForeachBaggageItem(func(k, v string) bool {
		if ctr >= baggageMaxItems {
			return false
		}

		var itemBuilder strings.Builder
		if ctr > 0 {
			itemBuilder.WriteRune(',')
		}

		itemBuilder.WriteString(encodeKey(k))
		itemBuilder.WriteRune('=')
		itemBuilder.WriteString(encodeValue(v))
		if itemBuilder.Len()+baggageBuilder.Len() > baggageMaxBytes {
			return false
		}
		baggageBuilder.WriteString(itemBuilder.String())
		ctr++
		return true
	})
	if baggageBuilder.Len() > 0 {
		writer.Set("baggage", baggageBuilder.String())
	}
	return nil
}

func (p *propagatorBaggage) Extract(carrier interface{}) (*SpanContext, error) {
	switch c := carrier.(type) {
	case TextMapReader:
		return p.extractTextMap(c)
	default:
		return nil, ErrInvalidCarrier
	}
}

func (*propagatorBaggage) extractTextMap(reader TextMapReader) (*SpanContext, error) {
	var baggageHeader string
	var ctx SpanContext
	err := reader.ForeachKey(func(k, v string) error {
		if strings.ToLower(k) == "baggage" {
			// Expect only one baggage header, return early
			baggageHeader = v
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if baggageHeader == "" {
		return &ctx, nil
	}

	parts := strings.Split(baggageHeader, ",")

	// 1) validation & single-trim pass
	for i, kv := range parts {
		k, v, ok := strings.Cut(kv, "=")
		trimmedK := strings.TrimSpace(k)
		trimmedV := strings.TrimSpace(v)
		if !ok || trimmedK == "" || trimmedV == "" {
			log.Warn("invalid baggage item: %q, dropping entire header", kv)
			return &ctx, nil
		}
		// store back the trimmed pair so we don't re-trim below
		parts[i] = trimmedK + "=" + trimmedV
	}

	// 2) safe to URL-decode & apply
	for _, kv := range parts {
		rawK, rawV, _ := strings.Cut(kv, "=")
		key, _ := url.QueryUnescape(rawK)
		val, _ := url.QueryUnescape(rawV)
		ctx.setBaggageItem(key, val)
	}

	return &ctx, nil
}
