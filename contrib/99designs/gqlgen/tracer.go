// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

// Package gqlgen contains an implementation of a gqlgen tracer, and functions
// to construct and configure the tracer. The tracer can be passed to the gqlgen
// handler (see package github.com/99designs/gqlgen/handler)
//
// Warning: Data obfuscation hasn't been implemented for graphql queries yet,
// any sensitive data in the query will be sent to Datadog as the resource name
// of the span. To ensure no sensitive data is included in your spans, always
// use parameterized graphql queries with sensitive data in variables.
package gqlgen

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation"
	"github.com/DataDog/dd-trace-go/v2/instrumentation/appsec/emitter/graphqlsec"
	instrgraphql "github.com/DataDog/dd-trace-go/v2/instrumentation/graphql"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

const componentName = instrumentation.Package99DesignsGQLGen

var instr *instrumentation.Instrumentation

func init() {
	instr = instrumentation.Load(instrumentation.Package99DesignsGQLGen)
}

const (
	readOp                  = "graphql.read"
	parsingOp               = "graphql.parse"
	validationOp            = "graphql.validate"
	executeOp               = "graphql.execute"
	fieldOp                 = "graphql.field"
	tagGraphqlSource        = "graphql.source"
	tagGraphqlField         = "graphql.field"
	tagGraphqlOperationType = "graphql.operation.type"
	tagGraphqlOperationName = "graphql.operation.name"
)

type gqlTracer struct {
	cfg *config
}

// NewTracer creates a graphql.HandlerExtension instance that can be used with
// a graphql.handler.Server.
// Options can be passed in for further configuration.
func NewTracer(opts ...Option) graphql.HandlerExtension {
	cfg := new(config)
	defaults(cfg)
	for _, fn := range opts {
		fn.apply(cfg)
	}
	return &gqlTracer{cfg: cfg}
}

func (t *gqlTracer) ExtensionName() string {
	return "DatadogTracing"
}

func (t *gqlTracer) Validate(_ graphql.ExecutableSchema) error {
	return nil // unimplemented
}

func (t *gqlTracer) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	opCtx := graphql.GetOperationContext(ctx)
	span, ctx := t.createRootSpan(ctx, opCtx)
	ctx, req := graphqlsec.StartRequestOperation(ctx, span, graphqlsec.RequestOperationArgs{
		RawQuery:      opCtx.RawQuery,
		OperationName: opCtx.OperationName,
		Variables:     opCtx.Variables,
	})
	ctx, query := graphqlsec.StartExecutionOperation(ctx, graphqlsec.ExecutionOperationArgs{
		Query:         opCtx.RawQuery,
		OperationName: opCtx.OperationName,
		Variables:     opCtx.Variables,
	})
	responseHandler := next(ctx)
	return func(ctx context.Context) *graphql.Response {
		response := responseHandler(ctx)
		if span != nil {
			var spanErr error
			if response != nil && len(response.Errors) > 0 {
				spanErr = response.Errors
				instrgraphql.AddErrorsAsSpanEvents(span, toGraphqlErrors(response.Errors), t.cfg.errExtensions)
			}
			defer span.Finish(tracer.WithError(spanErr))
		}

		var (
			executionOperationRes graphqlsec.ExecutionOperationRes
			requestOperationRes   graphqlsec.RequestOperationRes
		)
		if response != nil {
			executionOperationRes.Data = response.Data
			executionOperationRes.Error = response.Errors

			requestOperationRes.Data = response.Data
			requestOperationRes.Error = response.Errors
		}

		query.Finish(executionOperationRes)
		req.Finish(requestOperationRes)
		return response
	}
}

func (t *gqlTracer) InterceptField(ctx context.Context, next graphql.Resolver) (res any, err error) {
	opCtx := graphql.GetOperationContext(ctx)
	if t.cfg.withoutTraceIntrospectionQuery && isIntrospectionQuery(opCtx) {
		res, err = next(ctx)
		return
	}

	fieldCtx := graphql.GetFieldContext(ctx)
	isTrivial := !(fieldCtx.IsMethod || fieldCtx.IsResolver)
	if t.cfg.withoutTraceTrivialResolvedFields && isTrivial {
		res, err = next(ctx)
		return
	}

	opts := make([]tracer.StartSpanOption, 0, 6+len(t.cfg.tags))
	for k, v := range t.cfg.tags {
		opts = append(opts, tracer.Tag(k, v))
	}
	opts = append(opts,
		tracer.Tag(tagGraphqlField, fieldCtx.Field.Name),
		tracer.Tag(tagGraphqlOperationType, opCtx.Operation.Operation),
		tracer.Tag(ext.Component, componentName),
		tracer.ResourceName(fmt.Sprintf("%s.%s", fieldCtx.Object, fieldCtx.Field.Name)),
		tracer.Measured(),
	)
	if !math.IsNaN(t.cfg.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, t.cfg.analyticsRate))
	}

	span, ctx := tracer.StartSpanFromContext(ctx, fieldOp, opts...)
	defer func() { span.Finish(tracer.WithError(err)) }()
	ctx, op := graphqlsec.StartResolveOperation(ctx, graphqlsec.ResolveOperationArgs{
		Arguments: fieldCtx.Args,
		TypeName:  fieldCtx.Object,
		FieldName: fieldCtx.Field.Name,
		Trivial:   isTrivial,
	})
	defer func() { op.Finish(graphqlsec.ResolveOperationRes{Data: res, Error: err}) }()

	res, err = next(ctx)
	return
}

