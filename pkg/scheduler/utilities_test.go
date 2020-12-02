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
	"strconv"
	"testing"

	"gotest.tools/assert"

	"github.com/apache/incubator-yunikorn-core/pkg/common/configs"
	"github.com/apache/incubator-yunikorn-core/pkg/common/resources"
	"github.com/apache/incubator-yunikorn-core/pkg/common/security"
	"github.com/apache/incubator-yunikorn-core/pkg/scheduler/objects"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

const (
	appID1   = "app-1"
	appID2   = "app-2"
	nodeID1  = "node-1"
	nodeID2  = "node-2"
	defQueue = "root.default"
	rmID     = "testRM"
)

func newBasePartition() (*PartitionContext, error) {
	conf := configs.PartitionConfig{
		Name: "test",
		Queues: []configs.QueueConfig{
			{
				Name:      "root",
				Parent:    true,
				SubmitACL: "*",
				Queues: []configs.QueueConfig{
					{
						Name:   "default",
						Parent: false,
						Queues: nil,
					},
				},
			},
		},
		PlacementRules: nil,
		Limits:         nil,
		Preemption:     configs.PartitionPreemptionConfig{},
		NodeSortPolicy: configs.NodeSortingPolicy{},
	}

	return newPartitionContext(conf, rmID, nil)
}

func newConfiguredPartition() (*PartitionContext, error) {
	conf := configs.PartitionConfig{
		Name: "test",
		Queues: []configs.QueueConfig{
			{
				Name:      "root",
				Parent:    true,
				SubmitACL: "*",
				Queues: []configs.QueueConfig{
					{
						Name:   "leaf",
						Parent: false,
						Queues: nil,
					}, {
						Name:   "parent",
						Parent: true,
						Queues: []configs.QueueConfig{
							{
								Name:   "sub-leaf",
								Parent: false,
								Queues: nil,
							},
						},
					},
				},
			},
		},
		PlacementRules: nil,
		Limits:         nil,
		Preemption:     configs.PartitionPreemptionConfig{},
		NodeSortPolicy: configs.NodeSortingPolicy{},
	}
	return newPartitionContext(conf, rmID, nil)
}

func newApplication(appID, partition, queueName string) *objects.Application {
	return objects.NewApplication(appID, partition, queueName, security.UserGroup{}, nil, nil, rmID)
}

func newAllocationAsk(allocKey, appID string, res *resources.Resource) *objects.AllocationAsk {
	return newAllocationAskRepeat(allocKey, appID, res, 1)
}

func newAllocationAskRepeat(allocKey, appID string, res *resources.Resource, repeat int32) *objects.AllocationAsk {
	return newAllocationAskPriority(allocKey, appID, res, repeat, 1)
}

func newAllocationAskPriority(allocKey, appID string, res *resources.Resource, repeat int32, prio int32) *objects.AllocationAsk {
	return objects.NewAllocationAsk(&si.AllocationAsk{
		AllocationKey:  allocKey,
		ApplicationID:  appID,
		PartitionName:  "test",
		ResourceAsk:    res.ToProto(),
		MaxAllocations: repeat,
		Priority: &si.Priority{
			Priority: &si.Priority_PriorityValue{PriorityValue: prio},
		},
	})
}

func newNodeWithResources(nodeID string, max, occupied *resources.Resource) *objects.Node {
	proto := &si.NewNodeInfo{
		NodeID:              nodeID,
		Attributes:          nil,
		SchedulableResource: max.ToProto(),
		OccupiedResource:    occupied.ToProto(),
	}
	return objects.NewNode(proto)
}

func newNodeMaxResource(nodeID string, max *resources.Resource) *objects.Node {
	return newNodeWithResources(nodeID, max, nil)
}

// Simple node with just an ID in the node.
// That is all we need for iteration
func newNode(nodeID string) *objects.Node {
	proto := &si.NewNodeInfo{
		NodeID:     nodeID,
		Attributes: nil,
	}
	return objects.NewNode(proto)
}

// A list of nodes that can be iterated over.
func newSchedNodeList(number int) []*objects.Node {
	list := make([]*objects.Node, number)
	for i := 0; i < number; i++ {
		num := strconv.Itoa(i)
		node := newNode("node-" + num)
		list[i] = node
	}
	return list
}

// partition with an expected basic queue hierarchy
// root -> parent -> leaf1
//      -> leaf2
// and 2 nodes: node-1 & node-2
func createQueuesNodes(t *testing.T) *PartitionContext {
	partition, err := newConfiguredPartition()
	assert.NilError(t, err, "test partition create failed with error")
	var res *resources.Resource
	res, err = resources.NewResourceFromConf(map[string]string{"first": "10"})
	assert.NilError(t, err, "failed to create basic resource")
	err = partition.AddNode(newNodeMaxResource("node-1", res), nil)
	assert.NilError(t, err, "test node1 add failed unexpected")
	err = partition.AddNode(newNodeMaxResource("node-2", res), nil)
	assert.NilError(t, err, "test node2 add failed unexpected")
	return partition
}
