// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"fmt"
	"os"
	"strings"

	"github.com/DataDog/dd-trace-go/v2/internal/log"
	"github.com/DataDog/dd-trace-go/v2/internal/telemetry"
)

var additionalConfigs []telemetry.Configuration

func reportTelemetryOnAppStarted(c telemetry.Configuration) {
	additionalConfigs = append(additionalConfigs, c)
}

// startTelemetry starts the global instrumentation telemetry client with tracer data
// unless instrumentation telemetry is disabled via the DD_INSTRUMENTATION_TELEMETRY_ENABLED
// env var.
// If the telemetry client has already been started by the profiler, then
// an app-product-change event is sent with appsec information and an app-client-configuration-change
// event is sent with tracer config data.
// Note that the tracer is not considered as a standalone product by telemetry so we cannot send
// an app-product-change event for the tracer.
func startTelemetry(c *config) {
	if telemetry.Disabled() {
		// Do not do extra work populating config data if instrumentation telemetry is disabled.
		return
	}

	telemetry.ProductStarted(telemetry.NamespaceTracers)
	telemetryConfigs := []telemetry.Configuration{
		{Name: "agent_feature_drop_p0s", Value: c.agent.DropP0s},
		{Name: "stats_computation_enabled", Value: c.canComputeStats()},
		{Name: "dogstatsd_port", Value: c.agent.StatsdPort},
		{Name: "lambda_mode", Value: c.logToStdout},
		{Name: "send_retries", Value: c.sendRetries},
		{Name: "retry_interval", Value: c.retryInterval},
		{Name: "trace_startup_logs_enabled", Value: c.logStartup},
		{Name: "service", Value: c.serviceName},
		{Name: "universal_version", Value: c.universalVersion},
		{Name: "env", Value: c.env},
		{Name: "version", Value: c.version},
		{Name: "trace_agent_url", Value: c.agentURL.String()},
		{Name: "agent_hostname", Value: c.hostname},
		{Name: "runtime_metrics_v2_enabled", Value: c.runtimeMetricsV2},
		{Name: "dogstatsd_addr", Value: c.dogstatsdAddr},
		{Name: "debug_stack_enabled", Value: !c.noDebugStack},
		{Name: "profiling_hotspots_enabled", Value: c.profilerHotspots},
		{Name: "profiling_endpoints_enabled", Value: c.profilerEndpoints},
		{Name: "trace_span_attribute_schema", Value: c.spanAttributeSchemaVersion},
		{Name: "trace_peer_service_defaults_enabled", Value: c.peerServiceDefaultsEnabled},
		{Name: "orchestrion_enabled", Value: c.orchestrionCfg.Enabled, Origin: telemetry.OriginCode},
		{Name: "trace_enabled", Value: c.enabled.current, Origin: c.enabled.cfgOrigin},
		{Name: "trace_log_directory", Value: c.logDirectory},
		c.traceSampleRate.toTelemetry(),
		c.headerAsTags.toTelemetry(),
		c.globalTags.toTelemetry(),
		c.traceSampleRules.toTelemetry(),
		{Name: "span_sample_rules", Value: c.spanRules},
	}
	var peerServiceMapping []string
	for key, value := range c.peerServiceMappings {
		peerServiceMapping = append(peerServiceMapping, fmt.Sprintf("%s:%s", key, value))
	}
	telemetryConfigs = append(telemetryConfigs,
		telemetry.Configuration{Name: "trace_peer_service_mapping", Value: strings.Join(peerServiceMapping, ",")})

	if chained, ok := c.propagator.(*chainedPropagator); ok {
		telemetryConfigs = append(telemetryConfigs,
			telemetry.Configuration{Name: "trace_propagation_style_inject", Value: chained.injectorNames})
		telemetryConfigs = append(telemetryConfigs,
			telemetry.Configuration{Name: "trace_propagation_style_extract", Value: chained.extractorsNames})
	}
	for k, v := range c.featureFlags {
		telemetryConfigs = append(telemetryConfigs, telemetry.Configuration{Name: k, Value: v})
	}
	for k, v := range c.serviceMappings {
		telemetryConfigs = append(telemetryConfigs, telemetry.Configuration{Name: "service_mapping_" + k, Value: v})
	}
	for k, v := range c.globalTags.get() {
		telemetryConfigs = append(telemetryConfigs, telemetry.Configuration{Name: "global_tag_" + k, Value: v})
	}
	rules := append(c.spanRules, c.traceRules...)
	for _, rule := range rules {
		var service string
		var name string
		if rule.Service != nil {
			service = rule.Service.String()
		}
		if rule.Name != nil {
			name = rule.Name.String()
		}
		telemetryConfigs = append(telemetryConfigs,
			telemetry.Configuration{Name: fmt.Sprintf("sr_%s_(%s)_(%s)", rule.ruleType.String(), service, name),
				Value: fmt.Sprintf("rate:%f_maxPerSecond:%f", rule.Rate, rule.MaxPerSecond)})
	}
	if c.orchestrionCfg.Enabled {
		telemetryConfigs = append(telemetryConfigs, telemetry.Configuration{Name: "orchestrion_version", Value: c.orchestrionCfg.Metadata.Version, Origin: telemetry.OriginCode})
	}
	telemetryConfigs = append(telemetryConfigs, additionalConfigs...)
	telemetry.RegisterAppConfigs(telemetryConfigs...)
	cfg := telemetry.ClientConfig{
		HTTPClient: c.httpClient,
		AgentURL:   c.agentURL.String(),
	}
	if c.logToStdout || c.ciVisibilityAgentless {
		cfg.APIKey = os.Getenv("DD_API_KEY")
	}
	client, err := telemetry.NewClient(c.serviceName, c.env, c.version, cfg)
	if err != nil {
		log.Debug("tracer: failed to create telemetry client: %v", err)
		return
	}

	if c.orchestrionCfg.Enabled {
		// If orchestrion is enabled, report it to the back-end via a telemetry metric on every flush.
		handle := client.Gauge(telemetry.NamespaceTracers, "orchestrion.enabled", []string{"version:" + c.orchestrionCfg.Metadata.Version})
		client.AddFlushTicker(func(_ telemetry.Client) { handle.Submit(1) })
	}

	telemetry.StartApp(client)
}
