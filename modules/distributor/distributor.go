package distributor

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cortexproject/cortex/pkg/ring"
	ring_client "github.com/cortexproject/cortex/pkg/ring/client"
	cortex_util "github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/limiter"
	"github.com/cortexproject/cortex/pkg/util/services"

	"github.com/go-kit/kit/log/level"
	opentelemetry_proto_trace_v1 "github.com/open-telemetry/opentelemetry-proto/gen/go/trace/v1"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
	uber_atomic "go.uber.org/atomic"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/grafana/tempo/modules/distributor/receiver"
	ingester_client "github.com/grafana/tempo/modules/ingester/client"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/pkg/util/validation"
)

var (
	metricIngesterAppends = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_appends_total",
		Help:      "The total number of batch appends sent to ingesters.",
	}, []string{"ingester"})
	metricIngesterAppendFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_append_failures_total",
		Help:      "The total number of failed batch appends sent to ingesters.",
	}, []string{"ingester"})
	metricSpansIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_spans_received_total",
		Help:      "The total number of spans received per tenant",
	}, []string{"tenant"})
	metricTracesPerBatch = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "tempo",
		Name:      "distributor_traces_per_batch",
		Help:      "The number of traces in each batch",
		Buckets:   prometheus.LinearBuckets(0, 3, 5),
	})
	metricIngesterClients = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_clients",
		Help:      "The current number of ingester clients.",
	})

	readinessProbeSuccess = []byte("Ready")
)

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	cfg           Config
	clientCfg     ingester_client.Config
	ingestersRing ring.ReadRing
	overrides     *validation.Overrides
	pool          *ring_client.Pool

	// receiver shims
	receivers receiver.Receivers

	// The global rate limiter requires a distributors ring to count
	// the number of healthy instances.
	distributorsRing *ring.Lifecycler

	// Per-user rate limiter.
	ingestionRateLimiter *limiter.RateLimiter
}

// New a distributor creates.
func New(cfg Config, clientCfg ingester_client.Config, ingestersRing ring.ReadRing, overrides *validation.Overrides, authEnabled bool) (*Distributor, error) {
	factory := cfg.factory
	if factory == nil {
		factory = func(addr string) (ring_client.PoolClient, error) {
			return ingester_client.New(addr, clientCfg)
		}
	}

	// Create the configured ingestion rate limit strategy (local or global).
	var ingestionRateStrategy limiter.RateLimiterStrategy
	var distributorsRing *ring.Lifecycler

	if overrides.IngestionRateStrategy() == validation.GlobalIngestionRateStrategy {
		var err error
		distributorsRing, err = ring.NewLifecycler(cfg.DistributorRing.ToLifecyclerConfig(), nil, "distributor", ring.DistributorRingKey, false, prometheus.DefaultRegisterer)
		if err != nil {
			return nil, err
		}

		err = services.StartAndAwaitRunning(context.Background(), distributorsRing)
		if err != nil {
			return nil, err
		}

		ingestionRateStrategy = newGlobalIngestionRateStrategy(overrides, distributorsRing)
	} else {
		ingestionRateStrategy = newLocalIngestionRateStrategy(overrides)
	}

	d := &Distributor{
		cfg:              cfg,
		clientCfg:        clientCfg,
		ingestersRing:    ingestersRing,
		distributorsRing: distributorsRing,
		overrides:        overrides,
		pool: ring_client.NewPool("distributor_pool",
			clientCfg.PoolConfig,
			ring_client.NewRingServiceDiscovery(ingestersRing),
			factory,
			metricIngesterClients,
			cortex_util.Logger),
		ingestionRateLimiter: limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second),
	}

	if len(cfg.Receivers) > 0 {
		var err error
		d.receivers, err = receiver.New(cfg.Receivers, d, authEnabled)
		if err != nil {
			return nil, err
		}
		err = d.receivers.Start()
		if err != nil {
			return nil, err
		}
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFunc()
	err := services.StartAndAwaitRunning(ctx, d.pool)
	if err != nil {
		return nil, fmt.Errorf("Failed to start distributor pool %w", err)
	}

	return d, nil
}

func (d *Distributor) Stop() {
	if d.distributorsRing != nil {
		err := services.StopAndAwaitTerminated(context.Background(), d.distributorsRing)
		if err != nil {
			level.Error(cortex_util.Logger).Log("msg", "error stopping receivers", "error", err)
		}
	}

	if d.receivers != nil {
		err := d.receivers.Shutdown()
		if err != nil {
			level.Error(cortex_util.Logger).Log("msg", "error stopping receivers", "error", err)
		}
	}

	if d.pool != nil {
		ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFunc()

		err := services.StopAndAwaitTerminated(ctx, d.pool)
		if err != nil {
			level.Error(cortex_util.Logger).Log("msg", "error stopping pool", "error", err)
		}
	}
}

// ReadinessHandler is used to indicate to k8s when the distributor is ready.
// Returns 200 when the distributor is ready, 500 otherwise.
func (d *Distributor) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	_, err := d.ingestersRing.GetAll(ring.Write)
	if err != nil {
		http.Error(w, "Not ready: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(readinessProbeSuccess); err != nil {
		level.Error(cortex_util.Logger).Log("msg", "error writing success message", "error", err)
	}
}

