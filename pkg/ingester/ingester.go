package ingester

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cortexproject/cortex/pkg/chunk"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/util"

	"github.com/joe-elliott/frigg/pkg/ingester/client"
	"github.com/joe-elliott/frigg/pkg/friggpb"
	"github.com/joe-elliott/frigg/pkg/util/validation"
)

// ErrReadOnly is returned when the ingester is shutting down and a push was
// attempted.
var ErrReadOnly = errors.New("Ingester is shutting down")

var readinessProbeSuccess = []byte("Ready")

var flushQueueLength = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "cortex_ingester_flush_queue_length",
	Help: "The total number of series pending in the flush queue.",
})

// Config for an ingester.
type Config struct {
	LifecyclerConfig ring.LifecyclerConfig `yaml:"lifecycler,omitempty"`

	// Config for transferring chunks.
	MaxTransferRetries int `yaml:"max_transfer_retries,omitempty"`

	ConcurrentFlushes int           `yaml:"concurrent_flushes"`
	FlushCheckPeriod  time.Duration `yaml:"flush_check_period"`
	FlushOpTimeout    time.Duration `yaml:"flush_op_timeout"`
	RetainPeriod      time.Duration `yaml:"trace_retain_period"`
	MaxTraceIdle      time.Duration `yaml:"trace_idle_period"`

	// For testing, you can override the address and ID of this ingester.
	ingesterClientFactory func(cfg client.Config, addr string) (grpc_health_v1.HealthClient, error)
}

// RegisterFlags registers the flags.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.LifecyclerConfig.RegisterFlags(f)

	f.IntVar(&cfg.MaxTransferRetries, "ingester.max-transfer-retries", 10, "Number of times to try and transfer chunks before falling back to flushing. If set to 0 or negative value, transfers are disabled.")
	f.IntVar(&cfg.ConcurrentFlushes, "ingester.concurrent-flushed", 16, "")
	f.DurationVar(&cfg.FlushCheckPeriod, "ingester.flush-check-period", 30*time.Second, "")
	f.DurationVar(&cfg.FlushOpTimeout, "ingester.flush-op-timeout", 10*time.Second, "")
	f.DurationVar(&cfg.RetainPeriod, "ingester.chunks-retain-period", 15*time.Minute, "")
	f.DurationVar(&cfg.MaxChunkIdle, "ingester.chunks-idle-period", 30*time.Minute, "")
	f.IntVar(&cfg.BlockSize, "ingester.chunks-block-size", 256*1024, "")
	f.IntVar(&cfg.TargetChunkSize, "ingester.chunk-target-size", 0, "")
	f.StringVar(&cfg.ChunkEncoding, "ingester.chunk-encoding", chunkenc.EncGZIP.String(), fmt.Sprintf("The algorithm to use for compressing chunk. (%s)", chunkenc.SupportedEncoding()))
	f.DurationVar(&cfg.SyncPeriod, "ingester.sync-period", 0, "How often to cut chunks to synchronize ingesters.")
	f.Float64Var(&cfg.SyncMinUtilization, "ingester.sync-min-utilization", 0, "Minimum utilization of chunk when doing synchronization.")
}

// Ingester builds chunks for incoming log streams.
type Ingester struct {
	cfg          Config
	clientConfig client.Config

	shutdownMtx  sync.Mutex // Allows processes to grab a lock and prevent a shutdown
	instancesMtx sync.RWMutex
	instances    map[string]*instance
	readonly     bool

	lifecycler *ring.Lifecycler
	store      ChunkStore

	done     sync.WaitGroup
	quit     chan struct{}
	quitting chan struct{}

	// One queue per flush thread.  Fingerprint is used to
	// pick a queue.
	flushQueues     []*util.PriorityQueue
	flushQueuesDone sync.WaitGroup

	limiter *Limiter
}

// ChunkStore is the interface we need to store chunks.
type ChunkStore interface {
	Put(ctx context.Context, chunks []chunk.Chunk) error
}

