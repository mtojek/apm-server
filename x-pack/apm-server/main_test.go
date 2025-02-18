// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package main

// This file is mandatory as otherwise the apm-server.test binary is not generated correctly.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pkg/errors"
	"go.elastic.co/apm/v2/apmtest"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
	"github.com/elastic/elastic-agent-libs/paths"

	"github.com/elastic/apm-server/internal/beater"
	"github.com/elastic/apm-server/internal/beater/config"
	"github.com/elastic/apm-server/internal/elasticsearch"
	"github.com/elastic/apm-server/internal/model/modelprocessor"
)

func TestMonitoring(t *testing.T) {
	// samplingMonitoringRegistry will be nil, as under normal circumstances
	// we rely on apm-server/sampling to create the registry.
	samplingMonitoringRegistry = monitoring.NewRegistry()

	home := t.TempDir()
	err := paths.InitPaths(&paths.Path{Home: home})
	require.NoError(t, err)
	defer closeBadger() // close badger.DB so data dir can be deleted on Windows

	cfg := config.DefaultConfig()
	cfg.Sampling.Tail.Enabled = true
	cfg.Sampling.Tail.Policies = []config.TailSamplingPolicy{{SampleRate: 0.1}}

	// Wrap & run the server twice, to ensure metric registration does not panic.
	runServerError := errors.New("runServer")
	for i := 0; i < 2; i++ {
		var aggregationMonitoringSnapshot, tailSamplingMonitoringSnapshot monitoring.FlatSnapshot
		serverParams, runServer, err := wrapServer(beater.ServerParams{
			Config:                 cfg,
			Logger:                 logp.NewLogger(""),
			Tracer:                 apmtest.DiscardTracer,
			BatchProcessor:         modelprocessor.Nop{},
			Managed:                true,
			Namespace:              "default",
			NewElasticsearchClient: elasticsearch.NewClient,
		}, func(ctx context.Context, args beater.ServerParams) error {
			aggregationMonitoringSnapshot = monitoring.CollectFlatSnapshot(aggregationMonitoringRegistry, monitoring.Full, false)
			tailSamplingMonitoringSnapshot = monitoring.CollectFlatSnapshot(samplingMonitoringRegistry, monitoring.Full, false)
			return runServerError
		})
		require.NoError(t, err)

		err = runServer(context.Background(), serverParams)
		assert.Equal(t, runServerError, err)
		assert.NotEqual(t, monitoring.MakeFlatSnapshot(), aggregationMonitoringSnapshot)
		assert.NotEqual(t, monitoring.MakeFlatSnapshot(), tailSamplingMonitoringSnapshot)
	}
}
