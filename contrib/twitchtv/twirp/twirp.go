// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package twirp provides tracing functions for tracing clients and servers generated
// by the twirp framework (https://github.com/twitchtv/twirp).
package twirp // import "github.com/DataDog/dd-trace-go/contrib/twitchtv/twirp/v2"

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation"

	"github.com/twitchtv/twirp"
)

const component = instrumentation.PackageTwitchTVTwirp

var instr *instrumentation.Instrumentation

func init() {
	instr = instrumentation.Load(component)
}

type (
	twirpErrorKey struct{}
	twirpSpanKey  struct{}
)

// HTTPClient is duplicated from twirp's generated service code.
// It is declared in this package so that the client can be wrapped
// to initiate traces.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type wrappedClient struct {
	c   HTTPClient
	cfg *config
}

// WrapClient wraps an HTTPClient to add distributed tracing to its requests.
func WrapClient(c HTTPClient, opts ...Option) HTTPClient {
	cfg := new(config)
	clientDefaults(cfg)
	for _, fn := range opts {
		fn.apply(cfg)
	}
	instr.Logger().Debug("contrib/twitchtv/twirp: Wrapping Client: %#v", cfg)
	return &wrappedClient{c: c, cfg: cfg}
}

func (wc *wrappedClient) Do(req *http.Request) (*http.Response, error) {
	opts := []tracer.StartSpanOption{
		tracer.SpanType(ext.SpanTypeHTTP),
		tracer.ServiceName(wc.cfg.serviceName),
		tracer.Tag(ext.HTTPMethod, req.Method),
		tracer.Tag(ext.HTTPURL, req.URL.Path),
		tracer.Tag(ext.Component, component),
		tracer.Tag(ext.SpanKind, ext.SpanKindClient),
		tracer.Tag(ext.RPCSystem, ext.RPCSystemTwirp),
	}
	ctx := req.Context()
	if pkg, ok := twirp.PackageName(ctx); ok {
		opts = append(opts, tracer.Tag("twirp.package", pkg))
	}
	if svc, ok := twirp.ServiceName(ctx); ok {
		opts = append(
			opts,
			tracer.Tag("twirp.service", svc),
			tracer.Tag(ext.RPCService, svc),
		)
	}
	if method, ok := twirp.MethodName(ctx); ok {
		opts = append(
			opts,
			tracer.Tag("twirp.method", method),
			tracer.Tag(ext.RPCMethod, method),
		)
	}
	if !math.IsNaN(wc.cfg.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, wc.cfg.analyticsRate))
	}
	if spanctx, err := tracer.Extract(tracer.HTTPHeadersCarrier(req.Header)); err == nil {
		// If there are span links as a result of context extraction, add them as a StartSpanOption
		if spanctx != nil && spanctx.SpanLinks() != nil {
			opts = append(opts, tracer.WithSpanLinks(spanctx.SpanLinks()))
		}
		opts = append(opts, tracer.ChildOf(spanctx))
	}

	span, ctx := tracer.StartSpanFromContext(req.Context(), wc.cfg.spanName, opts...)
	defer span.Finish()

	err := tracer.Inject(span.Context(), tracer.HTTPHeadersCarrier(req.Header))
	if err != nil {
		instr.Logger().Warn("contrib/twitchtv/twirp.wrappedClient: failed to inject http headers: %s\n", err.Error())
	}

	req = req.WithContext(ctx)
	res, err := wc.c.Do(req)
	if err != nil {
		span.SetTag(ext.Error, err)
	} else {
		span.SetTag(ext.HTTPCode, strconv.Itoa(res.StatusCode))
		// treat 4XX and 5XX as errors for a client
		if res.StatusCode >= 400 {
			span.SetTag(ext.Error, true)
			span.SetTag(ext.ErrorMsg, fmt.Sprintf("%d: %s", res.StatusCode, http.StatusText(res.StatusCode)))
		}
	}
	return res, err
}