// Push a set of streams.
func (d *Distributor) Push(ctx context.Context, req *tempopb.PushRequest) (*tempopb.PushResponse, error) {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}

	// Track metrics.
	if req.Batch == nil {
		return &tempopb.PushResponse{}, nil
	}
	spanCount := 0
	for _, ils := range req.Batch.InstrumentationLibrarySpans {
		spanCount += len(ils.Spans)
	}
	if spanCount == 0 {
		return &tempopb.PushResponse{}, nil
	}
	metricSpansIngested.WithLabelValues(userID).Add(float64(spanCount))

	now := time.Now()
	if !d.ingestionRateLimiter.AllowN(now, userID, spanCount) {
		// Return a 4xx here to have the client discard the data and not retry. If a client
		// is sending too much data consistently we will unlikely ever catch up otherwise.
		validation.DiscardedSamples.WithLabelValues(validation.RateLimited, userID).Add(float64(spanCount))
		return nil, httpgrpc.Errorf(http.StatusTooManyRequests, "ingestion rate limit (%d bytes) exceeded while adding %d spans", int(d.ingestionRateLimiter.Limit(now, userID)), spanCount)
	}

	requestsByIngester, err := d.routeRequest(req, userID, spanCount)
	if err != nil {
		return nil, err
	}

	atomicErr := uber_atomic.NewError(nil)
	done := make(chan struct{})
	wg := sync.WaitGroup{}
	for ingester, reqs := range requestsByIngester {
		wg.Add(1)
		go func(ingesterAddr string, reqs []*tempopb.PushRequest) {
			// Use a background context to make sure all ingesters get samples even if we return early
			localCtx, cancel := context.WithTimeout(context.Background(), d.clientCfg.RemoteTimeout)
			defer cancel()
			localCtx = user.InjectOrgID(localCtx, userID)
			if sp := opentracing.SpanFromContext(ctx); sp != nil {
				localCtx = opentracing.ContextWithSpan(localCtx, sp)
			}
			err := d.sendSamples(localCtx, ingesterAddr, reqs)
			atomicErr.Store(err)
			wg.Done()
		}(ingester, reqs)
	}

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	select {
	case <-done:
		return &tempopb.PushResponse{}, atomicErr.Load()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TODO taken from Loki taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamples(ctx context.Context, ingesterAddr string, batches []*tempopb.PushRequest) error {
	var warning error

	for _, b := range batches {
		err := d.sendSamplesErr(ctx, ingesterAddr, b)

		if err != nil {
			warning = err
		}
	}

	return warning
}

// TODO taken from Loki taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamplesErr(ctx context.Context, ingesterAddr string, req *tempopb.PushRequest) error {
	c, err := d.pool.GetClientFor(ingesterAddr)
	if err != nil {
		return err
	}

	_, err = c.(tempopb.PusherClient).Push(ctx, req)
	metricIngesterAppends.WithLabelValues(ingesterAddr).Inc()
	if err != nil {
		metricIngesterAppendFailures.WithLabelValues(ingesterAddr).Inc()
	}
	return err
}

// Check implements the grpc healthcheck
func (*Distributor) Check(_ context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (d *Distributor) routeRequest(req *tempopb.PushRequest, userID string, spanCount int) (map[string][]*tempopb.PushRequest, error) {
	const maxExpectedReplicationSet = 3 // 3.  b/c frigg it
	var descs [maxExpectedReplicationSet]ring.IngesterDesc

	requestsByTrace := make(map[uint32]*tempopb.PushRequest)
	spansByILS := make(map[string]*opentelemetry_proto_trace_v1.InstrumentationLibrarySpans)

	for _, ils := range req.Batch.InstrumentationLibrarySpans {
		for _, span := range ils.Spans {
			if !validation.ValidTraceID(span.TraceId) {
				return nil, httpgrpc.Errorf(http.StatusBadRequest, "trace ids must be 128 bit")
			}

			traceKey := util.TokenFor(userID, span.TraceId)
			ilsKey := strconv.Itoa(int(traceKey))
			if ils.InstrumentationLibrary != nil {
				ilsKey = ilsKey + ils.InstrumentationLibrary.Name + ils.InstrumentationLibrary.Version
			}
			existingILS, ok := spansByILS[ilsKey]
			if !ok {
				existingILS = &opentelemetry_proto_trace_v1.InstrumentationLibrarySpans{
					InstrumentationLibrary: ils.InstrumentationLibrary,
					Spans:                  make([]*opentelemetry_proto_trace_v1.Span, 0, spanCount), // assume most spans belong to the same trace and ils
				}
				spansByILS[ilsKey] = existingILS
			}
			existingILS.Spans = append(existingILS.Spans, span)

			// if we found an ILS we assume its already part of a request and can go to the next span
			if ok {
				continue
			}

			existingReq, ok := requestsByTrace[traceKey]
			if !ok {
				existingReq = &tempopb.PushRequest{
					Batch: &opentelemetry_proto_trace_v1.ResourceSpans{
						InstrumentationLibrarySpans: make([]*opentelemetry_proto_trace_v1.InstrumentationLibrarySpans, 0, len(req.Batch.InstrumentationLibrarySpans)), // assume most spans belong to the same trace
						Resource:                    req.Batch.Resource,
					},
				}
				requestsByTrace[traceKey] = existingReq
			}
			existingReq.Batch.InstrumentationLibrarySpans = append(existingReq.Batch.InstrumentationLibrarySpans, existingILS)
		}
	}

	metricTracesPerBatch.Observe(float64(len(requestsByTrace)))
	requestsByIngester := make(map[string][]*tempopb.PushRequest)
	for key, routedReq := range requestsByTrace {
		// now map to ingesters
		replicationSet, err := d.ingestersRing.Get(key, ring.Write, descs[:0])
		if err != nil {
			return nil, err
		}
		for _, ingester := range replicationSet.Ingesters {
			requestsByIngester[ingester.Addr] = append(requestsByIngester[ingester.Addr], routedReq)
		}
	}

	return requestsByIngester, nil
}