// New makes a new Ingester.
func New(cfg Config, clientConfig client.Config, store ChunkStore, limits *validation.Overrides) (*Ingester, error) {
	if cfg.ingesterClientFactory == nil {
		cfg.ingesterClientFactory = client.New
	}
	enc, err := chunkenc.ParseEncoding(cfg.ChunkEncoding)
	if err != nil {
		return nil, err
	}

	i := &Ingester{
		cfg:          cfg,
		clientConfig: clientConfig,
		instances:    map[string]*instance{},
		store:        store,
		quit:         make(chan struct{}),
		flushQueues:  make([]*util.PriorityQueue, cfg.ConcurrentFlushes),
		quitting:     make(chan struct{}),
	}

	i.flushQueuesDone.Add(cfg.ConcurrentFlushes)
	for j := 0; j < cfg.ConcurrentFlushes; j++ {
		i.flushQueues[j] = util.NewPriorityQueue(flushQueueLength)
		go i.flushLoop(j)
	}

	i.lifecycler, err = ring.NewLifecycler(cfg.LifecyclerConfig, i, "ingester", ring.IngesterRingKey)
	if err != nil {
		return nil, err
	}

	i.lifecycler.Start()

	// Now that the lifecycler has been created, we can create the limiter
	// which depends on it.
	i.limiter = NewLimiter(limits, i.lifecycler, cfg.LifecyclerConfig.RingConfig.ReplicationFactor)

	i.done.Add(1)
	go i.loop()

	return i, nil
}

func (i *Ingester) loop() {
	defer i.done.Done()

	flushTicker := time.NewTicker(i.cfg.FlushCheckPeriod)
	defer flushTicker.Stop()

	for {
		select {
		case <-flushTicker.C:
			i.sweepUsers(false)

		case <-i.quit:
			return
		}
	}
}

// Shutdown stops the ingester.
func (i *Ingester) Shutdown() {
	close(i.quit)
	i.done.Wait()

	i.lifecycler.Shutdown()
}

// Stopping helps cleaning up resources before actual shutdown
func (i *Ingester) Stopping() {
	close(i.quitting)
}

// Push implements friggpb.Pusher.
func (i *Ingester) Push(ctx context.Context, req *friggpb.PushRequest) (*friggpb.PushResponse, error) {
	instanceID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	} else if i.readonly {
		return nil, ErrReadOnly
	}

	instance := i.getOrCreateInstance(instanceID)
	err = instance.Push(ctx, req)
	return &friggpb.PushResponse{}, err
}

// Check implements grpc_health_v1.HealthCheck.
func (*Ingester) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// Watch implements grpc_health_v1.HealthCheck.
func (*Ingester) Watch(*grpc_health_v1.HealthCheckRequest, grpc_health_v1.Health_WatchServer) error {
	return nil
}

// ReadinessHandler is used to indicate to k8s when the ingesters are ready for
// the addition removal of another ingester. Returns 200 when the ingester is
// ready, 500 otherwise.
func (i *Ingester) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	if err := i.lifecycler.CheckReady(r.Context()); err != nil {
		http.Error(w, "Not ready: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(readinessProbeSuccess); err != nil {
		level.Error(util.Logger).Log("msg", "error writing success message", "error", err)
	}
}

func (i *Ingester) getOrCreateInstance(instanceID string) *instance {
	inst, ok := i.getInstanceByID(instanceID)
	if ok {
		return inst
	}

	i.instancesMtx.Lock()
	defer i.instancesMtx.Unlock()
	inst, ok = i.instances[instanceID]
	if !ok {
		inst = newInstance(instanceID, i.factory, i.limiter, i.cfg.SyncPeriod, i.cfg.SyncMinUtilization)
		i.instances[instanceID] = inst
	}
	return inst
}

func (i *Ingester) getInstanceByID(id string) (*instance, bool) {
	i.instancesMtx.RLock()
	defer i.instancesMtx.RUnlock()

	inst, ok := i.instances[id]
	return inst, ok
}

func (i *Ingester) getInstances() []*instance {
	i.instancesMtx.RLock()
	defer i.instancesMtx.RUnlock()

	instances := make([]*instance, 0, len(i.instances))
	for _, instance := range i.instances {
		instances = append(instances, instance)
	}
	return instances
}
