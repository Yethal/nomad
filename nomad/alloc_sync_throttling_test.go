package nomad

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ryanuber/columnize"
	"golang.org/x/exp/maps"

	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
)

type throttleTestNodeHandler struct {
	// This represents the state store
	store      map[string]*structs.Allocation
	storeIndex uint64
	storeLock  sync.Mutex

	// Historical data
	updatesLastBatchSize        int
	updatesLastBatchNumClients  int
	updatesLastBatchTimeToWrite time.Duration
	history                     []*batchHistory

	// note: currently the Node.UpdateAlloc handler queues up all updates, even
	// if they're redundant for a particular Allocation
	updates        []*structs.Allocation
	updatesMap     map[string]*structs.Allocation
	updatedClients map[string]struct{}
	updateFuture   *structs.BatchFuture
	updateTimer    *time.Timer
	updatesLock    sync.Mutex // must be released in RPC handler

	serverBatchUpdateInterval time.Duration
	serverPerWrite            time.Duration
	serverBaseWriteLatency    time.Duration
}

type batchHistory struct {
	lastBatchSize        int
	lastBatchNumClients  int
	lastBatchTimeToWrite time.Duration
}

type throttleTestConfig struct {
	name                        string
	numClients                  int
	allocsPerClient             int
	clientBackoffThreshold      int
	clientDynamicBackoffLast    bool
	clientDynamicBackoffCurrent bool

	serverBatchUpdateInterval     time.Duration
	serverPerWrite                time.Duration
	serverBaseWriteLatency        time.Duration
	clientBatchUpdateInterval     time.Duration
	clientAllocEventsBaseInterval time.Duration
}

func newThrottleTestNodeHandler(cfg *throttleTestConfig) *throttleTestNodeHandler {
	return &throttleTestNodeHandler{
		store:                     map[string]*structs.Allocation{},
		updates:                   []*structs.Allocation{},
		updatesMap:                map[string]*structs.Allocation{},
		updatedClients:            map[string]struct{}{},
		serverBatchUpdateInterval: cfg.serverBatchUpdateInterval,
		serverPerWrite:            cfg.serverPerWrite,
		serverBaseWriteLatency:    cfg.serverBaseWriteLatency,
		history:                   []*batchHistory{},
	}
}

func (n *throttleTestNodeHandler) NodeUpdateAlloc(req *structs.AllocUpdateRequest, reply *throttleTestResponse) error {
	if len(req.Alloc) == 0 {
		return fmt.Errorf("no allocs sent in update")
	}

	n.updatesLock.Lock()
	n.updates = append(n.updates, req.Alloc...)

	for _, alloc := range req.Alloc {
		n.updatedClients[alloc.NodeID] = struct{}{}
		//		n.updatesMap[alloc.ID] = alloc
	}

	// This code is lifted from the Node.UpdateAlloc handler
	future := n.updateFuture
	if future == nil {
		future = structs.NewBatchFuture()
		n.updateFuture = future
		n.updateTimer = time.AfterFunc(n.serverBatchUpdateInterval, func() {
			n.updatesLock.Lock()
			updates := n.updates
			//updatesMap := n.updatesMap
			updatedClients := n.updatedClients
			future := n.updateFuture

			// Assume future update patterns will be similar to
			// current batch and set cap appropriately to avoid
			// slice resizing.
			n.updates = make([]*structs.Allocation, 0, len(updates))
			//n.updatesMap = make(map[string]*structs.Allocation, len(updatesMap))
			n.updatedClients = map[string]struct{}{}

			n.updateFuture = nil
			n.updateTimer = nil
			n.updatesLock.Unlock()

			// Perform the batch update
			n.batchUpdate(future, updates, len(updatedClients))
		})
	}

	reply.CurrentBatchSize = len(n.updatesMap)
	reply.CurrentBatchNumClients = len(n.updatedClients)
	reply.LastBatchSize = n.updatesLastBatchSize
	reply.LastBatchNumClients = n.updatesLastBatchNumClients
	reply.LastBatchTimeToWrite = n.updatesLastBatchTimeToWrite

	n.updatesLock.Unlock()

	if err := future.Wait(); err != nil {
		return err
	}

	reply.Index = future.Index()

	return nil
}

