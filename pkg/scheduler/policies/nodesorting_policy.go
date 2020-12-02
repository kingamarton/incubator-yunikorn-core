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

package policies

import (
	"fmt"

	"go.uber.org/zap"

	"github.com/apache/incubator-yunikorn-core/pkg/log"
)

type NodeSortingPolicy struct {
	PolicyType SortingPolicy
}

type SortingPolicy int

const (
	BinPackingPolicy SortingPolicy = iota
	FairnessPolicy
	Unknown
)

func (nsp SortingPolicy) String() string {
	return [...]string{"binpacking", "fair", "undefined"}[nsp]
}

func FromString(str string) (SortingPolicy, error) {
	switch str {
	// fair is the default policy when not set
	case FairnessPolicy.String(), "":
		return FairnessPolicy, nil
	case BinPackingPolicy.String():
		return BinPackingPolicy, nil
	default:
		return Unknown, fmt.Errorf("undefined policy: %s", str)
	}
}

func NewNodeSortingPolicy(policyType string) *NodeSortingPolicy {
	pType, err := FromString(policyType)
	if err != nil {
		log.Logger().Debug("node sorting policy defaulted to 'undefined'",
			zap.Error(err))
	}
	sp := &NodeSortingPolicy{
		PolicyType: pType,
	}

	log.Logger().Debug("new node sorting policy added",
		zap.String("type", pType.String()))
	return sp
}
