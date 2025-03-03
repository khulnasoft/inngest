package devserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coocood/freecache"
	"github.com/eko/gocache/lib/v4/cache"
	freecachestore "github.com/eko/gocache/store/freecache/v4"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/khulnasoft/inngest/pkg/api"
	"github.com/khulnasoft/inngest/pkg/api/apiv1"
	"github.com/khulnasoft/inngest/pkg/backoff"
	"github.com/khulnasoft/inngest/pkg/config"
	_ "github.com/khulnasoft/inngest/pkg/config/defaults"
	"github.com/khulnasoft/inngest/pkg/config/registration"
	"github.com/khulnasoft/inngest/pkg/connect"
	"github.com/khulnasoft/inngest/pkg/connect/auth"
	"github.com/khulnasoft/inngest/pkg/connect/lifecycles"
	pubsub2 "github.com/khulnasoft/inngest/pkg/connect/pubsub"
	connectv0 "github.com/khulnasoft/inngest/pkg/connect/rest/v0"
	connstate "github.com/khulnasoft/inngest/pkg/connect/state"
	"github.com/khulnasoft/inngest/pkg/consts"
	"github.com/khulnasoft/inngest/pkg/coreapi"
	"github.com/khulnasoft/inngest/pkg/cqrs/base_cqrs"
	"github.com/khulnasoft/inngest/pkg/deploy"
	"github.com/khulnasoft/inngest/pkg/enums"
	"github.com/khulnasoft/inngest/pkg/event"
	"github.com/khulnasoft/inngest/pkg/execution"
	"github.com/khulnasoft/inngest/pkg/execution/batch"
	"github.com/khulnasoft/inngest/pkg/execution/debounce"
	"github.com/khulnasoft/inngest/pkg/execution/driver"
	"github.com/khulnasoft/inngest/pkg/execution/driver/httpdriver"
	"github.com/khulnasoft/inngest/pkg/execution/executor"
	"github.com/khulnasoft/inngest/pkg/execution/history"
	"github.com/khulnasoft/inngest/pkg/execution/queue"
	"github.com/khulnasoft/inngest/pkg/execution/ratelimit"
	"github.com/khulnasoft/inngest/pkg/execution/realtime"
	"github.com/khulnasoft/inngest/pkg/execution/runner"
	"github.com/khulnasoft/inngest/pkg/execution/state"
	"github.com/khulnasoft/inngest/pkg/execution/state/redis_state"
	sv2 "github.com/khulnasoft/inngest/pkg/execution/state/v2"
	"github.com/khulnasoft/inngest/pkg/expressions"
	"github.com/khulnasoft/inngest/pkg/history_drivers/memory_reader"
	"github.com/khulnasoft/inngest/pkg/history_drivers/memory_writer"
	"github.com/khulnasoft/inngest/pkg/logger"
	"github.com/khulnasoft/inngest/pkg/pubsub"
	"github.com/khulnasoft/inngest/pkg/run"
	"github.com/khulnasoft/inngest/pkg/service"
	itrace "github.com/khulnasoft/inngest/pkg/telemetry/trace"
	"github.com/khulnasoft/inngest/pkg/testapi"
	"github.com/khulnasoft/inngest/pkg/util/awsgateway"
	"github.com/redis/rueidis"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/sync/errgroup"
)

const (
	DefaultTick               = 150
	DefaultTickDuration       = time.Millisecond * DefaultTick
	DefaultPollInterval       = 5
	DefaultQueueWorkers       = 100
	DefaultConnectGatewayPort = 8289
)

// StartOpts configures the dev server
type StartOpts struct {
	Config        config.Config `json:"-"`
	RootDir       string        `json:"dir"`
	URLs          []string      `json:"urls"`
	Autodiscover  bool          `json:"autodiscover"`
	Poll          bool          `json:"poll"`
	PollInterval  int           `json:"poll_interval"`
	Tick          time.Duration `json:"tick"`
	RetryInterval int           `json:"retry_interval"`
	QueueWorkers  int           `json:"queue_workers"`

	// SigningKey is used to decide that the server should sign requests and
	// validate responses where applicable, modelling cloud behaviour.
	SigningKey *string `json:"-"`

	// EventKey is used to authorize incoming events, ensuring they match the
	// given key.
	EventKeys []string `json:"-"`

	// RequireKeys defines whether event and signing keys are required for the
	// server to function. If this is true and signing keys are not defined,
	// the server will still boot but core actions such as syncing, runs, and
	// ingesting events will not work.
	RequireKeys bool `json:"require_keys"`

	ConnectGatewayPort int `json:"connectGatewayPort"`
}

