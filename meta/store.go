package meta

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/hashicorp/raft"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/zhexuany/influxcloud"
	"github.com/zhexuany/influxcloud/meta/internal"
	"golang.org/x/crypto/bcrypt"
)

// Retention policy settings.
const (
	autoCreateRetentionPolicyName   = "default"
	autoCreateRetentionPolicyPeriod = 0

	// maxAutoCreatedRetentionPolicyReplicaN is the maximum replication factor that will
	// be set for auto-created retention policies.
	maxAutoCreatedRetentionPolicyReplicaN = 3
)

// Raft configuration.
const (
	raftListenerStartupTimeout = time.Second
)

type store struct {
	mu      sync.RWMutex
	closing chan struct{}

	config      *Config
	data        *Data
	raftState   *raftState
	dataChanged chan struct{}
	path        string
	opened      bool
	logger      *log.Logger

	raftAddr string
	httpAddr string

	node *influxcloud.Node

	raftLn net.Listener
}

// newStore will create a new metastore with the passed in config
func newStore(c *Config, httpAddr, raftAddr string) *store {
	s := store{
		data: &Data{
			Data: &meta.Data{
				Index: 1,
			},
		},
		closing:     make(chan struct{}),
		dataChanged: make(chan struct{}),
		path:        c.Dir,
		config:      c,
		httpAddr:    httpAddr,
		raftAddr:    raftAddr,
	}
	if c.LoggingEnabled {
		s.logger = log.New(os.Stderr, "[metastore] ", log.LstdFlags)
	} else {
		s.logger = log.New(ioutil.Discard, "", 0)
	}

	func() ([]byte, error) {
		return bcrypt.GenerateFromPassword(nil, 1)
	}()

	return &s
}

// open opens and initializes the raft store.
//TODO refactor is needed. Currentyly, we use client to join node. This is not a good choice.
func (s *store) open(raftln net.Listener) error {
	s.logger.Printf("Using data dir: %v", s.path)

	joinPeers, err := s.filterAddr(s.config.JoinPeers, s.httpAddr)
	if err != nil {
		return err
	}

	joinPeers = s.config.JoinPeers
	var initializePeers []string
	if len(joinPeers) > 0 {
		c := NewClient(s.config)
		c.SetMetaServers(joinPeers)
		c.SetTLS(s.config.HTTPSEnabled)
		for {
			peers := c.peers()
			if !Peers(peers).Contains(s.raftAddr) {
				peers = append(peers, s.raftAddr)
			}
			if len(s.config.JoinPeers)-len(peers) == 0 {
				initializePeers = peers
				break
			}

			if len(peers) > len(s.config.JoinPeers) {
				s.logger.Printf("waiting for join peers to match config specified. found %v, config specified %v", peers, s.config.JoinPeers)
			} else {
				s.logger.Printf("Waiting for %d join peers.  Have %v. Asking nodes: %v", len(s.config.JoinPeers)-len(peers), peers, joinPeers)
			}
			time.Sleep(time.Second)
		}
	}

	if err := s.setOpen(); err != nil {
		return err
	}

	// Create the root directory if it doesn't already exist.
	if err := os.MkdirAll(s.path, 0777); err != nil {
		return fmt.Errorf("mkdir all: %s", err)
	}

	// Start to open the raft store.
	if err := s.openRaft(initializePeers, raftln); err != nil {
		return fmt.Errorf("raft: %s", err)
	}

	if len(joinPeers) > 0 {
		c := NewClient(s.config)
		c.SetMetaServers(joinPeers)
		c.SetTLS(s.config.HTTPSEnabled)
		if err := c.Open(); err != nil {
			return err
		}
		defer c.Close()

		n, err := c.JoinMetaServer(s.httpAddr, s.raftAddr)
		if err != nil {
			return err
		}

		s.node.ID = n.ID
		if err := s.node.Save(); err != nil {
			return err
		}

	}

	// Wait for a leader to be elected so we know the raft log is loaded
	// and up to date
	if err := s.waitForLeader(0); err != nil {
		return fmt.Errorf("raft: %s", err)
	}

	// Make sure this server is in the list of metanodes
	peers, err := s.raftState.peers()
	if err != nil {
		return err
	}

	if len(peers) <= 1 {
		// we have to loop here because if the hostname has changed
		// raft will take a little bit to normalize so that this host
		// will be marked as the leader
		for {
			err := s.setMetaNode(s.httpAddr, s.raftAddr)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	return nil
}

// setMetaNode is used when the raft group has only a single peer. It will
// either create a metanode or update the information for the one metanode
// that is there. It's used because hostnames can change
func (s *store) setMetaNode(addr, raftAddr string) error {
	val := &internal.SetMetaNodeCommand{
		HTTPAddr: proto.String(addr),
		TCPAddr:  proto.String(raftAddr),
		Rand:     proto.Uint64(uint64(rand.Int63())),
	}
	t := internal.Command_SetMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_SetMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

func (s *store) setOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Check if store has already been opened.
	if s.opened {
		return ErrStoreOpen
	}
	s.opened = true
	return nil
}

// peers returns the raft peers known to this store
func (s *store) peers() []string {
	if s.raftOpened() {
		return []string{s.raftAddr}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	peers, err := s.raftState.peers()
	if err != nil {
		return []string{s.raftAddr}
	}
	return peers
}

func (s *store) filterAddr(addrs []string, filter string) ([]string, error) {
	host, port, err := net.SplitHostPort(filter)
	if err != nil {
		return nil, err
	}

	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, err
	}

	var joinPeers []string
	for _, addr := range addrs {
		joinHost, joinPort, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		joinIP, err := net.ResolveIPAddr("ip", joinHost)
		if err != nil {
			return nil, err
		}

		// Don't allow joining ourselves
		if ip.String() == joinIP.String() && port == joinPort {
			continue
		}
		joinPeers = append(joinPeers, addr)
	}
	return joinPeers, nil
}

func (s *store) openRaft(initializePeers []string, raftln net.Listener) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rs := newRaftState(s.config, s.raftAddr)
	rs.logger = s.logger
	rs.path = s.path

	if err := rs.open(s, raftln, initializePeers); err != nil {
		return err
	}
	s.raftState = rs

	return nil
}

// raftOpened will return true is raftState is not nil
// otherise will return false.
func (s *store) raftOpened() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.raftState == nil
}