func (n *throttleTestNodeHandler) batchUpdate(future *structs.BatchFuture, updates []*structs.Allocation, numClients int) {
	if len(updates) == 0 {
		return
	}
	now := time.Now()

	// note: only one writer
	n.storeIndex++
	for _, update := range updates {
		n.store[update.ID] = update
	}

	// simulate the cost of writes
	time.Sleep(n.serverPerWrite * time.Duration(len(updates)))
	time.Sleep(helper.RandomStagger(n.serverBaseWriteLatency))

	n.updatesLastBatchSize = len(updates)
	n.updatesLastBatchNumClients = numClients

	elapsed := time.Since(now)
	n.updatesLastBatchTimeToWrite = elapsed
	n.history = append(n.history, &batchHistory{
		lastBatchSize:        n.updatesLastBatchSize,
		lastBatchNumClients:  n.updatesLastBatchNumClients,
		lastBatchTimeToWrite: elapsed,
	})

	future.Respond(n.storeIndex, nil)
}

type throttleTestClient struct {
	nodeID             string
	syncInterval       time.Duration
	allocEventInterval time.Duration
	allocs             []*structs.Allocation
	allocUpdates       chan *structs.Allocation

	backOffThreshold      int
	dynamicBackoffLast    bool
	dynamicBackoffCurrent bool

	responses []*throttleTestResponse
	srv       *throttleTestNodeHandler
}

func newThrottleTestClient(srv *throttleTestNodeHandler, cfg *throttleTestConfig) *throttleTestClient {
	c := &throttleTestClient{
		nodeID:                uuid.Generate(),
		syncInterval:          cfg.clientBatchUpdateInterval,
		allocEventInterval:    cfg.clientAllocEventsBaseInterval,
		allocUpdates:          make(chan *structs.Allocation, 64),
		backOffThreshold:      cfg.clientBackoffThreshold,
		dynamicBackoffLast:    cfg.clientDynamicBackoffLast,
		dynamicBackoffCurrent: cfg.clientDynamicBackoffCurrent,
		responses:             []*throttleTestResponse{},
		srv:                   srv,
	}
	for i := 0; i < cfg.allocsPerClient; i++ {
		// build a bare minimum allocation for demonstration purposes
		c.allocs = append(c.allocs, &structs.Allocation{
			ID:           uuid.Generate(),
			NodeID:       c.nodeID,
			ClientStatus: structs.AllocClientStatusRunning,
			TaskStates:   map[string]*structs.TaskState{},
		})
	}
	return c
}

func (c *throttleTestClient) run(ctx context.Context) {
	go c.allocSync(ctx)
	go c.runAllocs(ctx)
}

func (c *throttleTestClient) runAllocs(ctx context.Context) {
	for _, alloc := range c.allocs {
		go func(alloc *structs.Allocation) {
			//interval := helper.RandomStagger(c.allocEventInterval)
			interval := c.allocEventInterval
			eventTicker := time.NewTicker(interval)
			defer eventTicker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-eventTicker.C:
					c.allocUpdates <- alloc // ignoring dedupe for now
					//interval = helper.RandomStagger(c.allocEventInterval) + 1
					eventTicker.Reset(interval)
				}
			}
		}(alloc)
	}
}

