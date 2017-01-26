package hh

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/influxdb/models"
	"github.com/zhexuany/influxdb-cluster/meta"
	"sync/atomic"
)

// The statistics generated by the "write" mdoule
const (
	statWriteShardReq             = "writeShardReq"
	statWriteShardReqPoints       = "writeShardPointsReq"
	statWriteNodeReqFail          = "writeNodeReqFail"
	statWriteNodeReq              = "writeNodeReq"
	statWriteNodeReqPoints        = "writeNodeReqPoints"
	statWriteConcurrencyReq       = "writeConcurrencyReq"
	statWriteConcurrencyReqFail   = "writeConcurrencyReqFail"
	statWriteConcurrencyReqPoints = "writeConcurrencyReqPoints"
)

// NodeProcessor encapsulates a queue of hinted-handoff data for a node, and the
// transmission of the data to the node.
type NodeProcessor struct {
	cfg    Config
	nodeID uint64
	dir    string

	mu   sync.RWMutex
	wg   sync.WaitGroup
	done chan struct{}

	queue  *queue
	meta   metaClient
	writer shardWriter

	stats       *Statistics
	defaultTags models.StatisticTags
	Logger      *log.Logger
}

// NewNodeProcessor returns a new NodeProcessor for the given node, using dir for
// the hinted-handoff data.
func NewNodeProcessor(nodeID uint64, dir string, w shardWriter, m metaClient, cfg Config) *NodeProcessor {
	n := &NodeProcessor{
		cfg:    cfg,
		nodeID: nodeID,
		dir:    dir,
		writer: w,
		meta:   m,

		stats: &Statistics{},
		defaultTags: models.StatisticTags{
			"hh_processor": dir,
			"id":           fmt.Sprintf("%d", nodeID),
			"path":         dir,
		},

		Logger: log.New(os.Stderr, "[handoff] ", log.LstdFlags),
	}

	return n
}

// Open opens the NodeProcessor. It will read and write data present in dir, and
// start transmitting data to the node. A NodeProcessor must be opened before it
// can accept hinted data.
func (n *NodeProcessor) Open() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.done != nil {
		// Already open.
		return nil
	}
	n.done = make(chan struct{})

	// Create the queue directory if it doesn't already exist.
	if err := os.MkdirAll(n.dir, 0700); err != nil {
		return fmt.Errorf("mkdir all: %s", err)
	}

	// Create the queue of hinted-handoff data.
	queue, err := newQueue(n.dir, n.cfg.MaxSize)
	if err != nil {
		return err
	}

	if err := queue.Open(); err != nil {
		return err
	}

	atomic.StoreInt64(&n.stats.WriteDiskBytes, queue.TotalBytes())

	// queue.
	n.wg.Add(1)
	go n.run()
	n.queue = queue

	return nil
}

// Close closes the NodeProcessor, terminating all data tranmission to the node.
// When closed it will not accept hinted-handoff data.
func (n *NodeProcessor) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.done == nil {
		// Already closed.
		return nil
	}

	close(n.done)
	n.wg.Wait()
	n.done = nil

	return n.queue.Close()
}

// Closed will return true if node processor is currently closed
func (n *NodeProcessor) Closed() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()

	select {
	case <-n.done:
		// node processor service is closed
		return true
	default:
		// determine processor is closed or not
		// via done is nil or not
	}

	return n.done == nil
}

// Statistics keeps statistics related to the hinted handoff
type Statistics struct {
	WriteShardReq                int64
	WriteShardReqPoints          int64
	WriteNodeReqFail             int64
	WriteNodeReqPoints           int64
	WriteNodeReq                 int64
	WriteShardConcurrentlyReq    int64
	WriteShardConcurrentlyFail   int64
	WriteShardConcurrentlyPoints int64
	WriteDiskBytes               int64
	WriteDiskSegments            int64
}