// Create and start a new dev server.  The dev server is used during (surprise surprise)
// development.
//
// It runs all available services from `inngest serve`, plus:
// - Adds development-specific APIs for communicating with the SDK.
func New(ctx context.Context, opts StartOpts) error {
	// The dev server _always_ logs output for development.
	if !opts.Config.Execution.LogOutput {
		opts.Config.Execution.LogOutput = true
	}

	// NOTE: looks deprecated?
	// Before running the development service, ensure that we change the http
	// driver in development to use our AWS Gateway http client, attempting to
	// automatically transform dev requests to lambda invocations.
	httpdriver.DefaultExecutor.Client.Transport = awsgateway.NewTransformTripper(httpdriver.DefaultExecutor.Client.Transport)
	deploy.Client.Transport = awsgateway.NewTransformTripper(deploy.Client.Transport)

	return start(ctx, opts)
}

func start(ctx context.Context, opts StartOpts) error {
	db, err := base_cqrs.New(base_cqrs.BaseCQRSOptions{InMemory: true})
	if err != nil {
		return err
	}

	if opts.Tick == 0 {
		opts.Tick = DefaultTickDuration
	}

	// Initialize the devserver
	dbDriver := "sqlite"
	dbcqrs := base_cqrs.NewCQRS(db, dbDriver)
	hd := base_cqrs.NewHistoryDriver(db, dbDriver)
	loader := dbcqrs.(state.FunctionLoader)

	stepLimitOverrides := make(map[string]int)
	stateSizeLimitOverrides := make(map[string]int)

	shardedRc, err := createInmemoryRedis(ctx, opts.Tick)
	if err != nil {
		return err
	}

	unshardedRc, err := createInmemoryRedis(ctx, opts.Tick)
	if err != nil {
		return err
	}

	connectRc, err := createInmemoryRedis(ctx, opts.Tick)
	if err != nil {
		return err
	}

	unshardedClient := redis_state.NewUnshardedClient(unshardedRc, redis_state.StateDefaultKey, redis_state.QueueDefaultKey)
	shardedClient := redis_state.NewShardedClient(redis_state.ShardedClientOpts{
		UnshardedClient:        unshardedClient,
		FunctionRunStateClient: shardedRc,
		StateDefaultKey:        redis_state.StateDefaultKey,
		FnRunIsSharded:         redis_state.AlwaysShardOnRun,
		BatchClient:            shardedRc,
		QueueDefaultKey:        redis_state.QueueDefaultKey,
	})

	queueShard := redis_state.QueueShard{Name: consts.DefaultQueueShardName, RedisClient: unshardedClient.Queue(), Kind: string(enums.QueueShardKindRedis)}

	shardSelector := func(ctx context.Context, _ uuid.UUID, _ *string) (redis_state.QueueShard, error) {
		return queueShard, nil
	}

	queueShards := map[string]redis_state.QueueShard{
		consts.DefaultQueueShardName: queueShard,
	}

	var sm state.Manager
	t := runner.NewTracker()
	sm, err = redis_state.New(
		ctx,
		redis_state.WithShardedClient(shardedClient),
		redis_state.WithUnshardedClient(unshardedClient),
	)
	if err != nil {
		return err
	}
	smv2 := redis_state.MustRunServiceV2(sm)

	// Create a new broadcaster which lets us broadcast realtime messages.
	broadcaster := realtime.NewInProcessBroadcaster()

	queueOpts := []redis_state.QueueOpt{
		redis_state.WithRunMode(redis_state.QueueRunMode{
			Sequential: true,
			Scavenger:  true,
			Partition:  true,
		}),
		redis_state.WithIdempotencyTTL(time.Hour),
		redis_state.WithNumWorkers(int32(opts.QueueWorkers)),
		redis_state.WithPollTick(opts.Tick),
		redis_state.WithCustomConcurrencyKeyLimitRefresher(func(ctx context.Context, i queue.QueueItem) []state.CustomConcurrency {
			keys := i.Data.GetConcurrencyKeys()

			fn, err := dbcqrs.GetFunctionByInternalUUID(ctx, i.Data.Identifier.WorkspaceID, i.Data.Identifier.WorkflowID)
			if err != nil {
				// Use what's stored in the state store.
				return keys
			}
			f, err := fn.InngestFunction()
			if err != nil {
				return keys
			}

			if f.Concurrency != nil {
				for _, c := range f.Concurrency.Limits {
					if !c.IsCustomLimit() {
						continue
					}
					// If there's a concurrency key with the same hash, use the new function's
					// concurrency limits.
					//
					// NOTE:  This is accidentally quadratic but is okay as we bound concurrency
					// keys to a low value (2-3).
					for n, actual := range keys {
						if actual.Hash != "" && actual.Hash == c.Hash {
							actual.Limit = c.Limit
							keys[n] = actual
						}
					}
				}
			}

			return keys
		}),
		redis_state.WithConcurrencyLimitGetter(func(ctx context.Context, p redis_state.QueuePartition) redis_state.PartitionConcurrencyLimits {
			// In the dev server, there are never account limits.
			limits := redis_state.PartitionConcurrencyLimits{
				AccountLimit: redis_state.NoConcurrencyLimit,
			}

			// Ensure that we return the correct concurrency values per
			// partition.
			funcs, err := dbcqrs.GetFunctions(ctx)
			if err != nil {
				return redis_state.PartitionConcurrencyLimits{
					AccountLimit:   redis_state.NoConcurrencyLimit,
					FunctionLimit:  consts.DefaultConcurrencyLimit,
					CustomKeyLimit: consts.DefaultConcurrencyLimit,
				}
			}
			for _, fun := range funcs {
				f, _ := fun.InngestFunction()
				if f.ID == uuid.Nil {
					f.ID = f.DeterministicUUID()
				}
				// Update the function's concurrency here with latest defaults
				if p.FunctionID != nil && f.ID == *p.FunctionID && f.Concurrency != nil && f.Concurrency.PartitionConcurrency() > 0 {
					limits.FunctionLimit = f.Concurrency.PartitionConcurrency()
				}
			}
			if p.EvaluatedConcurrencyKey != "" {
				limits.CustomKeyLimit = p.ConcurrencyLimit
			}

			return limits
		}),
		redis_state.WithShardSelector(shardSelector),
		redis_state.WithQueueShardClients(queueShards),
	}
	if opts.RetryInterval > 0 {
		queueOpts = append(queueOpts, redis_state.WithBackoffFunc(
			backoff.GetLinearBackoffFunc(time.Duration(opts.RetryInterval)*time.Second),
		))
	}
	rq := redis_state.NewQueue(queueShard, queueOpts...)

	rl := ratelimit.New(ctx, unshardedRc, "{ratelimit}:")

	batcher := batch.NewRedisBatchManager(shardedClient.Batch(), rq)
	debouncer := debounce.NewRedisDebouncer(unshardedClient.Debounce(), queueShard, rq)

	connectPubSubRedis := createConnectPubSubRedis()
	gatewayProxy, err := pubsub2.NewConnector(ctx, pubsub2.WithRedis(connectPubSubRedis, logger.StdlibLoggerWithCustomVarName(ctx, "CONNECT_PUBSUB_LOG_LEVEL"), true))
	if err != nil {
		return fmt.Errorf("failed to create connect pubsub connector: %w", err)
	}

	connectionManager := connstate.NewRedisConnectionStateManager(connectRc)

	// Create a new expression aggregator, using Redis to load evaluables.
	agg := expressions.NewAggregator(ctx, 100, 100, sm.(expressions.EvaluableLoader), nil)

	var drivers = []driver.Driver{}
	for _, driverConfig := range opts.Config.Execution.Drivers {
		d, err := driverConfig.NewDriver(registration.NewDriverOpts{
			ConnectForwarder: gatewayProxy,
		})
		if err != nil {
			return err
		}
		drivers = append(drivers, d)
	}
	pb, err := pubsub.NewPublisher(ctx, opts.Config.EventStream.Service)
	if err != nil {
		return fmt.Errorf("failed to create publisher: %w", err)
	}

	hmw := memory_writer.NewWriter(ctx, memory_writer.WriterOptions{DumpToFile: false})

	exec, err := executor.NewExecutor(
		executor.WithStateManager(smv2),
		executor.WithPauseManager(sm),
		executor.WithRuntimeDrivers(
			drivers...,
		),
		executor.WithExpressionAggregator(agg),
		executor.WithQueue(rq),
		executor.WithLogger(logger.From(ctx)),
		executor.WithFunctionLoader(loader),
		executor.WithRealtimePublisher(broadcaster),
		executor.WithLifecycleListeners(
			history.NewLifecycleListener(
				nil,
				hd,
				hmw,
			),
			Lifecycle{
				Cqrs:       dbcqrs,
				Pb:         pb,
				EventTopic: opts.Config.EventStream.Service.Concrete.TopicName(),
			},
			run.NewTraceLifecycleListener(nil),
		),
		executor.WithStepLimits(func(id sv2.ID) int {
			if override, hasOverride := stepLimitOverrides[id.FunctionID.String()]; hasOverride {
				logger.From(ctx).Warn().Msgf("Using step limit override of %d for %q\n", override, id.FunctionID)
				return override
			}

			return consts.DefaultMaxStepLimit
		}),
		executor.WithStateSizeLimits(func(id sv2.ID) int {
			if override, hasOverride := stateSizeLimitOverrides[id.FunctionID.String()]; hasOverride {
				logger.From(ctx).Warn().Msgf("Using state size limit override of %d for %q\n", override, id.FunctionID)
				return override
			}

			return consts.DefaultMaxStateSizeLimit
		}),
		executor.WithInvokeFailHandler(getInvokeFailHandler(ctx, pb, opts.Config.EventStream.Service.Concrete.TopicName())),
		executor.WithSendingEventHandler(getSendingEventHandler(ctx, pb, opts.Config.EventStream.Service.Concrete.TopicName())),
		executor.WithDebouncer(debouncer),
		executor.WithBatcher(batcher),
		executor.WithAssignedQueueShard(queueShard),
		executor.WithShardSelector(shardSelector),
		executor.WithTraceReader(dbcqrs),
	)
	if err != nil {
		return err
	}

	// Create an executor.
	executorSvc := executor.NewService(
		opts.Config,
		executor.WithExecutionManager(dbcqrs),
		executor.WithState(sm),
		executor.WithServiceQueue(rq),
		executor.WithServiceExecutor(exec),
		executor.WithServiceBatcher(batcher),
		executor.WithServiceDebouncer(debouncer),
	)

	runner := runner.NewService(
		opts.Config,
		runner.WithCQRS(dbcqrs),
		runner.WithExecutor(exec),
		runner.WithExecutionManager(dbcqrs),
		runner.WithEventManager(event.NewManager()),
		runner.WithStateManager(sm),
		runner.WithRunnerQueue(rq),
		runner.WithTracker(t),
		runner.WithRateLimiter(rl),
		runner.WithBatchManager(batcher),
		runner.WithPublisher(pb),
	)

	// The devserver embeds the event API.
	ds := NewService(opts, runner, dbcqrs, pb, stepLimitOverrides, stateSizeLimitOverrides, unshardedRc, hmw, nil)
	// embed the tracker
	ds.Tracker = t
	ds.State = sm
	ds.Queue = rq
	ds.Executor = exec
	// start the API
	// Create a new API endpoint which hosts SDK-related functionality for
	// registering functions.
	devAPI := NewDevAPI(ds)

	devAPI.Route("/v1", func(r chi.Router) {
		// Add the V1 API to our dev server API.
		cache := cache.New[[]byte](freecachestore.NewFreecache(freecache.NewCache(1024 * 1024)))
		caching := apiv1.NewCacheMiddleware(cache)

		apiv1.AddRoutes(r, apiv1.Opts{
			CachingMiddleware:  caching,
			EventReader:        ds.Data,
			FunctionReader:     ds.Data,
			FunctionRunReader:  ds.Data,
			JobQueueReader:     ds.Queue.(queue.JobQueueReader),
			Executor:           ds.Executor,
			QueueShardSelector: shardSelector,
			Broadcaster:        broadcaster,
			RealtimeJWTSecret:  consts.DevServerRealtimeJWTSecret,
		})
	})

	// ds.opts.Config.EventStream.Service.TopicName()

	core, err := coreapi.NewCoreApi(coreapi.Options{
		Data:          ds.Data,
		Config:        ds.Opts.Config,
		Logger:        logger.From(ctx),
		Runner:        ds.Runner,
		Tracker:       ds.Tracker,
		State:         ds.State,
		Queue:         ds.Queue,
		EventHandler:  ds.HandleEvent,
		Executor:      ds.Executor,
		HistoryReader: memory_reader.NewReader(),
		ConnectOpts: connectv0.Opts{
			GroupManager:            connectionManager,
			ConnectManager:          connectionManager,
			ConnectResponseNotifier: gatewayProxy,
			Signer:                  auth.NewJWTSessionTokenSigner(consts.DevServerConnectJwtSecret),
			RequestAuther:           ds,
			ConnectGatewayRetriever: ds,
			Dev:                     true,
			ConnectionLimiter:       ds,
		},
	})
	if err != nil {
		return err
	}

	connGateway := connect.NewConnectGatewayService(
		connect.WithConnectionStateManager(connectionManager),
		connect.WithRequestReceiver(gatewayProxy),
		connect.WithGatewayAuthHandler(auth.NewJWTAuthHandler(consts.DevServerConnectJwtSecret)),
		connect.WithAppLoader(dbcqrs),
		connect.WithDev(),
		connect.WithGatewayPublicPort(opts.ConnectGatewayPort),
		connect.WithApiBaseUrl(fmt.Sprintf("http://127.0.0.1:%d", opts.Config.EventAPI.Port)),
		connect.WithLifeCycles(
			[]connect.ConnectGatewayLifecycleListener{
				lifecycles.NewHistoryLifecycle(dbcqrs),
			}),
	)
	connRouter := connect.NewConnectMessageRouterService(connectionManager, gatewayProxy)

	// Create a new data API directly in the devserver.  This allows us to inject
	// the data API into the dev server port, providing a single router for the dev
	// server UI, events, and API for loading data.
	//
	// Merge the dev server API (for handling files & registration) with the data
	// API into the event API router.

	mounts := []api.Mount{
		{At: "/", Router: devAPI},
		{At: "/v0", Router: core.Router},
		{At: "/debug", Handler: middleware.Profiler()},
	}

	if testapi.ShouldEnable() {
		mounts = append(mounts, api.Mount{At: "/test", Handler: testapi.New(testapi.Options{
			QueueShardSelector: shardSelector,
			Queue:              rq,
			Executor:           exec,
			StateManager:       smv2,
		})})
	}

	ds.Apiservice = api.NewService(api.APIServiceOptions{
		Config:         ds.Opts.Config,
		Mounts:         mounts,
		LocalEventKeys: opts.EventKeys,
	})

	svcs := []service.Service{ds, runner, executorSvc, ds.Apiservice}
	svcs = append(svcs, connGateway, connRouter)
	return service.StartAll(ctx, svcs...)
}