func (c *throttleTestClient) allocSync(ctx context.Context) {
	// sleep at the start to help randomize the client updates
	waitInterval := helper.RandomStagger(c.syncInterval)
	time.Sleep(waitInterval)

	syncInterval := c.syncInterval
	syncTicker := time.NewTicker(syncInterval)
	updates := make(map[string]*structs.Allocation)
	for {
		select {
		case <-ctx.Done():
			syncTicker.Stop()
			return
		case alloc := <-c.allocUpdates:
			updates[alloc.ID] = alloc
		case <-syncTicker.C:
			if len(updates) == 0 {
				continue
			}

			args := &structs.AllocUpdateRequest{Alloc: maps.Values(updates)}
			var resp throttleTestResponse

			now := time.Now()
			err := c.srv.NodeUpdateAlloc(args, &resp)
			if err != nil { // don't clear, but backoff
				syncInterval = c.syncInterval + helper.RandomStagger(c.syncInterval)
				syncTicker.Reset(syncInterval)
				continue
			}

			resp.elapsedClientTime = time.Since(now)
			c.responses = append(c.responses, &resp)
			updates = make(map[string]*structs.Allocation, len(updates))

			syncInterval = c.syncInterval
			if c.backOffThreshold > 0 {
				if c.dynamicBackoffLast {
					if resp.LastBatchSize > c.backOffThreshold {
						syncInterval = c.syncInterval + (time.Duration(resp.LastBatchSize-c.backOffThreshold) * time.Millisecond)
					}
				} else if c.dynamicBackoffCurrent {
					if resp.CurrentBatchSize > c.backOffThreshold {
						syncInterval = c.syncInterval + (time.Duration(resp.CurrentBatchSize-c.backOffThreshold) * time.Millisecond)
					}
				} else {
					syncInterval = c.syncInterval + helper.RandomStagger(c.syncInterval)
				}
			}
			syncTicker.Reset(syncInterval)
		}
	}
}

type throttleTestRequest struct {
	Allocs []*structs.Allocation

	structs.WriteRequest
}

type throttleTestResponse struct {
	LastBatchSize          int
	LastBatchNumClients    int
	LastBatchTimeToWrite   time.Duration
	CurrentBatchSize       int
	CurrentBatchNumClients int

	elapsedClientTime time.Duration

	structs.WriteMeta
}

func batchStats(values []float64) (float64, float64, float64) {
	sumSq := float64(0)
	sum := float64(0)
	max := float64(0)

	for _, val := range values {
		sum += val
		if val > max {
			max = val
		}
		sumSq += val * val
	}
	mean := sum / float64(len(values))
	stddev := math.Sqrt((sumSq/float64(len(values)) - (mean * mean)))
	return mean, stddev, max
}