// Statistics returns statistics for periodic monitoring.
func (n *NodeProcessor) Statistics(tags map[string]string) []models.Statistic {
	key := strings.Join([]string{"hh_processor", n.dir}, ":")
	return []models.Statistic{{
		Name: key,
		Tags: n.defaultTags.Merge(tags),
		Values: map[string]interface{}{
			statWriteNodeReq:              atomic.LoadInt64(&n.stats.WriteNodeReq),
			statWriteNodeReqPoints:        atomic.LoadInt64(&n.stats.WriteNodeReqPoints),
			statWriteNodeReqFail:          atomic.LoadInt64(&n.stats.WriteNodeReqFail),
			statWriteShardReqPoints:       atomic.LoadInt64(&n.stats.WriteShardReqPoints),
			statWriteShardReq:             atomic.LoadInt64(&n.stats.WriteShardReq),
			statWriteConcurrencyReq:       atomic.LoadInt64(&n.stats.WriteShardConcurrentlyReq),
			statWriteConcurrencyReqFail:   atomic.LoadInt64(&n.stats.WriteShardConcurrentlyFail),
			statWriteConcurrencyReqPoints: atomic.LoadInt64(&n.stats.WriteShardConcurrentlyPoints),
			"diskBytes":                   atomic.LoadInt64(&n.stats.WriteDiskBytes),
			"totalSegments":               atomic.LoadInt64(&n.stats.WriteDiskSegments),
		},
	}}
}

// Purge deletes all hinted-handoff data under management by a NodeProcessor.
// The NodeProcessor should be in the closed state before calling this function.
func (n *NodeProcessor) Purge() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.done != nil {
		return fmt.Errorf("node processor is open")
	}

	return os.RemoveAll(n.dir)
}

// WriteShard writes hinted-handoff data for the given shard and node. Since it may manipulate
// hinted-handoff queues, and be called concurrently, it takes a lock during queue access.
func (n *NodeProcessor) WriteShard(shardID uint64, points []models.Point) error {
	n.mu.RLock()
	defer n.mu.RUnlock()

	if n.done == nil {
		return fmt.Errorf("node processor is closed")
	}

	atomic.AddInt64(&n.stats.WriteShardReq, 1)
	atomic.AddInt64(&n.stats.WriteShardReqPoints, int64(len(points)))

	go func() {
		atomic.StoreInt64(&n.stats.WriteDiskSegments, n.queue.totalSegments())
		atomic.StoreInt64(&n.stats.WriteDiskBytes, n.queue.TotalBytes())

	}()

	b := marshalWrite(shardID, points)
	if len(b) == 0 {
		return nil
	}

	if err := n.queue.Append(b); err != nil {
		//
		if err == ErrNotOpen {
			select {
			case <-n.done:
				//do nothing, wait queue closed
			}
		}
		return err
	}

	//
	atomic.AddInt64(&n.stats.WriteShardReq, 1)

	return nil
}