func (*gqlTracer) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	return next(ctx)
}

// createRootSpan creates a graphql server root span starting at the beginning
// of the operation context. If the operation is a subscription, a nil span is
// returned as those may run indefinitely and would be problematic. This function
// also creates child spans (orphans in the case of a subscription) for the
// read, parsing and validation phases of the operation.
func (t *gqlTracer) createRootSpan(ctx context.Context, opCtx *graphql.OperationContext) (*tracer.Span, context.Context) {
	opts := make([]tracer.StartSpanOption, 0, 7+len(t.cfg.tags))
	for k, v := range t.cfg.tags {
		opts = append(opts, tracer.Tag(k, v))
	}
	opts = append(opts,
		tracer.SpanType(ext.SpanTypeGraphQL),
		tracer.Tag(ext.SpanKind, ext.SpanKindServer),
		tracer.ServiceName(t.cfg.serviceName),
		tracer.Tag(ext.Component, componentName),
		tracer.ResourceName(opCtx.RawQuery),
		tracer.StartTime(opCtx.Stats.OperationStart),
	)
	if !math.IsNaN(t.cfg.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, t.cfg.analyticsRate))
	}
	var rootSpan *tracer.Span
	if opCtx.Operation.Operation != ast.Subscription {
		// Subscriptions are long running queries which may remain open indefinitely
		// until the subscription ends. We do not create the root span for these.
		rootSpan, ctx = tracer.StartSpanFromContext(ctx, serverSpanName(opCtx), opts...)
	}
	createChildSpan := func(name string, start, finish time.Time) {
		childOpts := []tracer.StartSpanOption{
			tracer.StartTime(start),
			tracer.ResourceName(name),
			tracer.Tag(ext.Component, componentName),
		}
		if rootSpan == nil {
			// If there is no root span, decorate the orphan spans with more information
			childOpts = append(childOpts, opts...)
		}
		var childSpan *tracer.Span
		childSpan, _ = tracer.StartSpanFromContext(ctx, name, childOpts...)
		childSpan.Finish(tracer.FinishTime(finish))
	}
	createChildSpan(readOp, opCtx.Stats.Read.Start, opCtx.Stats.Read.End)
	createChildSpan(parsingOp, opCtx.Stats.Parsing.Start, opCtx.Stats.Parsing.End)
	createChildSpan(validationOp, opCtx.Stats.Validation.Start, opCtx.Stats.Validation.End)
	return rootSpan, ctx
}

func serverSpanName(octx *graphql.OperationContext) string {
	graphqlOperation := ""
	if octx != nil && octx.Operation != nil {
		graphqlOperation = string(octx.Operation.Operation)
	}

	return instr.OperationName(
		instrumentation.ComponentDefault,
		instrumentation.OperationContext{
			"graphql.operation": graphqlOperation,
		})
}

func isIntrospectionQuery(octx *graphql.OperationContext) bool {
	if octx.Operation != nil {
		return octx.Operation.Name == "IntrospectionQuery"
	}
	return octx.OperationName == "IntrospectionQuery"
}

// Ensure all of these interfaces are implemented.
var _ interface {
	graphql.HandlerExtension
	graphql.OperationInterceptor
	graphql.FieldInterceptor
	graphql.ResponseInterceptor
} = &gqlTracer{}

func toGraphqlErrors(errs gqlerror.List) []instrgraphql.Error {
	res := make([]instrgraphql.Error, 0, len(errs))
	for _, err := range errs {
		locs := make([]instrgraphql.ErrorLocation, 0, len(err.Locations))
		for _, loc := range err.Locations {
			locs = append(locs, instrgraphql.ErrorLocation{
				Line:   loc.Line,
				Column: loc.Column,
			})
		}
		errPath := make([]any, 0, len(err.Path))
		for _, p := range err.Path {
			errPath = append(errPath, p)
		}
		res = append(res, instrgraphql.Error{
			OriginalErr: err,
			Message:     err.Message,
			Locations:   locs,
			Path:        errPath,
			Extensions:  err.Extensions,
		})
	}
	return res
}