func TestAllocSyncThrottling(t *testing.T) {

	testWindow := time.Duration(10 * time.Second)
	numClients := 1000
	allocsPerClient := 100
	serverBatchUpdateInterval := time.Millisecond * 50
	clientBatchUpdateInterval := time.Millisecond * 200
	clientAllocEventsBaseInterval := time.Millisecond * 50
	perWrite := time.Microsecond * 100
	batchLatency := time.Millisecond * 5

	testCfgs := []*throttleTestConfig{

		{
			name: "default",
		},
		{
			name:                   "backoff_1000",
			clientBackoffThreshold: 1000,
		},
		{
			name:                     "backoff_1000_dynamic_last",
			clientBackoffThreshold:   1000,
			clientDynamicBackoffLast: true,
		},
		{
			name:                        "backoff_1000_dynamic_current",
			clientBackoffThreshold:      1000,
			clientDynamicBackoffCurrent: true,
		},

		{
			name:                   "backoff_500",
			clientBackoffThreshold: 500,
		},
		{
			name:                     "backoff_500_dynamic_last",
			clientBackoffThreshold:   500,
			clientDynamicBackoffLast: true,
		},
		{
			name:                        "backoff_500_dynamic_current",
			clientBackoffThreshold:      500,
			clientDynamicBackoffCurrent: true,
		},

		{
			name:                   "backoff_250",
			clientBackoffThreshold: 250,
		},
		{
			name:                     "backoff_250_dynamic_last",
			clientBackoffThreshold:   250,
			clientDynamicBackoffLast: true,
		},
		{
			name:                        "backoff_250_dynamic_current",
			clientBackoffThreshold:      250,
			clientDynamicBackoffCurrent: true,
		},
	}

	results := []string{
		"Backoff Threshold|Dynamic?|# Batches|Updates/Batch|Clients/Batch|Time/Batch (ms)|# Responses|Response Latency (ms)",
	}

	for _, cfg := range testCfgs {

		t.Run(cfg.name, func(t *testing.T) {
			cfg.numClients = numClients
			cfg.allocsPerClient = allocsPerClient
			cfg.serverBatchUpdateInterval = serverBatchUpdateInterval
			cfg.clientBatchUpdateInterval = clientBatchUpdateInterval
			cfg.clientAllocEventsBaseInterval = clientAllocEventsBaseInterval
			cfg.serverPerWrite = perWrite
			cfg.serverBaseWriteLatency = batchLatency
			srv := newThrottleTestNodeHandler(cfg)

			clients := []*throttleTestClient{}
			for i := 0; i < cfg.numClients; i++ {
				clients = append(clients, newThrottleTestClient(srv, cfg))
			}

			ctx, cancel := context.WithTimeout(context.TODO(), testWindow)
			defer cancel()
			for _, client := range clients {
				client.run(ctx)
			}

			<-ctx.Done()
			srv.updatesLock.Lock()
			defer srv.updatesLock.Unlock()
			result := resultFromHistory(cfg, srv.history, clients)
			results = append(results, result)
		})
	}

	fmt.Printf("# Clients: %d\n# Allocs: %v\nServer Batch Interval: %v\nClient Batch Interval: %v\nAlloc Event Interval: %v\n",
		numClients,
		allocsPerClient,
		serverBatchUpdateInterval,
		clientBatchUpdateInterval,
		clientAllocEventsBaseInterval)

	columnizeCfg := columnize.DefaultConfig()
	columnizeCfg.Glue = " | "
	fmt.Println(columnize.Format(results, columnizeCfg))
}

func resultFromHistory(cfg *throttleTestConfig, history []*batchHistory, clients []*throttleTestClient) string {

	batchSizes := helper.ConvertSlice(history, func(b *batchHistory) float64 {
		return float64(b.lastBatchSize)
	})
	batchClients := helper.ConvertSlice(history, func(b *batchHistory) float64 {
		return float64(b.lastBatchNumClients)
	})
	times := helper.ConvertSlice(history, func(b *batchHistory) float64 {
		return float64(b.lastBatchTimeToWrite.Milliseconds())
	})

	meanBatchSize, stdBatchSize, maxBatchSize := batchStats(batchSizes)
	meanBatchClients, stdBatchClients, maxBatchClients := batchStats(batchClients)
	meanBatchTime, stdBatchTimes, maxBatchTime := batchStats(times)

	clientResponses := 0
	clientLatencies := []float64{}

	for _, client := range clients {
		clientResponses += len(client.responses)
		for _, resp := range client.responses {
			clientLatencies = append(clientLatencies, float64(resp.elapsedClientTime.Milliseconds()))
		}
	}

	meanClientLatency, stdClientLatency, maxClientLatency := batchStats(clientLatencies)

	dynamicEnum := "no"
	if cfg.clientDynamicBackoffLast {
		dynamicEnum = "last"
	}
	if cfg.clientDynamicBackoffCurrent {
		dynamicEnum = "current"
	}

	return fmt.Sprintf("%d|%s|%d|%.0f ± %.0f (max %.0f)|%.0f ± %.0f (max %.0f)|%.0f ± %.0f (max %.0f)|%d|%.0f ± %.0f (max %.0f)",
		cfg.clientBackoffThreshold,
		dynamicEnum,
		len(history),
		meanBatchSize,
		stdBatchSize,
		maxBatchSize,
		meanBatchClients,
		stdBatchClients,
		maxBatchClients,
		meanBatchTime,
		stdBatchTimes,
		maxBatchTime,
		clientResponses,
		meanClientLatency,
		stdClientLatency,
		maxClientLatency,
	)

}