// LastModified returns the time the NodeProcessor last receieved hinted-handoff data.
func (n *NodeProcessor) LastModified() (time.Time, error) {
	t, err := n.queue.LastModified()
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

//
//
// run attempts to send any existing hinted handoff data to the target node. It also purges
// any hinted handoff data older than the configured time.
func (n *NodeProcessor) run() {
	defer n.wg.Done()

	currInterval := time.Duration(n.cfg.RetryInterval)
	if currInterval > time.Duration(n.cfg.RetryMaxInterval) {
		currInterval = time.Duration(n.cfg.RetryMaxInterval)
	}

	for {
		purgeTicker := time.NewTicker(time.Duration(n.cfg.PurgeInterval))
		defer purgeTicker.Stop()
		retryTicker := time.NewTicker(currInterval)
		defer retryTicker.Stop()

		limiter := NewRateLimiter(n.cfg.RetryRateLimit)

		select {
		case <-n.done:
			return
		case <-purgeTicker.C:
			if err := n.queue.PurgeOlderThan(time.Now().Add(-time.Duration(n.cfg.MaxAge))); err != nil {
				n.Logger.Printf("failed to purge for node %d: %s", n.nodeID, err.Error())
			}
		case <-retryTicker.C:
			for {
				c, err := n.SendWrite()
				if err != nil {
					if err == io.EOF {
						// No more data, return to configured interval.
						currInterval = time.Duration(n.cfg.RetryInterval)
					} else if err == meta.ErrNodeNotFound {
						// Node is crashed for some reason, just return.
						return
					} else {
						if currInterval > time.Duration(n.cfg.RetryMaxInterval) {
							currInterval = time.Duration(n.cfg.RetryMaxInterval)
						}
						n.Logger.Printf("error on sending write:%v", err)
					}
					break
				}

				// Success! Ensure backoff is cancelled.
				currInterval = time.Duration(n.cfg.RetryInterval)

				// Update how many bytes we've sent
				limiter.Update(c)

				// Block to maintain the throughput rate
				time.Sleep(limiter.Delay())
			}
		}
	}
}

// SendWrite attempts to sent the current block of hinted data to the target node. If successful,
// it returns the number of bytes it sent and advances to the next block. Otherwise returns EOF
// when there is no more data or the node is inactive.
func (n *NodeProcessor) SendWrite() (int, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	active, err := n.Active()
	if err != nil {
		return 0, err
	}
	if !active {
		return 0, io.EOF
	}

	buf, err := n.queue.Current()
	if err != nil {
		return 0, err
	}

	ch := make(chan error)

	go func(buf []byte) {
		// unmarshal the byte slice back to shard ID and points
		shardID, points, err := unmarshalWrite(buf)
		if err != nil {
			atomic.AddInt64(&n.stats.WriteNodeReqFail, 1)
			n.Logger.Printf("unmarshal write failed: %v", err)

			// send err via channels
			ch <- err
			return
		}

		if err := n.writer.WriteShard(shardID, n.nodeID, points); err != nil {
			ch <- err
			return
		}
		atomic.AddInt64(&n.stats.WriteShardReq, 1)
		atomic.AddInt64(&n.stats.WriteNodeReq, 1)
		atomic.AddInt64(&n.stats.WriteNodeReqPoints, int64(len(points)))
		ch <- nil
	}(buf)

	// Process err message from err channel.
	r := <-ch
	if r != nil {
		if r == io.EOF || r == meta.ErrNodeNotFound {
			return 0, r
		}
		n.Logger.Printf("SendWrite error: %v", r)
	}

	// Advance pos in segment if r is nil
	if err := n.queue.Advance(); err != nil {
		n.Logger.Printf("failed to advance queue for node %d: %s", n.nodeID, err.Error())
	}

	// return how much length already wroten into node
	return len(buf), nil
}

// Head returns the head of the processor's queue.
func (n *NodeProcessor) Head() string {
	qp, err := n.queue.Position()
	if err != nil {
		return ""
	}
	return qp.head
}

// Tail returns the tail of the processor's queue.
func (n *NodeProcessor) Tail() string {
	qp, err := n.queue.Position()
	if err != nil {
		return ""
	}
	return qp.tail
}

// Active returns whether this node processor is for a currently active node.
func (n *NodeProcessor) Active() (bool, error) {
	nio, err := n.meta.DataNode(n.nodeID)
	if err != nil {
		return false, err
	}
	return nio != nil, nil
}

func (n *NodeProcessor) Empty() bool {
	if n.Closed() {
		return false
	}
	return n.queue.Empty()
}

func marshalWrite(shardID uint64, points []models.Point) []byte {
	b := make([]byte, 8)
	totalPB := make([]byte, 0)
	binary.BigEndian.PutUint64(b, shardID)
	for _, p := range points {
		// If on fail, skip this point and continue
		pB, err := p.MarshalBinary()
		if err != nil {
			continue
		}

		totalPB = append(totalPB, pB...)
		totalPB = append(totalPB, '\n')
	}
	return append(b, totalPB...)
}

func unmarshalWrite(b []byte) (uint64, []models.Point, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("too short: len = %d", len(b))
	}
	shardID := binary.BigEndian.Uint64(b[:8])
	points, err := models.ParsePoints(b[8:])
	return shardID, points, err
}
