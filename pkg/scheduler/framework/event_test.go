/*
Copyright 2026 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"volcano.sh/volcano/pkg/scheduler/api"
)

func newEventFlagTestSession() (*Session, *api.TaskInfo, *api.NodeInfo) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-pod",
			UID:       types.UID("test-pod"),
		},
	}
	task := api.NewTaskInfo(pod)
	job := api.NewJobInfo(task.Job, task)

	nodeObj := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("32Gi"),
				v1.ResourcePods:   resource.MustParse("100"),
			},
		},
	}
	node := api.NewNodeInfo(nodeObj)

	ssn := &Session{
		UID:   types.UID("session-a"),
		Jobs:  map[api.JobID]*api.JobInfo{task.Job: job},
		Nodes: map[string]*api.NodeInfo{node.Name: node},
	}

	return ssn, task, node
}

func TestStatementPipelineDoesNotUseExternalResources(t *testing.T) {
	ssn, task, node := newEventFlagTestSession()

	var allocateFlags []bool
	var deallocateFlags []bool
	var allocateOps []EventOperation
	var deallocateOps []EventOperation
	ssn.eventHandlers = []*EventHandler{{
		AllocateFunc: func(event *Event) {
			allocateFlags = append(allocateFlags, event.ExternalResources)
			allocateOps = append(allocateOps, event.Operation)
		},
		DeallocateFunc: func(event *Event) {
			deallocateFlags = append(deallocateFlags, event.ExternalResources)
			deallocateOps = append(deallocateOps, event.Operation)
		},
	}}

	stmt := NewStatement(ssn)
	if err := stmt.Pipeline(task, node.Name, false); err != nil {
		t.Fatalf("Pipeline returned error: %v", err)
	}
	if len(allocateFlags) != 1 || allocateFlags[0] {
		t.Fatalf("Pipeline AllocateFunc ExternalResources = %v, want [false]", allocateFlags)
	}
	if len(allocateOps) != 1 || allocateOps[0] != EventPipeline {
		t.Fatalf("Pipeline AllocateFunc Operation = %v, want [%s]", allocateOps, EventPipeline)
	}

	if err := stmt.UnPipeline(task); err != nil {
		t.Fatalf("UnPipeline returned error: %v", err)
	}
	if len(deallocateFlags) != 1 || deallocateFlags[0] {
		t.Fatalf("UnPipeline DeallocateFunc ExternalResources = %v, want [false]", deallocateFlags)
	}
	if len(deallocateOps) != 1 || deallocateOps[0] != EventUnPipeline {
		t.Fatalf("UnPipeline DeallocateFunc Operation = %v, want [%s]", deallocateOps, EventUnPipeline)
	}
}

func TestStatementAllocateUsesExternalResources(t *testing.T) {
	ssn, task, node := newEventFlagTestSession()

	var allocateFlags []bool
	var deallocateFlags []bool
	var allocateOps []EventOperation
	var deallocateOps []EventOperation
	ssn.eventHandlers = []*EventHandler{{
		AllocateFunc: func(event *Event) {
			allocateFlags = append(allocateFlags, event.ExternalResources)
			allocateOps = append(allocateOps, event.Operation)
		},
		DeallocateFunc: func(event *Event) {
			deallocateFlags = append(deallocateFlags, event.ExternalResources)
			deallocateOps = append(deallocateOps, event.Operation)
		},
	}}

	stmt := NewStatement(ssn)
	if err := stmt.Allocate(task, node); err != nil {
		t.Fatalf("Allocate returned error: %v", err)
	}
	if len(allocateFlags) != 1 || !allocateFlags[0] {
		t.Fatalf("AllocateFunc ExternalResources = %v, want [true]", allocateFlags)
	}
	if len(allocateOps) != 1 || allocateOps[0] != EventAllocate {
		t.Fatalf("AllocateFunc Operation = %v, want [%s]", allocateOps, EventAllocate)
	}

	if err := stmt.UnAllocate(task); err != nil {
		t.Fatalf("UnAllocate returned error: %v", err)
	}
	if len(deallocateFlags) != 1 || !deallocateFlags[0] {
		t.Fatalf("UnAllocate DeallocateFunc ExternalResources = %v, want [true]", deallocateFlags)
	}
	if len(deallocateOps) != 1 || deallocateOps[0] != EventDeallocate {
		t.Fatalf("UnAllocate DeallocateFunc Operation = %v, want [%s]", deallocateOps, EventDeallocate)
	}
}

func TestSessionPipelineDoesNotUseExternalResources(t *testing.T) {
	ssn, task, node := newEventFlagTestSession()

	var allocateFlags []bool
	var allocateOps []EventOperation
	ssn.eventHandlers = []*EventHandler{{
		AllocateFunc: func(event *Event) {
			allocateFlags = append(allocateFlags, event.ExternalResources)
			allocateOps = append(allocateOps, event.Operation)
		},
	}}

	if err := ssn.Pipeline(task, node.Name); err != nil {
		t.Fatalf("Pipeline returned error: %v", err)
	}
	if len(allocateFlags) != 1 || allocateFlags[0] {
		t.Fatalf("Session Pipeline AllocateFunc ExternalResources = %v, want [false]", allocateFlags)
	}
	if len(allocateOps) != 1 || allocateOps[0] != EventPipeline {
		t.Fatalf("Session Pipeline AllocateFunc Operation = %v, want [%s]", allocateOps, EventPipeline)
	}
}
