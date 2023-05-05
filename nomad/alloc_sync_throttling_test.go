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
	historyLock                 sync.RWMutex

	// note: currently the Node.UpdateAlloc handler queues up all updates, even
	// if they're redundant for a particular Allocation
	updates        []*structs.Allocation
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
	name                          string
	numClients                    int
	allocsPerClient               int
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
	}

	// This code is lifted from the Node.UpdateAlloc handler
	future := n.updateFuture
	if future == nil {
		future = structs.NewBatchFuture()
		n.updateFuture = future
		n.updateTimer = time.AfterFunc(n.serverBatchUpdateInterval, func() {
			n.updatesLock.Lock()
			updates := n.updates
			updatedClients := n.updatedClients
			future := n.updateFuture

			// Assume future update patterns will be similar to
			// current batch and set cap appropriately to avoid
			// slice resizing.
			n.updates = make([]*structs.Allocation, 0, len(updates))
			n.updatedClients = map[string]struct{}{}

			n.updateFuture = nil
			n.updateTimer = nil
			n.updatesLock.Unlock()

			now := time.Now()

			// Perform the batch update
			n.batchUpdate(future, updates, updatedClients)

			elapsed := time.Since(now)
			n.historyLock.Lock()
			defer n.historyLock.Unlock()
			n.updatesLastBatchSize = len(updates)
			n.updatesLastBatchNumClients = len(maps.Keys(updatedClients))
			n.updatesLastBatchTimeToWrite = elapsed
			n.history = append(n.history, &batchHistory{
				lastBatchSize:        n.updatesLastBatchSize,
				lastBatchNumClients:  n.updatesLastBatchNumClients,
				lastBatchTimeToWrite: elapsed,
			})

		})
	}

	reply.CurrentBatchSize = len(n.updates)
	reply.CurrentBatchNumClients = len(maps.Keys(n.updatedClients))
	n.updatesLock.Unlock()

	n.historyLock.RLock()
	reply.LastBatchSize = n.updatesLastBatchSize
	reply.LastBatchNumClients = n.updatesLastBatchNumClients
	reply.LastBatchTimeToWrite = n.updatesLastBatchTimeToWrite
	n.historyLock.RUnlock()

	if err := future.Wait(); err != nil {
		return err
	}

	reply.Index = future.Index()

	return nil
}

func (n *throttleTestNodeHandler) batchUpdate(future *structs.BatchFuture, updates []*structs.Allocation, updatedClients map[string]struct{}) {
	if len(updates) == 0 {
		return
	}

	n.storeLock.Lock()
	defer n.storeLock.Unlock()

	n.storeIndex++
	for _, update := range updates {
		n.store[update.ID] = update
	}

	// simulate the cost of writes
	time.Sleep(n.serverPerWrite * time.Duration(len(updates)))
	time.Sleep(helper.RandomStagger(n.serverBaseWriteLatency))

	future.Respond(n.storeIndex, nil)
}

type throttleTestClient struct {
	nodeID             string
	syncInterval       time.Duration
	allocEventInterval time.Duration
	allocs             []*structs.Allocation
	allocUpdates       chan *structs.Allocation
	responses          []*throttleTestResponse
	srv                *throttleTestNodeHandler
}