func createInmemoryRedis(ctx context.Context, tick time.Duration) (rueidis.Client, error) {
	r := miniredis.NewMiniRedis()
	_ = r.Start()
	rc, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress:  []string{r.Addr()},
		DisableCache: true,
	})
	if err != nil {
		return nil, err
	}

	// If tick is lower than the default, tick every 50ms.  This lets us save
	// CPU for standard dev-server testing.
	poll := time.Second
	if tick < DefaultTickDuration {
		poll = time.Millisecond * 50
	}

	go func() {
		for range time.Tick(poll) {
			r.FastForward(poll)
		}
	}()
	return rc, nil
}

func createConnectPubSubRedis() rueidis.ClientOption {
	r := miniredis.NewMiniRedis()
	_ = r.Start()
	return rueidis.ClientOption{
		InitAddress:  []string{r.Addr()},
		DisableCache: true,
	}
}

func getSendingEventHandler(ctx context.Context, pb pubsub.Publisher, topic string) execution.HandleSendingEvent {
	return func(ctx context.Context, evt event.Event, item queue.Item) error {
		trackedEvent := event.NewOSSTrackedEvent(evt)
		byt, err := json.Marshal(trackedEvent)
		if err != nil {
			return fmt.Errorf("error marshalling invocation event: %w", err)
		}

		carrier := itrace.NewTraceCarrier()
		itrace.UserTracer().Propagator().Inject(ctx, propagation.MapCarrier(carrier.Context))

		err = pb.Publish(
			ctx,
			topic,
			pubsub.Message{
				Name:      event.EventReceivedName,
				Data:      string(byt),
				Timestamp: time.Now(),
				Metadata: map[string]any{
					consts.OtelPropagationKey: carrier,
				},
			},
		)
		if err != nil {
			return fmt.Errorf("error publishing invocation event: %w", err)
		}

		return nil
	}
}

func getInvokeFailHandler(ctx context.Context, pb pubsub.Publisher, topic string) execution.InvokeFailHandler {
	return func(ctx context.Context, opts execution.InvokeFailHandlerOpts, evts []event.Event) error {
		eg := errgroup.Group{}

		for _, e := range evts {
			evt := e
			eg.Go(func() error {
				trackedEvent := event.NewOSSTrackedEvent(evt)
				byt, err := json.Marshal(trackedEvent)
				if err != nil {
					return fmt.Errorf("error marshalling function finished event: %w", err)
				}

				err = pb.Publish(
					ctx,
					topic,
					pubsub.Message{
						Name:      event.EventReceivedName,
						Data:      string(byt),
						Timestamp: trackedEvent.GetEvent().Time(),
					},
				)
				if err != nil {
					return fmt.Errorf("error publishing function finished event: %w", err)
				}

				return nil
			})
		}

		return eg.Wait()
	}
}
