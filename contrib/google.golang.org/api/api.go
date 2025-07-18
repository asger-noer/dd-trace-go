// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package api provides functions to trace the google.golang.org/api package.
//
// WARNING: Please note we periodically re-generate the endpoint metadata that is used to enrich some tags
// added by this integration using the latest versions of github.com/googleapis/google-api-go-client (which does not
// follow semver due to the auto-generated nature of the package). For this reason, there might be unexpected changes
// in some tag values like service.name and resource.name, depending on the google.golang.org/api that you are using in your
// project. If this is not an acceptable behavior for your use-case, you can disable this feature using the
// WithEndpointMetadataDisabled option.
package api // import "github.com/DataDog/dd-trace-go/contrib/google.golang.org/api/v2"

//go:generate go run ./internal/gen_endpoints -o gen_endpoints.json

import (
	_ "embed"
	"encoding/json"
	"math"
	"net/http"

	"github.com/DataDog/dd-trace-go/contrib/google.golang.org/api/v2/internal/tree"
	httptrace "github.com/DataDog/dd-trace-go/contrib/net/http/v2"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation"

	"golang.org/x/oauth2/google"
)

//go:embed gen_endpoints.json
var endpointBytes []byte

const componentName = "google.golang.org/api"

var instr *instrumentation.Instrumentation

func init() {
	instr = instrumentation.Load(instrumentation.PackageGoogleAPI)
	initAPIEndpointsTree()
}

// apiEndpoints are the defined endpoints for the Google API; it is populated
// by "go generate".
var apiEndpointsTree *tree.Tree

func loadEndpointsFromJSON() ([]*tree.Endpoint, error) {
	var apiEndpoints []*tree.Endpoint
	if err := json.Unmarshal(endpointBytes, &apiEndpoints); err != nil {
		return nil, err
	}
	return apiEndpoints, nil
}

func initAPIEndpointsTree() {
	apiEndpoints, err := loadEndpointsFromJSON()
	if err != nil {
		instr.Logger().Warn("contrib/google.golang.org/api: failed load json endpoints: %s", err.Error())
		return
	}
	tr, err := tree.New(apiEndpoints...)
	if err != nil {
		instr.Logger().Warn("contrib/google.golang.org/api: failed to create endpoints tree: %s", err.Error())
		return
	}
	apiEndpointsTree = tr
}

// NewClient creates a new oauth http client suitable for use with the google
// APIs with all requests traced automatically.
func NewClient(options ...Option) (*http.Client, error) {
	cfg := newConfig(options...)
	instr.Logger().Debug("contrib/google.golang.org/api: Creating Client: %#v", cfg)
	client, err := google.DefaultClient(cfg.ctx, cfg.scopes...)
	if err != nil {
		return nil, err
	}
	client.Transport = WrapRoundTripper(client.Transport, options...)
	return client, nil
}

// WrapRoundTripper wraps a RoundTripper intended for interfacing with
// Google APIs and traces all requests.
func WrapRoundTripper(transport http.RoundTripper, options ...Option) http.RoundTripper {
	cfg := newConfig(options...)
	instr.Logger().Debug("contrib/google.golang.org/api: Wrapping RoundTripper: %#v", cfg)
	rtOpts := []httptrace.RoundTripperOption{
		httptrace.WithBefore(func(req *http.Request, span *tracer.Span) {
			if !cfg.endpointMetadataDisabled {
				setTagsWithEndpointMetadata(req, span)
			} else {
				setTagsWithoutEndpointMetadata(req, span)
			}
			if cfg.serviceName != "" {
				span.SetTag(ext.ServiceName, cfg.serviceName)
			}
			span.SetTag(ext.Component, componentName)
			span.SetTag(ext.SpanKind, ext.SpanKindClient)
		}),
	}
	if !math.IsNaN(cfg.analyticsRate) {
		rtOpts = append(rtOpts, httptrace.WithAnalyticsRate(cfg.analyticsRate))
	}
	return httptrace.WrapRoundTripper(transport, rtOpts...)
}

func setTagsWithEndpointMetadata(req *http.Request, span *tracer.Span) {
	e, ok := apiEndpointsTree.Get(req.URL.Hostname(), req.Method, req.URL.Path)
	if ok {
		span.SetTag(ext.ServiceName, e.ServiceName)
		span.SetTag(ext.ResourceName, e.ResourceName)
	} else {
		setTagsWithoutEndpointMetadata(req, span)
	}
}

func setTagsWithoutEndpointMetadata(req *http.Request, span *tracer.Span) {
	span.SetTag(ext.ServiceName, "google")
	span.SetTag(ext.ResourceName, req.Method+" "+req.URL.Hostname())
}
