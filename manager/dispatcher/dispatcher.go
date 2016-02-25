package dispatcher

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/Sirupsen/logrus"
	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/pkg/heartbeat"
	"github.com/docker/swarm-v2/state"
	"golang.org/x/net/context"
)

var defaultTTL = 5 * time.Second

type registeredNode struct {
	Heartbeat *heartbeat.Heartbeat
	Tasks     []string
	Node      *api.Node
}

var (
	// ErrNodeAlreadyRegistered returned if node with same ID was already
	// registered with this dispatcher.
	ErrNodeAlreadyRegistered = errors.New("node already registered")
	// ErrNodeNotRegistered returned if node with such ID wasn't registered
	// with this dispatcher.
	ErrNodeNotRegistered = errors.New("node not registered")
)

// Dispatcher is responsible for dispatching tasks and tracking agent health.
type Dispatcher struct {
	mu    sync.Mutex
	nodes map[string]*registeredNode
	store state.Store
}

// New returns Dispatcher with store.
func New(store state.Store) *Dispatcher {
	return &Dispatcher{
		nodes: make(map[string]*registeredNode),
		store: store,
	}
}

// RegisterNode is used for registration of node with particular dispatcher.
func (d *Dispatcher) RegisterNode(ctx context.Context, r *api.RegisterNodeRequest) (*api.RegisterNodeResponse, error) {
	d.mu.Lock()
	_, ok := d.nodes[r.Node.Id]
	d.mu.Unlock()
	if ok {
		return nil, grpc.Errorf(codes.AlreadyExists, ErrNodeAlreadyRegistered.Error())
	}
	n := r.Node
	n.Status = api.NodeStatus_READY
	// create or update node in raft
	err := d.store.CreateNode(n.Id, n)
	if err != nil {
		if err != state.ErrExist {
			return nil, err
		}
		if err := d.store.UpdateNode(n.Id, n); err != nil {
			return nil, err
		}
	}
	ttl := d.electTTL()
	d.mu.Lock()
	d.nodes[n.Id] = &registeredNode{
		Heartbeat: heartbeat.New(ttl, func() {
			if err := d.nodeDown(n.Id); err != nil {
				logrus.Errorf("error deregistering node %s after heartbeat was not received: %v", n.Id, err)
			}
		}),
		Node: n,
	}
	d.mu.Unlock()
	return &api.RegisterNodeResponse{HeartbeatTTL: uint64(ttl)}, nil
}

// UpdateNodeStatus updates status of particular node. Nodes can use it
// for notifying about graceful shutdowns for example.
func (d *Dispatcher) UpdateNodeStatus(context.Context, *api.UpdateNodeStatusRequest) (*api.UpdateNodeStatusResponse, error) {
	return nil, nil
}

// UpdateTaskStatus updates status of task. Node should send such updates
// on every status change of its tasks.
func (d *Dispatcher) UpdateTaskStatus(context.Context, *api.UpdateTaskStatusRequest) (*api.UpdateTaskStatusResponse, error) {
	return nil, nil
}

// WatchTasks is a stream of tasks for node. It returns full list of tasks
// which should be runned on node each time.
func (d *Dispatcher) WatchTasks(*api.WatchTasksRequest, api.Agent_WatchTasksServer) error {
	return nil
}

func (d *Dispatcher) nodeDown(id string) error {
	d.mu.Lock()
	delete(d.nodes, id)
	d.mu.Unlock()
	if err := d.store.UpdateNode(id, &api.Node{Id: id, Status: api.NodeStatus_DOWN}); err != nil {
		return fmt.Errorf("failed to update node %s status to down", id)
	}
	return nil
}

func (d *Dispatcher) electTTL() time.Duration {
	return defaultTTL
}

// Heartbeat is heartbeat method for nodes. It returns new TTL in response.
// Node should send new heartbeat earlier than now + TTL, otherwise it will
// be deregistered from dispatcher and its status will be updated to NodeStatus_DOWN
func (d *Dispatcher) Heartbeat(ctx context.Context, r *api.HeartbeatRequest) (*api.HeartbeatResponse, error) {
	d.mu.Lock()
	node, ok := d.nodes[r.NodeID]
	d.mu.Unlock()
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}
	ttl := d.electTTL()
	node.Heartbeat.Update(ttl)
	node.Heartbeat.Beat()
	return &api.HeartbeatResponse{HeartbeatTTL: uint64(ttl)}, nil
}