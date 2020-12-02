/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package scheduler

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/looplab/fsm"
	"go.uber.org/zap"

	"github.com/apache/incubator-yunikorn-core/pkg/common"
	"github.com/apache/incubator-yunikorn-core/pkg/common/configs"
	"github.com/apache/incubator-yunikorn-core/pkg/common/resources"
	"github.com/apache/incubator-yunikorn-core/pkg/common/security"
	"github.com/apache/incubator-yunikorn-core/pkg/interfaces"
	"github.com/apache/incubator-yunikorn-core/pkg/log"
	"github.com/apache/incubator-yunikorn-core/pkg/metrics"
	"github.com/apache/incubator-yunikorn-core/pkg/scheduler/objects"
	"github.com/apache/incubator-yunikorn-core/pkg/scheduler/placement"
	"github.com/apache/incubator-yunikorn-core/pkg/scheduler/policies"
	"github.com/apache/incubator-yunikorn-core/pkg/webservice/dao"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

type PartitionContext struct {
	RmID string // the RM the partition belongs to
	Name string // name of the partition (logging mainly)

	// Private fields need protection
	root                   *objects.Queue                  // start of the queue hierarchy
	applications           map[string]*objects.Application // applications assigned to this partition
	reservedApps           map[string]int                  // applications reserved within this partition, with reservation count
	nodes                  map[string]*objects.Node        // nodes assigned to this partition
	allocations            map[string]*objects.Allocation  // allocations
	placementManager       *placement.AppPlacementManager  // placement manager for this partition
	partitionManager       *partitionManager               // manager for this partition
	stateMachine           *fsm.FSM                        // the state of the partition for scheduling
	stateTime              time.Time                       // last time the state was updated (needed for cleanup)
	isPreemptable          bool                            // can allocations be preempted
	rules                  *[]configs.PlacementRule        // placement rules to be loaded by the scheduler
	userGroupCache         *security.UserGroupCache        // user cache per partition
	totalPartitionResource *resources.Resource             // Total node resources
	nodeSortingPolicy      *policies.NodeSortingPolicy     // Global Node Sorting Policies

	sync.RWMutex
}

func newPartitionContext(conf configs.PartitionConfig, rmID string, cc *ClusterContext) (*PartitionContext, error) {
	if conf.Name == "" || rmID == "" {
		log.Logger().Info("partition cannot be created",
			zap.String("partition name", conf.Name),
			zap.String("rmID", rmID),
			zap.Any("cluster context", cc))
		return nil, fmt.Errorf("partition cannot be created without name or RM, one is not set")
	}
	pc := &PartitionContext{
		Name:         conf.Name,
		RmID:         rmID,
		stateMachine: objects.NewObjectState(),
		stateTime:    time.Now(),
		applications: make(map[string]*objects.Application),
		reservedApps: make(map[string]int),
		nodes:        make(map[string]*objects.Node),
		allocations:  make(map[string]*objects.Allocation),
	}
	pc.partitionManager = &partitionManager{
		pc: pc,
		cc: cc,
	}
	if err := pc.initialPartitionFromConfig(conf); err != nil {
		return nil, err
	}
	return pc, nil
}

// Initialise the partition
func (pc *PartitionContext) initialPartitionFromConfig(conf configs.PartitionConfig) error {
	if len(conf.Queues) == 0 || conf.Queues[0].Name != configs.RootQueue {
		return fmt.Errorf("partition cannot be created without root queue")
	}

	// Setup the queue structure: root first it should be the only queue at this level
	// Add the rest of the queue structure recursively
	queueConf := conf.Queues[0]
	var err error
	if pc.root, err = objects.NewConfiguredQueue(queueConf, nil); err != nil {
		return err
	}
	// recursively add the queues to the root
	if err = pc.addQueue(queueConf.Queues, pc.root); err != nil {
		return err
	}
	log.Logger().Info("root queue added",
		zap.String("partitionName", pc.Name),
		zap.String("rmID", pc.RmID))

	// set preemption needed flag
	pc.isPreemptable = conf.Preemption.Enabled

	pc.rules = &conf.PlacementRules
	// We need to pass in the unlocked version of the getQueue function.
	// Placing an application will already have a lock on the partition context.
	pc.placementManager = placement.NewPlacementManager(*pc.rules, pc.getQueue)
	// get the user group cache for the partition
	// TODO get the resolver from the config
	pc.userGroupCache = security.GetUserGroupCache("")

	// TODO Need some more cleaner interface here.
	var configuredPolicy policies.SortingPolicy
	configuredPolicy, err = policies.FromString(conf.NodeSortPolicy.Type)
	if err != nil {
		log.Logger().Debug("NodeSorting policy incorrectly set or unknown",
			zap.Error(err))
	}
	switch configuredPolicy {
	case policies.BinPackingPolicy, policies.FairnessPolicy:
		log.Logger().Info("NodeSorting policy set from config",
			zap.String("policyName", configuredPolicy.String()))
		pc.nodeSortingPolicy = policies.NewNodeSortingPolicy(conf.NodeSortPolicy.Type)
	case policies.Unknown:
		log.Logger().Info("NodeSorting policy not set using 'fair' as default")
		pc.nodeSortingPolicy = policies.NewNodeSortingPolicy("fair")
	}
	return nil
}