// ready will just return boolean ready back
func (s *store) ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return true
}

// reset will reset old raft store and set newly passed store as new raft stroe
func (s *store) reset(st *store) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.raftState.close(); err != nil {
		return err
	}

	// we have to remove all file in raft path
	// directory, since we want to a reset operation.
	//
	err := os.RemoveAll(filepath.Join(s.raftState.path, "/*"))
	if err != nil {
		os.Remove(filepath.Join(s.path, "raft.db"))
	}

	st.path = s.path
	st.raftState = s.raftState
	// reopen the strore after remove all files
	if err := st.open(st.raftLn); err != nil {
		return err
	}
	if s.raftState == nil {
		// return nil
	}

	return nil
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.closing:
		// already closed
		return nil
	default:
		//closing
		close(s.closing)
		return s.raftState.close()
	}
}

func (s *store) snapshot() (*Data, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Clone(), nil
}

func (s *store) setSnapshot(data *Data) error {
	dataB, err := data.MarshalBinary()
	if err != nil {
		return err
	}

	// Prepare proto command for snapshot
	val := &internal.SetDataCommand{Data: dataB}

	t := internal.Command_SetDataCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_SetDataCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return nil
	}

	return s.apply(b)
}

// afterIndex returns a channel that will be closed to signal
// the caller when an updated snapshot is available.
func (s *store) afterIndex(index uint64) <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if index < s.data.Data.Index {
		// Client needs update so return a closed channel.
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	return s.dataChanged
}

// applied return nil if all preceeding operations have been applied to
// the FSM. An optional timeout can be provided to limit the amount of
// time we waite for the command to be started. This must be run on the
// leader or it will fail.
// applied return error if some preceeding operations have not been
// applied to the FSM within the timeout provided provided at run time.
// The Error() is defined in Future and it will blocks until the future
// arrives and then return the error satus of the future
// Note that for same operation, this method can only call once.
func (s *store) applied(timeout time.Duration) error {
	return s.raftState.raft.Barrier(timeout).Error()
}

// WaitForLeader sleeps until a leader is found or a timeout occurs.
// timeout == 0 means to wait forever.
func (s *store) waitForLeader(timeout time.Duration) error {
	// Begin timeout timer.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Continually check for leader until timeout.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.closing:
			return errors.New("closing")
		case <-timer.C:
			if timeout != 0 {
				return errors.New("timeout")
			}
		case <-ticker.C:
			if s.leader() != "" {
				return nil
			}
		}
	}
}

// isLeader returns true if the store is currently the leader.
func (s *store) isLeader() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return false
	}
	return s.raftState.raft.State() == raft.Leader
}

