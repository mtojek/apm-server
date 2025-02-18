// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package main

import (
	"context"
	"os"
	"sync"

	"github.com/dgraph-io/badger/v2"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
	"github.com/elastic/elastic-agent-libs/paths"

	"github.com/elastic/apm-server/internal/beater"
	"github.com/elastic/apm-server/internal/model"
	"github.com/elastic/apm-server/internal/model/modelprocessor"
	"github.com/elastic/apm-server/x-pack/apm-server/aggregation/spanmetrics"
	"github.com/elastic/apm-server/x-pack/apm-server/aggregation/txmetrics"
	"github.com/elastic/apm-server/x-pack/apm-server/sampling"
	"github.com/elastic/apm-server/x-pack/apm-server/sampling/eventstorage"
)

const (
	tailSamplingStorageDir = "tail_sampling"
)

var (
	aggregationMonitoringRegistry = monitoring.Default.NewRegistry("apm-server.aggregation")

	// Note: this registry is created in github.com/elastic/apm-server/sampling. That package
	// will hopefully disappear in the future, when agents no longer send unsampled transactions.
	samplingMonitoringRegistry = monitoring.Default.GetRegistry("apm-server.sampling")

	// badgerDB holds the badger database to use when tail-based sampling is configured.
	badgerMu sync.Mutex
	badgerDB *badger.DB

	storageMu sync.Mutex
	storage   *eventstorage.ShardedReadWriter
)

type namedProcessor struct {
	processor
	name string
}

type processor interface {
	model.BatchProcessor
	Run() error
	Stop(context.Context) error
}

// newProcessors returns a list of processors which will process
// events in sequential order, prior to the events being published.
func newProcessors(args beater.ServerParams) ([]namedProcessor, error) {
	processors := make([]namedProcessor, 0, 3)
	const txName = "transaction metrics aggregation"
	args.Logger.Infof("creating %s with config: %+v", txName, args.Config.Aggregation.Transactions)
	agg, err := txmetrics.NewAggregator(txmetrics.AggregatorConfig{
		BatchProcessor:                 args.BatchProcessor,
		MaxTransactionGroups:           args.Config.Aggregation.Transactions.MaxTransactionGroups,
		MetricsInterval:                args.Config.Aggregation.Transactions.Interval,
		HDRHistogramSignificantFigures: args.Config.Aggregation.Transactions.HDRHistogramSignificantFigures,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "error creating %s", txName)
	}
	processors = append(processors, namedProcessor{name: txName, processor: agg})
	aggregationMonitoringRegistry.Remove("txmetrics")
	monitoring.NewFunc(aggregationMonitoringRegistry, "txmetrics", agg.CollectMonitoring, monitoring.Report)

	const spanName = "service destinations aggregation"
	args.Logger.Infof("creating %s with config: %+v", spanName, args.Config.Aggregation.ServiceDestinations)
	spanAggregator, err := spanmetrics.NewAggregator(spanmetrics.AggregatorConfig{
		BatchProcessor: args.BatchProcessor,
		Interval:       args.Config.Aggregation.ServiceDestinations.Interval,
		MaxGroups:      args.Config.Aggregation.ServiceDestinations.MaxGroups,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "error creating %s", spanName)
	}
	processors = append(processors, namedProcessor{name: spanName, processor: spanAggregator})
	if args.Config.Sampling.Tail.Enabled {
		const name = "tail sampler"
		sampler, err := newTailSamplingProcessor(args)
		if err != nil {
			return nil, errors.Wrapf(err, "error creating %s", name)
		}
		samplingMonitoringRegistry.Remove("tail")
		monitoring.NewFunc(samplingMonitoringRegistry, "tail", sampler.CollectMonitoring, monitoring.Report)
		processors = append(processors, namedProcessor{name: name, processor: sampler})
	}
	return processors, nil
}