func (pc *PartitionContext) updatePartitionDetails(conf configs.PartitionConfig) error {
	pc.Lock()
	defer pc.Unlock()
	if len(conf.Queues) == 0 || conf.Queues[0].Name != configs.RootQueue {
		return fmt.Errorf("partition cannot be created without root queue")
	}

	if pc.placementManager.IsInitialised() {
		log.Logger().Info("Updating placement manager rules on config reload")
		err := pc.placementManager.UpdateRules(conf.PlacementRules)
		if err != nil {
			log.Logger().Info("New placement rules not activated, config reload failed", zap.Error(err))
			return err
		}
		pc.rules = &conf.PlacementRules
	} else {
		log.Logger().Info("Creating new placement manager on config reload")
		pc.rules = &conf.PlacementRules
		// We need to pass in the unlocked version of the getQueue function.
		// Placing an application will already have a lock on the partition context.
		pc.placementManager = placement.NewPlacementManager(*pc.rules, pc.getQueue)
	}
	// start at the root: there is only one queue
	queueConf := conf.Queues[0]
	root := pc.root
	// update the root queue
	if err := root.SetQueueConfig(queueConf); err != nil {
		return err
	}
	root.UpdateSortType()
	// update the rest of the queues recursively
	return pc.updateQueues(queueConf.Queues, root)
}