// leader returns what the store thinks is the current leader. An empty
// string indicates no leader exists.
func (s *store) leader() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil || s.raftState.raft == nil {
		return ""
	}
	return s.raftState.raft.Leader()
}

// leaderHTTP returns the HTTP API connection info for the metanode
// that is the raft leader
func (s *store) leaderHTTP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return ""
	}
	l := s.raftState.raft.Leader()

	for _, n := range s.data.MetaNodes {
		if n.TCPHost == l {
			return n.Host
		}
	}

	return ""
}

// otherMetaServersHTTP will return the HTTP bind addresses of the other
// meta servers in the cluster
func (s *store) otherMetaServersHTTP() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var a []string
	for _, n := range s.data.MetaNodes {
		if n.TCPHost != s.raftAddr {
			a = append(a, n.Host)
		}
	}
	return a

}

// dataNode will return a data node info according to its id
func (s *store) dataNode(id uint64) *NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, n := range s.data.DataNodes {
		if n.ID == id {
			return &NodeInfo{
				ID:      n.ID,
				TCPHost: n.TCPHost,
				Host:    n.Host,
			}
		}
	}
	return nil
}

// dataNodeByTCPHost will return a data node according tcpHost
func (s *store) dataNodeByTCPHost(tcpHost string) *NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, n := range s.data.DataNodes {
		if tcpHost != "" && n.TCPHost == tcpHost {
			// if tcpHost is empty, then data node
			// must be nil. We need consider this.
			return &n
		}
	}

	return nil
}

// index returns the current store index.
func (s *store) index() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Data.Index
}

// apply applies a command to raft.
func (s *store) apply(b []byte) error {
	if s.raftState == nil {
		return fmt.Errorf("store not open")
	}

	return s.raftState.apply(b)
}

// join adds a new server to the metaservice and raft
func (s *store) join(n *NodeInfo) (*NodeInfo, error) {
	s.mu.RLock()
	if s.raftState == nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("store not open")
	}

	if err := s.raftState.addPeer(n.TCPHost); err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	s.mu.RUnlock()

	if err := s.createMetaNode(n.Host, n.TCPHost); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, node := range s.data.MetaNodes {
		if node.TCPHost == n.TCPHost && node.Host == n.Host {
			return &node, nil
		}
	}
	return nil, ErrNodeNotFound
}

func (s *store) leave(n *NodeInfo) error {
	return s.removePeer(n.TCPHost)
}

// removePeer will remove a peer node according to peer's addr
func (s *store) removePeer(peer string) error {
	return s.raftState.removePeer(peer)
}

// createMetaNode is used by the join command to create the metanode int
// the metastore
func (s *store) createMetaNode(addr, raftAddr string) error {
	val := &internal.CreateMetaNodeCommand{
		HTTPAddr: proto.String(addr),
		TCPAddr:  proto.String(raftAddr),
		Rand:     proto.Uint64(uint64(rand.Int63())),
	}
	t := internal.Command_CreateMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_CreateMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

func (s *store) deleteMetaNode(id uint64) error {
	val := &internal.DeleteMetaNodeCommand{
		ID: proto.Uint64(id),
	}
	t := internal.Command_DeleteMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_DeleteMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return nil
	}

	return s.apply(b)
}

//
func (s *store) createDataNode(addr, raftAddr string) error {
	val := &internal.CreateDataNodeCommand{
		HTTPAddr: proto.String(addr),
		TCPAddr:  proto.String(raftAddr),
	}
	t := internal.Command_CreateDataNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_CreateDataNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

// deleteDataNode will delete a data node according to its id
func (s *store) deleteDataNode(id uint64) error {
	val := &internal.DeleteDataNodeCommand{
		ID: proto.Uint64(id),
	}
	t := internal.Command_DeleteDataNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_DeleteDataNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

func (s *store) updateDataNode(id uint64, host, tcpHost string) error {
	val := &internal.UpdateDataNodeCommand{
		ID:      proto.Uint64(id),
		Host:    proto.String(host),
		TCPHost: proto.String(tcpHost),
	}

	t := internal.Command_UpdateDataNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_UpdateDataNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	if err := s.apply(b); err != nil {
		return err
	}

	n := s.dataNodeByTCPHost(tcpHost)
	if n == nil {
		return ErrNodeNotFound
	}

	return nil
}

//
func (s *store) nodeByHTTPAddr(addr string) (*NodeInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ni := range s.data.MetaNodes {
		if ni.Host == addr {
			return &ni, nil
		}
	}

	return nil, ErrNodeNotFound
}