func newThrottleTestClient(srv *throttleTestNodeHandler, cfg *throttleTestConfig) *throttleTestClient {
	c := &throttleTestClient{
		nodeID:             uuid.Generate(),
		syncInterval:       cfg.clientBatchUpdateInterval,
		allocEventInterval: cfg.clientAllocEventsBaseInterval,
		allocUpdates:       make(chan *structs.Allocation, 64),
		responses:          []*throttleTestResponse{},
		srv:                srv,
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
			interval := helper.RandomStagger(c.allocEventInterval)
			eventTicker := time.NewTicker(interval)
			defer eventTicker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-eventTicker.C:
					c.allocUpdates <- alloc // ignoring dedupe for now
					interval = helper.RandomStagger(c.allocEventInterval) + 1
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

			sync := make([]*structs.Allocation, 0, len(updates))
			for _, alloc := range updates {
				sync = append(sync, alloc)
			}

			args := &structs.AllocUpdateRequest{Alloc: sync}
			var resp throttleTestResponse

			err := c.srv.NodeUpdateAlloc(args, &resp)
			if err != nil { // don't clear, but backoff
				syncTicker.Stop()
				syncInterval = c.syncInterval + helper.RandomStagger(c.syncInterval)
				syncTicker = time.NewTicker(syncInterval)
				continue
			}
			c.responses = append(c.responses, &resp)

			updates = make(map[string]*structs.Allocation, len(updates))
			syncTicker.Stop()
			syncTicker = time.NewTicker(syncInterval)
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
	perWrite := time.Microsecond * 100
	batchLatency := time.Millisecond * 5

	testCfgs := []*throttleTestConfig{
		{
			name:                          "2000_clients_50_allocs_per",
			numClients:                    2000,
			allocsPerClient:               50,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
		{
			name:                          "2000_clients_100_allocs_per",
			numClients:                    2000,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
		{
			name:                          "2000_clients_100_allocs_per_slow",
			numClients:                    2000,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 500,
			clientAllocEventsBaseInterval: time.Millisecond * 300,
		},
		{
			name:                          "2000_clients_100_allocs_per_fast_srv",
			numClients:                    2000,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 15,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},

		{
			name:                          "1000_clients_50_allocs_per",
			numClients:                    1000,
			allocsPerClient:               50,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
		{
			name:                          "1000_clients_100_allocs_per",
			numClients:                    1000,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
		{
			name:                          "1000_clients_100_allocs_per_slow",
			numClients:                    1000,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 500,
			clientAllocEventsBaseInterval: time.Millisecond * 300,
		},
		{
			name:                          "1000_clients_100_allocs_per_fast_srv",
			numClients:                    1000,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 15,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},

		{
			name:                          "500_clients_50_allocs_per",
			numClients:                    500,
			allocsPerClient:               50,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
		{
			name:                          "500_clients_100_allocs_per",
			numClients:                    500,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
		{
			name:                          "500_clients_100_allocs_per_slow",
			numClients:                    500,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 50,
			clientBatchUpdateInterval:     time.Millisecond * 500,
			clientAllocEventsBaseInterval: time.Millisecond * 300,
		},
		{
			name:                          "500_clients_100_allocs_per_fast_srv",
			numClients:                    500,
			allocsPerClient:               100,
			serverBatchUpdateInterval:     time.Millisecond * 15,
			clientBatchUpdateInterval:     time.Millisecond * 200,
			clientAllocEventsBaseInterval: time.Millisecond * 50,
		},
	}

	results := []string{
		"# Clients|Allocs Per|Server Batch|Client Batch|Alloc Events|# Batches|Updates/Batch|Clients/Batch|Time/Batch (ms)",
	}

	for _, cfg := range testCfgs {

		t.Run(cfg.name, func(t *testing.T) {
			cfg.serverPerWrite = perWrite
			cfg.serverBaseWriteLatency = batchLatency
			srv := newThrottleTestNodeHandler(cfg)

			clients := []*throttleTestClient{}
			for i := 0; i < cfg.numClients; i++ {
				clients = append(clients, newThrottleTestClient(srv, cfg))
			}

			ctx, cancel := context.WithTimeout(context.TODO(), time.Duration(5*time.Second))
			defer cancel()
			for _, client := range clients {
				client.run(ctx)
			}

			<-ctx.Done()
			srv.historyLock.RLock()
			defer srv.historyLock.RUnlock()
			result := resultFromHistory(cfg, srv.history)
			results = append(results, result)
		})
	}

	columnizeCfg := columnize.DefaultConfig()
	columnizeCfg.Glue = " | "
	fmt.Println(columnize.Format(results, columnizeCfg))
}

func resultFromHistory(cfg *throttleTestConfig, history []*batchHistory) string {

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

	return fmt.Sprintf("%d|%d|%v|%v|%v|%d|%.0f ± %.0f (max %.0f)|%.0f ± %.0f (max %.0f)|%.0f ± %.0f (max %.0f)",
		cfg.numClients,
		cfg.allocsPerClient,
		cfg.serverBatchUpdateInterval,
		cfg.clientBatchUpdateInterval,
		cfg.clientAllocEventsBaseInterval,
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
	)

}