// Process the config structure and create a queue info tree for this partition
func (pc *PartitionContext) addQueue(conf []configs.QueueConfig, parent *objects.Queue) error {
	// create the queue at this level
	for _, queueConf := range conf {
		thisQueue, err := objects.NewConfiguredQueue(queueConf, parent)
		if err != nil {
			return err
		}
		// recursive create the queues below
		if len(queueConf.Queues) > 0 {
			err = pc.addQueue(queueConf.Queues, thisQueue)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Update the passed in queues and then do this recursively for the children
//
// NOTE: this is a lock free call. It should only be called holding the PartitionContext lock.
func (pc *PartitionContext) updateQueues(config []configs.QueueConfig, parent *objects.Queue) error {
	// get the name of the passed in queue
	parentPath := parent.QueuePath + configs.DOT
	// keep track of which children we have updated
	visited := map[string]bool{}
	// walk over the queues recursively
	for _, queueConfig := range config {
		pathName := parentPath + queueConfig.Name
		queue := pc.getQueue(pathName)
		var err error
		if queue == nil {
			queue, err = objects.NewConfiguredQueue(queueConfig, parent)
		} else {
			err = queue.SetQueueConfig(queueConfig)
		}
		if err != nil {
			return err
		}
		// special call to convert to a real policy from the property
		queue.UpdateSortType()
		if err = pc.updateQueues(queueConfig.Queues, queue); err != nil {
			return err
		}
		visited[queue.Name] = true
	}
	// remove all children that were not visited
	for childName, childQueue := range parent.GetCopyOfChildren() {
		if !visited[childName] {
			childQueue.MarkQueueForRemoval()
		}
	}
	return nil
}

// Mark the partition  for removal from the system.
// This can be executed multiple times and is only effective the first time.
// The current cleanup sequence is "immediate". This is implemented to allow a graceful cleanup.
func (pc *PartitionContext) markPartitionForRemoval() {
	if err := pc.handlePartitionEvent(objects.Remove); err != nil {
		log.Logger().Error("failed to mark partition for deletion",
			zap.String("partitionName", pc.Name),
			zap.Error(err))
	}
}

// Get the state of the partition.
// No new nodes and applications will be accepted if stopped or being removed.
func (pc *PartitionContext) isDraining() bool {
	return pc.stateMachine.Current() == objects.Draining.String()
}

func (pc *PartitionContext) isRunning() bool {
	return pc.stateMachine.Current() == objects.Active.String()
}

func (pc *PartitionContext) isStopped() bool {
	return pc.stateMachine.Current() == objects.Stopped.String()
}

// Handle the state event for the partition.
// The state machine handles the locking.
func (pc *PartitionContext) handlePartitionEvent(event objects.ObjectEvent) error {
	err := pc.stateMachine.Event(event.String(), pc.Name)
	if err == nil {
		pc.stateTime = time.Now()
		return nil
	}
	// handle the same state transition not nil error (limit of fsm).
	if err.Error() == "no transition" {
		return nil
	}
	return err
}

// Add a new application to the partition.
func (pc *PartitionContext) AddApplication(app *objects.Application) error {
	pc.Lock()
	defer pc.Unlock()

	if pc.isDraining() || pc.isStopped() {
		return fmt.Errorf("partition %s is stopped cannot add a new application %s", pc.Name, app.ApplicationID)
	}

	// Add to applications
	appID := app.ApplicationID
	if pc.applications[appID] != nil {
		return fmt.Errorf("adding application %s to partition %s, but application already existed", appID, pc.Name)
	}

	// Put app under the queue
	queueName := app.QueueName
	if pc.placementManager.IsInitialised() {
		err := pc.placementManager.PlaceApplication(app)
		if err != nil {
			return fmt.Errorf("failed to place application %s: %v", appID, err)
		}
		queueName = app.QueueName
		if queueName == "" {
			return fmt.Errorf("application rejected by placement rules: %s", appID)
		}
	}
	// we have a queue name either from placement or direct, get the queue
	queue := pc.getQueue(queueName)
	if queue == nil {
		// queue must exist if not using placement rules
		if !pc.placementManager.IsInitialised() {
			return fmt.Errorf("application '%s' rejected, cannot create queue '%s' without placement rules", appID, queueName)
		}
		// with placement rules the hierarchy might not exist so try and create it
		var err error
		queue, err = pc.createQueue(queueName, app.GetUser())
		if err != nil {
			return fmt.Errorf("failed to create rule based queue %s for application %s", queueName, appID)
		}
	}
	// check the queue: is a leaf queue with submit access
	if !queue.IsLeafQueue() || !queue.CheckSubmitAccess(app.GetUser()) {
		return fmt.Errorf("failed to find queue %s for application %s", queueName, appID)
	}

	// all is OK update the app and partition
	app.SetQueue(queue)
	queue.AddApplication(app)
	pc.applications[appID] = app

	return nil
}

// Remove the application from the partition.
// This does not fail and handles missing /app/queue/node/allocations internally
func (pc *PartitionContext) removeApplication(appID string) []*objects.Allocation {
	pc.Lock()
	defer pc.Unlock()

	// Remove from applications map
	if pc.applications[appID] == nil {
		return nil
	}
	app := pc.applications[appID]
	// remove from partition then cleanup underlying objects
	delete(pc.applications, appID)
	delete(pc.reservedApps, appID)

	queueName := app.QueueName
	// Remove all asks and thus all reservations and pending resources (queue included)
	_ = app.RemoveAllocationAsk("")
	// Remove app from queue
	if queue := pc.getQueue(queueName); queue != nil {
		queue.RemoveApplication(app)
	}
	// Remove all allocations
	allocations := app.RemoveAllAllocations()
	// Remove all allocations from nodes and the partition (queues have been updated already)
	if len(allocations) != 0 {
		for _, alloc := range allocations {
			currentUUID := alloc.UUID
			// Remove from partition
			if globalAlloc := pc.allocations[currentUUID]; globalAlloc == nil {
				log.Logger().Warn("unknown allocation: not found on the partition",
					zap.String("appID", appID),
					zap.String("allocationId", currentUUID))
			} else {
				delete(pc.allocations, currentUUID)
			}

			// Remove from node: even if not found on the partition to keep things clean
			node := pc.nodes[alloc.NodeID]
			if node == nil {
				log.Logger().Warn("unknown node: not found in active node list",
					zap.String("appID", appID),
					zap.String("nodeID", alloc.NodeID))
				continue
			}
			if nodeAlloc := node.RemoveAllocation(currentUUID); nodeAlloc == nil {
				log.Logger().Warn("unknown allocation: not found on the node",
					zap.String("appID", appID),
					zap.String("allocationId", currentUUID),
					zap.String("nodeID", alloc.NodeID))
			}
		}
	}

	log.Logger().Debug("application removed from the scheduler",
		zap.String("queue", queueName),
		zap.String("applicationID", appID))

	return allocations
}

func (pc *PartitionContext) getApplication(appID string) *objects.Application {
	pc.RLock()
	defer pc.RUnlock()

	return pc.applications[appID]
}

// Return a copy of the map of all reservations for the partition.
// This will return an empty map if there are no reservations.
// Visible for tests
func (pc *PartitionContext) getReservations() map[string]int {
	pc.RLock()
	defer pc.RUnlock()
	reserve := make(map[string]int)
	for key, num := range pc.reservedApps {
		reserve[key] = num
	}
	return reserve
}

// Get the queue from the structure based on the fully qualified name.
// Wrapper around the unlocked version getQueue()
// Visible by tests
func (pc *PartitionContext) GetQueue(name string) *objects.Queue {
	pc.RLock()
	defer pc.RUnlock()
	return pc.getQueue(name)
}

// Get the queue from the structure based on the fully qualified name.
// The name is not syntax checked and must be valid.
// Returns nil if the queue is not found otherwise the queue object.
//
// NOTE: this is a lock free call. It should only be called holding the PartitionContext lock.
func (pc *PartitionContext) getQueue(name string) *objects.Queue {
	// start at the root
	queue := pc.root
	part := strings.Split(strings.ToLower(name), configs.DOT)
	// no input
	if len(part) == 0 || part[0] != configs.RootQueue {
		return nil
	}
	// walk over the parts going down towards the requested queue
	for i := 1; i < len(part); i++ {
		// if child not found break out and return
		if queue = queue.GetChildQueue(part[i]); queue == nil {
			break
		}
	}
	return queue
}

// Get the queue info for the whole queue structure to pass to the webservice
func (pc *PartitionContext) GetQueueInfos() dao.QueueDAOInfo {
	return pc.root.GetQueueInfos()
}

// Create a queue with full hierarchy. This is called when a new queue is created from a placement rule.
// The final leaf queue does not exist otherwise we would not get here.
// This means that at least 1 queue (a leaf queue) will be created
// NOTE: this is a lock free call. It should only be called holding the PartitionContext lock.
func (pc *PartitionContext) createQueue(name string, user security.UserGroup) (*objects.Queue, error) {
	// find the queue furthest down the hierarchy that exists
	var toCreate []string
	if !strings.HasPrefix(name, configs.RootQueue) || !strings.Contains(name, configs.DOT) {
		return nil, fmt.Errorf("illegal queue name passed in: %s", name)
	}
	current := name
	queue := pc.getQueue(current)
	log.Logger().Debug("Checking queue creation")
	for queue == nil {
		toCreate = append(toCreate, current[strings.LastIndex(current, configs.DOT)+1:])
		current = current[0:strings.LastIndex(current, configs.DOT)]
		queue = pc.getQueue(current)
	}
	// Check the ACL before we really create
	// The existing parent queue is the lowest we need to look at
	if !queue.CheckSubmitAccess(user) {
		return nil, fmt.Errorf("submit access to queue %s denied during create of: %s", current, name)
	}
	if queue.IsLeafQueue() {
		return nil, fmt.Errorf("creation of queue %s failed parent is already a leaf: %s", name, current)
	}
	log.Logger().Debug("Creating queue(s)",
		zap.String("parent", current),
		zap.String("fullPath", name))
	for i := len(toCreate) - 1; i >= 0; i-- {
		// everything is checked and there should be no errors
		var err error
		queue, err = objects.NewDynamicQueue(toCreate[i], i == 0, queue)
		if err != nil {
			log.Logger().Warn("Queue auto create failed unexpected",
				zap.String("queueName", toCreate[i]),
				zap.Error(err))
			return nil, err
		}
	}
	return queue, nil
}

// Get a node from the partition by nodeID.
func (pc *PartitionContext) GetNode(nodeID string) *objects.Node {
	pc.RLock()
	defer pc.RUnlock()

	return pc.nodes[nodeID]
}

// Get a copy of the  nodes from the partition.
// This list does not include reserved nodes or nodes marked unschedulable
func (pc *PartitionContext) getSchedulableNodes() []*objects.Node {
	return pc.getNodes(true)
}

// Get a copy of the nodes from the partition.
// Excludes unschedulable nodes only, reserved node inclusion depends on the parameter passed in.
func (pc *PartitionContext) getNodes(excludeReserved bool) []*objects.Node {
	pc.RLock()
	defer pc.RUnlock()

	nodes := make([]*objects.Node, 0)
	for _, node := range pc.nodes {
		// filter out the nodes that are not scheduling
		if !node.IsSchedulable() || (excludeReserved && node.IsReserved()) {
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// Add the node to the partition and process the allocations that are reported by the node.
func (pc *PartitionContext) AddNode(node *objects.Node, existingAllocations []*objects.Allocation) error {
	if node == nil {
		return fmt.Errorf("cannot add 'nil' node to partition %s", pc.Name)
	}
	pc.Lock()
	defer pc.Unlock()

	if pc.isDraining() || pc.isStopped() {
		return fmt.Errorf("partition %s is stopped cannot add a new node %s", pc.Name, node.NodeID)
	}

	if pc.nodes[node.NodeID] != nil {
		return fmt.Errorf("partition %s has an existing node %s, node name must be unique", pc.Name, node.NodeID)
	}

	log.Logger().Debug("adding node to partition",
		zap.String("nodeID", node.NodeID),
		zap.String("partition", pc.Name))

	// update the resources available in the cluster
	if pc.totalPartitionResource == nil {
		pc.totalPartitionResource = node.GetCapacity().Clone()
	} else {
		pc.totalPartitionResource.AddTo(node.GetCapacity())
	}
	pc.root.SetMaxResource(pc.totalPartitionResource)

	// Node is added to the system to allow processing of the allocations
	pc.nodes[node.NodeID] = node
	// Add allocations that exist on the node when added
	if len(existingAllocations) > 0 {
		log.Logger().Info("add existing allocations",
			zap.String("nodeID", node.NodeID),
			zap.Int("existingAllocations", len(existingAllocations)))
		for current, alloc := range existingAllocations {
			if err := pc.addAllocation(alloc); err != nil {
				released := pc.removeNodeInternal(node.NodeID)
				log.Logger().Info("failed to add existing allocations",
					zap.String("nodeID", node.NodeID),
					zap.Int("existingAllocations", len(existingAllocations)),
					zap.Int("releasedAllocations", len(released)),
					zap.Int("processingAlloc", current))
				metrics.GetSchedulerMetrics().IncFailedNodes()
				return err
			}
		}
	}

	// Node is added update the metrics
	metrics.GetSchedulerMetrics().IncActiveNodes()
	log.Logger().Info("added node to partition",
		zap.String("nodeID", node.NodeID),
		zap.String("partition", pc.Name))

	return nil
}

// Remove a node from the partition. It returns all removed allocations.
func (pc *PartitionContext) removeNode(nodeID string) []*objects.Allocation {
	pc.Lock()
	defer pc.Unlock()
	return pc.removeNodeInternal(nodeID)
}

// Remove a node from the partition. It returns all removed allocations.
// Unlocked version must be called holding the partition lock.
func (pc *PartitionContext) removeNodeInternal(nodeID string) []*objects.Allocation {
	log.Logger().Info("remove node from partition",
		zap.String("nodeID", nodeID),
		zap.String("partition", pc.Name))

	node := pc.nodes[nodeID]
	if node == nil {
		log.Logger().Debug("node was not found",
			zap.String("nodeID", nodeID),
			zap.String("partitionName", pc.Name))
		return nil
	}

	// Remove node from list of tracked nodes
	delete(pc.nodes, nodeID)
	metrics.GetSchedulerMetrics().DecActiveNodes()

	// found the node cleanup the node and all linked data
	released := pc.removeNodeAllocations(node)
	pc.totalPartitionResource.SubFrom(node.GetCapacity())
	pc.root.SetMaxResource(pc.totalPartitionResource)

	// unreserve all the apps that were reserved on the node
	reservedKeys, releasedAsks := node.UnReserveApps()
	// update the partition reservations based on the node clean up
	for i, appID := range reservedKeys {
		pc.unReserveCount(appID, releasedAsks[i])
	}

	log.Logger().Info("node removed",
		zap.String("partitionName", pc.Name),
		zap.String("nodeID", node.NodeID))
	return released
}

// Remove all allocations that are assigned to a node as part of the node removal. This is not part of the node object
// as updating the applications and queues is the only goal. Applications and queues are not accessible from the node.
// The removed allocations are returned.
func (pc *PartitionContext) removeNodeAllocations(node *objects.Node) []*objects.Allocation {
	released := make([]*objects.Allocation, 0)
	// walk over all allocations still registered for this node
	for _, alloc := range node.GetAllAllocations() {
		allocID := alloc.UUID
		// since we are not locking the node and or application we could have had an update while processing
		// note that we do not return the allocation if the app or allocation is not found and assume that it
		// was already removed
		app := pc.applications[alloc.ApplicationID]
		if app == nil {
			log.Logger().Info("app is not found, skipping while removing the node",
				zap.String("appID", alloc.ApplicationID),
				zap.String("nodeID", node.NodeID))
			continue
		}
		// check allocations on the app
		if app.RemoveAllocation(allocID) == nil {
			log.Logger().Info("allocation is not found, skipping while removing the node",
				zap.String("allocationId", allocID),
				zap.String("appID", app.ApplicationID),
				zap.String("nodeID", node.NodeID))
			continue
		}
		if err := app.GetQueue().DecAllocatedResource(alloc.AllocatedResource); err != nil {
			log.Logger().Warn("failed to release resources from queue",
				zap.String("appID", alloc.ApplicationID),
				zap.Error(err))
		}

		// the allocation is removed so add it to the list that we return
		released = append(released, alloc)
		log.Logger().Info("allocation removed",
			zap.String("allocationId", allocID),
			zap.String("nodeID", node.NodeID))
	}
	return released
}

func (pc *PartitionContext) calculateOutstandingRequests() []*objects.AllocationAsk {
	if !resources.StrictlyGreaterThanZero(pc.root.GetPendingResource()) {
		return nil
	}
	outstanding := make([]*objects.AllocationAsk, 0)
	pc.root.GetQueueOutstandingRequests(&outstanding)
	return outstanding
}

// Try regular allocation for the partition
// Lock free call this all locks are taken when needed in called functions
func (pc *PartitionContext) tryAllocate() *objects.Allocation {
	if !resources.StrictlyGreaterThanZero(pc.root.GetPendingResource()) {
		// nothing to do just return
		return nil
	}
	// try allocating from the root down
	alloc := pc.root.TryAllocate(pc.GetNodeIterator)
	if alloc != nil {
		return pc.allocate(alloc)
	}
	return nil
}

// Try process reservations for the partition
// Lock free call this all locks are taken when needed in called functions
func (pc *PartitionContext) tryReservedAllocate() *objects.Allocation {
	if !resources.StrictlyGreaterThanZero(pc.root.GetPendingResource()) {
		// nothing to do just return
		return nil
	}
	// try allocating from the root down
	alloc := pc.root.TryReservedAllocate(pc.GetNodeIterator)
	if alloc != nil {
		return pc.allocate(alloc)
	}
	return nil
}

// Process the allocation and make the left over changes in the partition.
func (pc *PartitionContext) allocate(alloc *objects.Allocation) *objects.Allocation {
	pc.Lock()
	defer pc.Unlock()
	// partition is locked nothing can change from now on
	// find the app make sure it still exists
	appID := alloc.ApplicationID
	app := pc.applications[appID]
	if app == nil {
		log.Logger().Info("Application was removed while allocating",
			zap.String("appID", appID))
		return nil
	}
	// find the node make sure it still exists
	// if the node was passed in use that ID instead of the one from the allocation
	// the node ID is set when a reservation is allocated on a non-reserved node
	var nodeID string
	if alloc.ReservedNodeID == "" {
		nodeID = alloc.NodeID
	} else {
		nodeID = alloc.ReservedNodeID
		log.Logger().Debug("Reservation allocated on different node",
			zap.String("current node", alloc.NodeID),
			zap.String("reserved node", nodeID),
			zap.String("appID", appID))
	}
	node := pc.nodes[nodeID]
	if node == nil {
		log.Logger().Info("Node was removed while allocating",
			zap.String("nodeID", nodeID),
			zap.String("appID", appID))
		return nil
	}
	// reservation
	if alloc.Result == objects.Reserved {
		pc.reserve(app, node, alloc.Ask)
		return nil
	}
	// unreserve
	if alloc.Result == objects.Unreserved || alloc.Result == objects.AllocatedReserved {
		pc.unReserve(app, node, alloc.Ask)
		if alloc.Result == objects.Unreserved {
			return nil
		}
		// remove the link to the reserved node
		alloc.ReservedNodeID = ""
	}

	// Safeguard against the unlikely case that we have clashes.
	// A clash points to entropy issues on the node.
	if _, found := pc.allocations[alloc.UUID]; found {
		for {
			allocationUUID := common.GetNewUUID()
			log.Logger().Warn("UUID clash, random generator might be lacking entropy",
				zap.String("uuid", alloc.UUID),
				zap.String("new UUID", allocationUUID))
			if pc.allocations[allocationUUID] == nil {
				alloc.UUID = allocationUUID
				break
			}
		}
	}
	pc.allocations[alloc.UUID] = alloc
	log.Logger().Info("scheduler allocation processed",
		zap.String("appID", alloc.ApplicationID),
		zap.String("allocationKey", alloc.AllocationKey),
		zap.String("allocatedResource", alloc.AllocatedResource.String()),
		zap.String("targetNode", alloc.NodeID))
	// pass the allocation back to the RM via the cluster context
	return alloc
}

// Process the reservation in the scheduler
// Lock free call this must be called holding the context lock
func (pc *PartitionContext) reserve(app *objects.Application, node *objects.Node, ask *objects.AllocationAsk) {
	appID := app.ApplicationID
	// app has node already reserved cannot reserve again
	if app.IsReservedOnNode(node.NodeID) {
		log.Logger().Info("Application is already reserved on node",
			zap.String("appID", appID),
			zap.String("nodeID", node.NodeID))
		return
	}
	// all ok, add the reservation to the app, this will also reserve the node
	if err := app.Reserve(node, ask); err != nil {
		log.Logger().Debug("Failed to handle reservation, error during update of app",
			zap.Error(err))
		return
	}

	// add the reservation to the queue list
	app.GetQueue().Reserve(appID)
	// increase the number of reservations for this app
	pc.reservedApps[appID]++

	log.Logger().Info("allocation ask is reserved",
		zap.String("appID", ask.ApplicationID),
		zap.String("queue", ask.QueueName),
		zap.String("allocationKey", ask.AllocationKey),
		zap.String("node", node.NodeID))
}

// Process the unreservation in the scheduler
// Lock free call this must be called holding the context lock
func (pc *PartitionContext) unReserve(app *objects.Application, node *objects.Node, ask *objects.AllocationAsk) {
	appID := app.ApplicationID
	if pc.reservedApps[appID] == 0 {
		log.Logger().Info("Application is not reserved in partition",
			zap.String("appID", appID))
		return
	}
	// all ok, remove the reservation of the app, this will also unReserve the node
	var err error
	var num int
	if num, err = app.UnReserve(node, ask); err != nil {
		log.Logger().Info("Failed to unreserve, error during allocate on the app",
			zap.Error(err))
		return
	}
	// remove the reservation of the queue
	app.GetQueue().UnReserve(appID, num)
	// make sure we cannot go below 0
	pc.unReserveCount(appID, num)

	log.Logger().Info("allocation ask is unreserved",
		zap.String("appID", ask.ApplicationID),
		zap.String("queue", ask.QueueName),
		zap.String("allocationKey", ask.AllocationKey),
		zap.String("node", node.NodeID),
		zap.Int("reservationsRemoved", num))
}

// Get the iterator for the sorted nodes list from the partition.
// Sorting should use a copy of the node list not the main list.
func (pc *PartitionContext) getNodeIteratorForPolicy(nodes []*objects.Node) interfaces.NodeIterator {
	pc.RLock()
	configuredPolicy := pc.nodeSortingPolicy.PolicyType
	pc.RUnlock()
	if configuredPolicy == policies.Unknown {
		return nil
	}
	// Sort Nodes based on the policy configured.
	objects.SortNodes(nodes, configuredPolicy)
	return newDefaultNodeIterator(nodes)
}

// Create a node iterator for the schedulable nodes based on the policy set for this partition.
// The iterator is nil if there are no schedulable nodes available.
func (pc *PartitionContext) GetNodeIterator() interfaces.NodeIterator {
	if nodeList := pc.getSchedulableNodes(); len(nodeList) != 0 {
		return pc.getNodeIteratorForPolicy(nodeList)
	}
	return nil
}

// Update the reservation counter for the app
// Lock free call this must be called holding the context lock
func (pc *PartitionContext) unReserveCount(appID string, asks int) {
	if num, found := pc.reservedApps[appID]; found {
		// decrease the number of reservations for this app and cleanup
		if num == asks {
			delete(pc.reservedApps, appID)
		} else {
			pc.reservedApps[appID] -= asks
		}
	}
}

func (pc *PartitionContext) GetTotalPartitionResource() *resources.Resource {
	pc.RLock()
	defer pc.RUnlock()

	return pc.totalPartitionResource
}

func (pc *PartitionContext) GetAllocatedResource() *resources.Resource {
	pc.RLock()
	defer pc.RUnlock()

	return pc.root.GetAllocatedResource()
}

func (pc *PartitionContext) GetTotalApplicationCount() int {
	pc.RLock()
	defer pc.RUnlock()
	return len(pc.applications)
}

func (pc *PartitionContext) GetTotalAllocationCount() int {
	pc.RLock()
	defer pc.RUnlock()
	return len(pc.allocations)
}

func (pc *PartitionContext) GetTotalNodeCount() int {
	pc.RLock()
	defer pc.RUnlock()
	return len(pc.nodes)
}

func (pc *PartitionContext) GetApplications() []*objects.Application {
	pc.RLock()
	defer pc.RUnlock()
	var appList []*objects.Application
	for _, app := range pc.applications {
		appList = append(appList, app)
	}
	return appList
}

func (pc *PartitionContext) GetNodes() []*objects.Node {
	pc.RLock()
	defer pc.RUnlock()
	var nodeList []*objects.Node
	for _, node := range pc.nodes {
		nodeList = append(nodeList, node)
	}
	return nodeList
}

// Add an allocation to the partition/node/application/queue during node registration.
// Queue max allocation is not checked as the allocation is part of a new node addition.
//
// NOTE: this is a lock free call. It should only be called holding the Partition lock.
func (pc *PartitionContext) addAllocation(alloc *objects.Allocation) error {
	if pc.isStopped() {
		return fmt.Errorf("partition %s is stopped cannot add new allocation %s", pc.Name, alloc.AllocationKey)
	}

	// if the allocation is node reported (aka recovery an existing allocation),
	// we must not generate an UUID for it, we directly use the UUID reported by shim
	// to track this allocation, a missing UUID is a broken allocation
	if alloc.UUID == "" {
		metrics.GetSchedulerMetrics().IncSchedulingError()
		return fmt.Errorf("failing to restore allocation %s for application %s: missing UUID",
			alloc.AllocationKey, alloc.ApplicationID)
	}

	log.Logger().Debug("adding recovered allocation",
		zap.String("partitionName", pc.Name),
		zap.String("appID", alloc.ApplicationID),
		zap.String("allocKey", alloc.AllocationKey),
		zap.String("UUID", alloc.UUID))

	// Check if allocation violates any resource restriction, or allocate on a
	// non-existent application or nodes.
	var node *objects.Node
	var app *objects.Application
	var ok bool

	if node, ok = pc.nodes[alloc.NodeID]; !ok {
		metrics.GetSchedulerMetrics().IncSchedulingError()
		return fmt.Errorf("failed to find node %s", alloc.NodeID)
	}

	if app, ok = pc.applications[alloc.ApplicationID]; !ok {
		metrics.GetSchedulerMetrics().IncSchedulingError()
		return fmt.Errorf("failed to find application %s", alloc.ApplicationID)
	}
	queue := app.GetQueue()

	// check the node status again
	if !node.IsSchedulable() {
		metrics.GetSchedulerMetrics().IncSchedulingError()
		return fmt.Errorf("node %s is not in schedulable state", node.NodeID)
	}

	// If the new allocation goes beyond the queue's max resource (recursive)?
	// Only check if it is allocated not when it is node reported.
	if err := queue.IncAllocatedResource(alloc.AllocatedResource, true); err != nil {
		metrics.GetSchedulerMetrics().IncSchedulingError()
		return fmt.Errorf("cannot allocate resource from application %s: %v ",
			alloc.ApplicationID, err)
	}

	node.AddAllocation(alloc)
	app.RecoverAllocationAsk(alloc.Ask)
	app.AddAllocation(alloc)
	pc.allocations[alloc.UUID] = alloc

	log.Logger().Debug("recovered allocation",
		zap.String("partitionName", pc.Name),
		zap.String("appID", alloc.ApplicationID),
		zap.String("allocationUid", alloc.UUID),
		zap.String("allocKey", alloc.AllocationKey))
	return nil
}

func (pc *PartitionContext) convertUGI(ugi *si.UserGroupInformation) (security.UserGroup, error) {
	pc.RLock()
	defer pc.RUnlock()
	return pc.userGroupCache.ConvertUGI(ugi)
}

// calculate overall nodes resource usage and returns a map as the result,
// where the key is the resource name, e.g memory, and the value is a []int,
// which is a slice with 10 elements,
// each element represents a range of resource usage,
// such as
//   0: 0%->10%
//   1: 10% -> 20%
//   ...
//   9: 90% -> 100%
// the element value represents number of nodes fall into this bucket.
// if slice[9] = 3, this means there are 3 nodes resource usage is in the range 80% to 90%.
func (pc *PartitionContext) CalculateNodesResourceUsage() map[string][]int {
	pc.RLock()
	defer pc.RUnlock()
	mapResult := make(map[string][]int)
	for _, node := range pc.nodes {
		for name, total := range node.GetCapacity().Resources {
			if float64(total) > 0 {
				resourceAllocated := float64(node.GetAllocatedResource().Resources[name])
				v := resourceAllocated / float64(total)
				idx := int(math.Dim(math.Ceil(v*10), 1))
				if dist, ok := mapResult[name]; !ok {
					newDist := make([]int, 10)
					for i := range newDist {
						newDist[i] = 0
					}
					mapResult[name] = newDist
					mapResult[name][idx]++
				} else {
					dist[idx]++
				}
			}
		}
	}
	return mapResult
}

func (pc *PartitionContext) removeAllocation(appID string, uuid string) []*objects.Allocation {
	pc.Lock()
	defer pc.Unlock()
	releasedAllocs := make([]*objects.Allocation, 0)
	var queue *objects.Queue = nil
	if app := pc.applications[appID]; app != nil {
		// when uuid not specified, remove all allocations from the app
		if uuid == "" {
			log.Logger().Debug("remove all allocations",
				zap.String("appID", app.ApplicationID))
			releasedAllocs = append(releasedAllocs, app.RemoveAllAllocations()...)
		} else {
			log.Logger().Debug("removing allocation",
				zap.String("appID", app.ApplicationID),
				zap.String("allocationId", uuid))
			if alloc := app.RemoveAllocation(uuid); alloc != nil {
				releasedAllocs = append(releasedAllocs, alloc)
			}
		}
		queue = app.GetQueue()
	}
	// for each allocations to release, update node.
	total := resources.NewResource()

	for _, alloc := range releasedAllocs {
		// remove allocation from node
		node := pc.nodes[alloc.NodeID]
		if node == nil || node.RemoveAllocation(alloc.UUID) == nil {
			log.Logger().Info("node allocation is not found while releasing resources",
				zap.String("appID", appID),
				zap.String("allocationId", alloc.UUID))
			continue
		}
		// remove from partition
		delete(pc.allocations, alloc.UUID)
		// track total resources
		total.AddTo(alloc.AllocatedResource)
	}
	// this nil check is not really needed as we can only reach here with a queue set, IDE complains without this
	if queue != nil {
		if err := queue.DecAllocatedResource(total); err != nil {
			log.Logger().Warn("failed to release resources from queue",
				zap.String("appID", appID),
				zap.String("allocationId", uuid),
				zap.Error(err))
		}
	}
	return releasedAllocs
}

func (pc *PartitionContext) removeAllocationAsk(appID string, allocationKey string) {
	pc.Lock()
	defer pc.Unlock()
	if app := pc.applications[appID]; app != nil {
		// remove the allocation asks from the app
		reservedAsks := app.RemoveAllocationAsk(allocationKey)
		log.Logger().Info("release allocation ask",
			zap.String("allocation", allocationKey),
			zap.String("appID", appID),
			zap.Int("reservedAskReleased", reservedAsks))
		// update the partition if the asks were reserved (clean up)
		if reservedAsks != 0 {
			pc.unReserveCount(appID, reservedAsks)
		}
	}
}
