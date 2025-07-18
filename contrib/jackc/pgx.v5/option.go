// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package pgx

import (
	"github.com/DataDog/dd-trace-go/v2/instrumentation"
)

type config struct {
	serviceName   string
	traceQuery    bool
	traceBatch    bool
	traceCopyFrom bool
	tracePrepare  bool
	traceConnect  bool
	traceAcquire  bool
	poolStats     bool
	statsdClient  instrumentation.StatsdClient
}

func defaultConfig() *config {
	return &config{
		serviceName:   instr.ServiceName(instrumentation.ComponentDefault, nil),
		traceQuery:    true,
		traceBatch:    true,
		traceCopyFrom: true,
		tracePrepare:  true,
		traceConnect:  true,
		traceAcquire:  true,
	}
}

// checkStatsdRequired adds a statsdClient onto the config if poolStats is enabled
// NOTE: For now, the only use-case for a statsdclient is the poolStats feature. If a statsdclient becomes necessary for other items in future work, then this logic should change
func (c *config) checkStatsdRequired() {
	if c.poolStats && c.statsdClient == nil {
		// contrib/jackc/pgx's statsdclient should always inherit its address from the tracer's statsdclient via the globalconfig
		// destination is not user-configurable
		sc, err := instr.StatsdClient(statsTags(c))
		if err == nil {
			c.statsdClient = sc
		} else {
			instr.Logger().Warn("contrib/jackc/pgx.v5: Error creating statsd client; Pool stats will be dropped: %s", err.Error())
		}
	}
}

type Option func(*config)

// WithService sets the service name to use for all spans.
func WithService(name string) Option {
	return func(c *config) {
		c.serviceName = name
	}
}

// WithTraceQuery enables tracing query operations.
func WithTraceQuery(enabled bool) Option {
	return func(c *config) {
		c.traceQuery = enabled
	}
}

// WithTraceBatch enables tracing batched operations (i.e. pgx.Batch{}).
func WithTraceBatch(enabled bool) Option {
	return func(c *config) {
		c.traceBatch = enabled
	}
}

// WithTraceCopyFrom enables tracing pgx.CopyFrom calls.
func WithTraceCopyFrom(enabled bool) Option {
	return func(c *config) {
		c.traceCopyFrom = enabled
	}
}

// WithTraceAcquire enables tracing pgxpool connection acquire calls.
func WithTraceAcquire(enabled bool) Option {
	return func(c *config) {
		c.traceAcquire = enabled
	}
}

// WithTracePrepare enables tracing prepared statements.
func WithTracePrepare(enabled bool) Option {
	return func(c *config) {
		c.tracePrepare = enabled
	}
}

// WithTraceConnect enables tracing calls to Connect and ConnectConfig.
func WithTraceConnect(enabled bool) Option {
	return func(c *config) {
		c.traceConnect = enabled
	}
}

// WithPoolStats enables polling of pgxpool.Stat metrics
// ref: https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool#Stat
// These metrics are submitted to Datadog and are not billed as custom metrics
func WithPoolStats() Option {
	return func(cfg *config) {
		cfg.poolStats = true
	}
}
