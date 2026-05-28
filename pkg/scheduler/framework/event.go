/*
Copyright 2018 The Kubernetes Authors.
Copyright 2018-2024 The Volcano Authors.

Modifications made by Volcano authors:
- Added Err field to Event structure for error handling

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
	"volcano.sh/volcano/pkg/scheduler/api"
)

// EventOperation describes the scheduler state transition that raised an event.
//
// Event operations form two symmetric pairs that drive plugin/event handlers:
//
//   - Allocate  <-> Deallocate : real allocation lifecycle. Allocate is fired
//     when a task is assigned to a node and may later be bound; Deallocate is
//     fired when a real allocation is rolled back (e.g. statement discard) or
//     released (e.g. evict). Both carry ExternalResources=true so that handlers
//     run external side effects such as DRA/volume Reserve/Unreserve and device
//     Allocate/Release.
//
//   - Pipeline <-> UnPipeline : scheduler-internal future reservation against
//     releasing resources. These do NOT touch external resources, so handlers
//     must keep ExternalResources=false. UnPipeline is also used to roll back a
//     Pipeline reservation.
//
// Note that Statement.unevict (used by reclaim/preempt rollback) emits
// EventAllocate with ExternalResources=true, which is the correct counter-event
// for an earlier Evict (a real release): it must re-Reserve DRA claims and
// re-Allocate devices.
type EventOperation string

const (
	// EventAllocate is a real allocation which may later be bound.
	EventAllocate EventOperation = "Allocate"
	// EventPipeline is a scheduler-internal reservation against releasing resources.
	EventPipeline EventOperation = "Pipeline"
	// EventDeallocate rolls back or releases a real allocation.
	EventDeallocate EventOperation = "Deallocate"
	// EventUnPipeline releases a scheduler-internal pipeline reservation.
	EventUnPipeline EventOperation = "UnPipeline"
)

// Event structure
type Event struct {
	Task      *api.TaskInfo
	Err       error
	Operation EventOperation

	// ExternalResources indicates whether callbacks may run external reservation
	// side effects, such as DRA/volume Reserve and device Allocate/Release.
	// Pipeline events are scheduler-internal future reservations and must keep
	// this false; real allocate/evict rollback paths set it true.
	ExternalResources bool
}

// EventHandler structure
type EventHandler struct {
	AllocateFunc   func(event *Event)
	DeallocateFunc func(event *Event)
}