func newTailSamplingProcessor(args beater.ServerParams) (*sampling.Processor, error) {
	tailSamplingConfig := args.Config.Sampling.Tail
	es, err := args.NewElasticsearchClient(tailSamplingConfig.ESConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create Elasticsearch client for tail-sampling")
	}

	storageDir := paths.Resolve(paths.Data, tailSamplingStorageDir)
	badgerDB, err = getBadgerDB(storageDir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get Badger database")
	}
	readWriters := getStorage(badgerDB)

	policies := make([]sampling.Policy, len(tailSamplingConfig.Policies))
	for i, in := range tailSamplingConfig.Policies {
		policies[i] = sampling.Policy{
			PolicyCriteria: sampling.PolicyCriteria{
				ServiceName:        in.Service.Name,
				ServiceEnvironment: in.Service.Environment,
				TraceName:          in.Trace.Name,
				TraceOutcome:       in.Trace.Outcome,
			},
			SampleRate: in.SampleRate,
		}
	}

	return sampling.NewProcessor(sampling.Config{
		BeatID:         args.UUID.String(),
		BatchProcessor: args.BatchProcessor,
		LocalSamplingConfig: sampling.LocalSamplingConfig{
			FlushInterval:         tailSamplingConfig.Interval,
			MaxDynamicServices:    1000,
			Policies:              policies,
			IngestRateDecayFactor: tailSamplingConfig.IngestRateDecayFactor,
		},
		RemoteSamplingConfig: sampling.RemoteSamplingConfig{
			CompressionLevel: tailSamplingConfig.ESConfig.CompressionLevel,
			Elasticsearch:    es,
			SampledTracesDataStream: sampling.DataStreamConfig{
				Type:      "traces",
				Dataset:   "apm.sampled",
				Namespace: args.Namespace,
			},
		},
		StorageConfig: sampling.StorageConfig{
			DB:                badgerDB,
			Storage:           readWriters,
			StorageDir:        storageDir,
			StorageGCInterval: tailSamplingConfig.StorageGCInterval,
			StorageLimit:      tailSamplingConfig.StorageLimitParsed,
			TTL:               tailSamplingConfig.TTL,
		},
	})
}

func getBadgerDB(storageDir string) (*badger.DB, error) {
	badgerMu.Lock()
	defer badgerMu.Unlock()
	if badgerDB == nil {
		db, err := eventstorage.OpenBadger(storageDir, -1)
		if err != nil {
			return nil, err
		}
		badgerDB = db
	}
	return badgerDB, nil
}

func getStorage(db *badger.DB) *eventstorage.ShardedReadWriter {
	storageMu.Lock()
	defer storageMu.Unlock()
	if storage == nil {
		eventCodec := eventstorage.JSONCodec{}
		storage = eventstorage.New(db, eventCodec).NewShardedReadWriter()
	}
	return storage
}

// runServerWithProcessors runs the APM Server and the given list of processors.
//
// newProcessors returns a list of processors which will process events in
// sequential order, prior to the events being published.
func runServerWithProcessors(ctx context.Context, runServer beater.RunServerFunc, args beater.ServerParams, processors ...namedProcessor) error {
	if len(processors) == 0 {
		return runServer(ctx, args)
	}

	g, ctx := errgroup.WithContext(ctx)
	serverStopped := make(chan struct{})
	for _, p := range processors {
		p := p // copy for closure
		g.Go(func() error {
			if err := p.Run(); err != nil {
				args.Logger.With(logp.Error(err)).Errorf("%s aborted", p.name)
				return err
			}
			args.Logger.Infof("%s stopped", p.name)
			return nil
		})
		g.Go(func() error {
			<-serverStopped
			stopctx := context.Background()
			if args.Config.ShutdownTimeout > 0 {
				// On shutdown wait for the aggregator to stop
				// in order to flush any accumulated metrics.
				var cancel context.CancelFunc
				stopctx, cancel = context.WithTimeout(stopctx, args.Config.ShutdownTimeout)
				defer cancel()
			}
			return p.Stop(stopctx)
		})
	}
	g.Go(func() error {
		defer close(serverStopped)
		return runServer(ctx, args)
	})
	return g.Wait()
}

func wrapServer(args beater.ServerParams, runServer beater.RunServerFunc) (beater.ServerParams, beater.RunServerFunc, error) {
	processors, err := newProcessors(args)
	if err != nil {
		return beater.ServerParams{}, nil, err
	}

	// Add the processors to the chain.
	processorChain := make(modelprocessor.Chained, len(processors)+1)
	for i, p := range processors {
		processorChain[i] = p
	}
	processorChain[len(processors)] = args.BatchProcessor
	args.BatchProcessor = processorChain

	wrappedRunServer := func(ctx context.Context, args beater.ServerParams) error {
		return runServerWithProcessors(ctx, runServer, args, processors...)
	}
	return args, wrappedRunServer, nil
}

// closeBadger is called at process exit time to close the badger.DB opened
// by the tail-based sampling processor constructor, if any. This is never
// called concurrently with opening badger.DB/accessing the badgerDB global,
// so it does not need to hold badgerMu.
func closeBadger() error {
	if badgerDB != nil {
		return badgerDB.Close()
	}
	return nil
}

func closeStorage() {
	if storage != nil {
		storage.Close()
	}
}

func cleanup() (result error) {
	// Close the underlying storage, the storage will be flushed on processor stop.
	closeStorage()

	if err := closeBadger(); err != nil {
		result = multierror.Append(result, err)
	}
	return result
}

func Main() error {
	rootCmd := newXPackRootCommand(
		beater.NewCreator(beater.CreatorParams{
			WrapServer: wrapServer,
		}),
	)
	result := rootCmd.Execute()
	if err := cleanup(); err != nil {
		result = multierror.Append(result, err)
	}
	return result
}

func main() {
	if err := Main(); err != nil {
		os.Exit(1)
	}
}
