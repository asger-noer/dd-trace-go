// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package elastic provides functions to trace the gopkg.in/olivere/elastic.v5 packages.
//
// Deprecated: https://github.com/olivere/elastic is deprecated. Please use the official Elasticsearch client for Go
// at https://github.com/elastic/go-elasticsearch and use the corresponding integration. This integration will be removed in a future release.
package elastic // import "github.com/DataDog/dd-trace-go/contrib/olivere/elastic/v2"

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation"
)

var instr *instrumentation.Instrumentation

// in v2 this mod was renamed to olivere/elastic.v5, so we use this instead of instrumentation.PackageOlivereElasticV5
// to keep the same component name.
const componentName = "olivere/elastic"

func init() {
	instr = instrumentation.Load(instrumentation.PackageOlivereElasticV5)
}

// NewHTTPClient returns a new http.Client which traces requests under the given service name.
func NewHTTPClient(opts ...ClientOption) *http.Client {
	cfg := new(clientConfig)
	defaults(cfg)
	for _, fn := range opts {
		fn.apply(cfg)
	}
	instr.Logger().Debug("contrib/olivere/elastic: Configuring HTTP Client: %#v", cfg)
	return &http.Client{Transport: &httpTransport{config: cfg}}
}

// httpTransport is a traced HTTP transport that captures Elasticsearch spans.
type httpTransport struct{ config *clientConfig }

// bodyCutoff specifies the maximum number of bytes that will be stored as a tag
// value obtained from an HTTP request or response body.
var bodyCutoff = 5 * 1024

// RoundTrip satisfies the RoundTripper interface, wraps the sub Transport and
// captures a span of the Elasticsearch request.
func (t *httpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.Path
	method := req.Method
	resource := t.config.resourceNamer(url, method)
	opts := []tracer.StartSpanOption{
		tracer.ServiceName(t.config.serviceName),
		tracer.SpanType(ext.SpanTypeElasticSearch),
		tracer.ResourceName(resource),
		tracer.Tag("elasticsearch.method", method),
		tracer.Tag("elasticsearch.url", url),
		tracer.Tag("elasticsearch.params", req.URL.Query().Encode()),
		tracer.Tag(ext.Component, componentName),
		tracer.Tag(ext.SpanKind, ext.SpanKindClient),
		tracer.Tag(ext.DBSystem, ext.DBSystemElasticsearch),
		tracer.Tag(ext.NetworkDestinationName, req.URL.Hostname()),
	}
	if !math.IsNaN(t.config.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, t.config.analyticsRate))
	}
	span, _ := tracer.StartSpanFromContext(req.Context(), t.config.spanName, opts...)
	defer span.Finish()

	contentEncoding := req.Header.Get("Content-Encoding")
	snip, rc, err := peek(req.Body, contentEncoding, int(req.ContentLength), bodyCutoff)
	if err == nil {
		span.SetTag("elasticsearch.body", snip)
	}
	req.Body = rc
	// process using the standard transport
	res, err := t.config.transport.RoundTrip(req)
	if err != nil {
		// roundtrip error
		span.SetTag(ext.Error, err)
	} else if res.StatusCode < 200 || res.StatusCode > 299 {
		// HTTP error
		snip, rc, err := peek(res.Body, contentEncoding, int(res.ContentLength), bodyCutoff)
		if err != nil {
			snip = http.StatusText(res.StatusCode)
		}
		span.SetTag(ext.Error, errors.New(snip))
		res.Body = rc
	}
	if res != nil {
		span.SetTag(ext.HTTPCode, strconv.Itoa(res.StatusCode))
	}
	return res, err
}

var (
	idRegexp         = regexp.MustCompile("/([0-9]+)([/\\?]|$)")
	idPlaceholder    = []byte("/?$2")
	indexRegexp      = regexp.MustCompile("[0-9]{2,}")
	indexPlaceholder = []byte("?")
)

// quantize quantizes an Elasticsearch to extract a meaningful resource from the request.
// We quantize based on the method+url with some cleanup applied to the URL.
// URLs with an ID will be generalized as will (potential) timestamped indices.
func quantize(url, method string) string {
	quantizedURL := idRegexp.ReplaceAll([]byte(url), idPlaceholder)
	quantizedURL = indexRegexp.ReplaceAll(quantizedURL, indexPlaceholder)
	return fmt.Sprintf("%s %s", method, quantizedURL)
}

// peek attempts to return the first n bytes, as a string, from the provided io.ReadCloser.
// It returns a new io.ReadCloser which points to the same underlying stream and can be read
// from to access the entire data including the snippet. max is used to specify the length
// of the stream contained in the reader. If unknown, it should be -1. If 0 < max < n it
// will override n.
func peek(rc io.ReadCloser, encoding string, max, n int) (string, io.ReadCloser, error) {
	if rc == nil {
		return "", rc, errors.New("empty stream")
	}
	if max > 0 && max < n {
		n = max
	}
	r := bufio.NewReaderSize(rc, n)
	rc2 := struct {
		io.Reader
		io.Closer
	}{
		Reader: r,
		Closer: rc,
	}
	snip, err := r.Peek(n)
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		return string(snip), rc2, err
	}
	if encoding == "gzip" {
		// unpack the snippet
		gzr, err2 := gzip.NewReader(bytes.NewReader(snip))
		if err2 != nil {
			// snip wasn't gzip; return it as is
			return string(snip), rc2, nil
		}
		defer gzr.Close()
		snip, err = io.ReadAll(gzr)
	}
	return string(snip), rc2, err
}