// WrapServer wraps an http.Handler to add distributed tracing to a Twirp server.
func WrapServer(h http.Handler, opts ...Option) http.Handler {
	cfg := new(config)
	serverDefaults(cfg)
	for _, fn := range opts {
		fn.apply(cfg)
	}
	instr.Logger().Debug("contrib/twitchtv/twirp: Wrapping Server: %#v", cfg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spanOpts := []tracer.StartSpanOption{
			tracer.SpanType(ext.SpanTypeWeb),
			tracer.ServiceName(cfg.serviceName),
			tracer.Tag(ext.HTTPMethod, r.Method),
			tracer.Tag(ext.HTTPURL, r.URL.Path),
			tracer.Tag(ext.Component, component),
			tracer.Tag(ext.SpanKind, ext.SpanKindServer),
			tracer.Tag(ext.RPCSystem, ext.RPCSystemTwirp),
			tracer.Measured(),
		}
		if !math.IsNaN(cfg.analyticsRate) {
			spanOpts = append(spanOpts, tracer.Tag(ext.EventSampleRate, cfg.analyticsRate))
		}
		if spanctx, err := tracer.Extract(tracer.HTTPHeadersCarrier(r.Header)); err == nil {
			// If there are span links as a result of context extraction, add them as a StartSpanOption
			if spanctx != nil && spanctx.SpanLinks() != nil {
				spanOpts = append(spanOpts, tracer.WithSpanLinks(spanctx.SpanLinks()))
			}
			spanOpts = append(spanOpts, tracer.ChildOf(spanctx))
		}
		span, ctx := tracer.StartSpanFromContext(r.Context(), "twirp.handler", spanOpts...)
		defer span.Finish()

		r = r.WithContext(ctx)
		h.ServeHTTP(w, r)
	})
}

// NewServerHooks creates the callback hooks for a twirp server to perform tracing.
// It is used in conjunction with WrapServer.
func NewServerHooks(opts ...Option) *twirp.ServerHooks {
	cfg := new(config)
	serverDefaults(cfg)
	for _, fn := range opts {
		fn.apply(cfg)
	}
	instr.Logger().Debug("contrib/twitchtv/twirp: Creating Server Hooks: %#v", cfg)
	return &twirp.ServerHooks{
		RequestReceived:  requestReceivedHook(cfg),
		RequestRouted:    requestRoutedHook(),
		ResponsePrepared: responsePreparedHook(),
		ResponseSent:     responseSentHook(),
		Error:            errorHook(),
	}
}

func serverSpanName(ctx context.Context) string {
	rpcService := ""
	if svc, ok := twirp.ServiceName(ctx); ok {
		rpcService = svc
	}
	return instr.OperationName(
		instrumentation.ComponentServer,
		instrumentation.OperationContext{
			ext.RPCService: rpcService,
		},
	)
}

func requestReceivedHook(cfg *config) func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) {
		opts := []tracer.StartSpanOption{
			tracer.SpanType(ext.SpanTypeWeb),
			tracer.ServiceName(cfg.serviceName),
			tracer.Measured(),
			tracer.Tag(ext.Component, component),
			tracer.Tag(ext.RPCSystem, ext.RPCSystemTwirp),
		}
		if pkg, ok := twirp.PackageName(ctx); ok {
			opts = append(opts, tracer.Tag("twirp.package", pkg))
		}
		if svc, ok := twirp.ServiceName(ctx); ok {
			opts = append(
				opts,
				tracer.Tag("twirp.service", svc),
				tracer.Tag(ext.RPCService, svc),
			)
		}
		if !math.IsNaN(cfg.analyticsRate) {
			opts = append(opts, tracer.Tag(ext.EventSampleRate, cfg.analyticsRate))
		}
		span, ctx := tracer.StartSpanFromContext(ctx, serverSpanName(ctx), opts...)
		ctx = context.WithValue(ctx, twirpSpanKey{}, span)
		return ctx, nil
	}
}

func requestRoutedHook() func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) {
		maybeSpan := ctx.Value(twirpSpanKey{})
		if maybeSpan == nil {
			instr.Logger().Error("contrib/twitchtv/twirp.requestRoutedHook: found no span in context")
			return ctx, nil
		}
		span, ok := maybeSpan.(*tracer.Span)
		if !ok {
			instr.Logger().Error("contrib/twitchtv/twirp.requestRoutedHook: found invalid span type in context")
			return ctx, nil
		}
		if method, ok := twirp.MethodName(ctx); ok {
			span.SetTag(ext.ResourceName, method)
			span.SetTag("twirp.method", method)
			span.SetTag(ext.RPCMethod, method)
		}
		return ctx, nil
	}
}

func responsePreparedHook() func(context.Context) context.Context {
	return func(ctx context.Context) context.Context {
		return ctx
	}
}

func responseSentHook() func(context.Context) {
	return func(ctx context.Context) {
		maybeSpan := ctx.Value(twirpSpanKey{})
		if maybeSpan == nil {
			return
		}
		span, ok := maybeSpan.(*tracer.Span)
		if !ok {
			return
		}
		if sc, ok := twirp.StatusCode(ctx); ok {
			span.SetTag(ext.HTTPCode, sc)
		}
		err, _ := ctx.Value(twirpErrorKey{}).(twirp.Error)
		span.Finish(tracer.WithError(err))
	}
}

func errorHook() func(context.Context, twirp.Error) context.Context {
	return func(ctx context.Context, err twirp.Error) context.Context {
		return context.WithValue(ctx, twirpErrorKey{}, err)
	}
}
