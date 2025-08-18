/*
Copyright 2018 The Kubernetes Authors.

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
	"context"
	"errors"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/metrics"
)

// Operation type
type Operation int8

const (
	// Evict op
	Evict = iota
	// Pipeline op
	Pipeline
	// Allocate op
	Allocate
)

// String returns the string representation of Operation
func (op Operation) String() string {
	switch op {
	case Evict:
		return "Evict"
	case Pipeline:
		return "Pipeline"
	case Allocate:
		return "Allocate"
	default:
		return "Unknown"
	}
}

type operation struct {
	name   Operation
	task   *api.TaskInfo
	reason string
}

// Statement structure
type Statement struct {
	operations []operation
	ssn        *Session
}

// NewStatement returns new statement object
func NewStatement(ssn *Session) *Statement {
	return &Statement{
		ssn: ssn,
	}
}

// AddOperation adds operation to statement
func (s *Statement) Operations() []operation {
	return s.operations
}

// Evict the pod
func (s *Statement) Evict(reclaimee *api.TaskInfo, reason string) error {
	// Update status in session
	if job, found := s.ssn.Jobs[reclaimee.Job]; found {
		if err := job.UpdateTaskStatus(reclaimee, api.Releasing); err != nil {
			klog.Errorf("Failed to update task <%v/%v> status to %v when evicting in Session <%v>: %v",
				reclaimee.Namespace, reclaimee.Name, api.Releasing, s.ssn.UID, err)
		}
	} else {
		klog.Errorf("Failed to find Job <%s> in Session <%s> index when evicting.",
			reclaimee.Job, s.ssn.UID)
	}

	// Update task in node.
	if node, found := s.ssn.Nodes[reclaimee.NodeName]; found {
		err := node.UpdateTask(reclaimee)
		if err != nil {
			klog.Errorf("Failed to update task <%v/%v> in node %v for: %s",
				reclaimee.Namespace, reclaimee.Name, reclaimee.NodeName, err.Error())
			return err
		}
	}

	for _, eh := range s.ssn.eventHandlers {
		if eh.DeallocateFunc != nil {
			eh.DeallocateFunc(&Event{
				Task: reclaimee,
			})
		}
	}

	s.operations = append(s.operations, operation{
		name:   Evict,
		task:   reclaimee,
		reason: reason,
	})

	return nil
}

func (s *Statement) evict(reclaimee *api.TaskInfo, reason string) error {
	if err := s.ssn.cache.Evict(reclaimee, reason); err != nil {
		if e := s.unevict(reclaimee); e != nil {
			klog.Errorf("Faled to unevict task <%v/%v>: %v.", reclaimee.Namespace, reclaimee.Name, e)
		}
		return err
	}

	return nil
}

func (s *Statement) unevict(reclaimee *api.TaskInfo) error {
	// Update status in session
	job, found := s.ssn.Jobs[reclaimee.Job]
	if found {
		if err := job.UpdateTaskStatus(reclaimee, api.Running); err != nil {
			klog.Errorf("Failed to update task <%v/%v> status to %v when unevicting in Session <%v>: %v",
				reclaimee.Namespace, reclaimee.Name, api.Running, s.ssn.UID, err)
		}
	} else {
		klog.Errorf("Failed to find Job <%s> in Session <%s> index when unevicting.",
			reclaimee.Job, s.ssn.UID)
	}

	// Update task in node.
	if node, found := s.ssn.Nodes[reclaimee.NodeName]; found {
		err := node.UpdateTask(reclaimee)
		if err != nil {
			klog.Errorf("Failed to update task <%v/%v> in node %v for: %s",
				reclaimee.Namespace, reclaimee.Name, reclaimee.NodeName, err.Error())
			return err
		}
	}

	for _, eh := range s.ssn.eventHandlers {
		if eh.AllocateFunc != nil {
			eh.AllocateFunc(&Event{
				Task: reclaimee,
			})
		}
	}

	return nil
}

// Pipeline the task for the node
func (s *Statement) Pipeline(task *api.TaskInfo, hostname string, evictionOccurred bool) error {

	errInfos := make([]error, 0)
	job, found := s.ssn.Jobs[task.Job]
	if found {
		if err := job.UpdateTaskStatus(task, api.Pipelined); err != nil {
			klog.Errorf("Failed to update task <%v/%v> status to %v when pipeline in Session <%v>: %v",
				task.Namespace, task.Name, api.Pipelined, s.ssn.UID, err)
			errInfos = append(errInfos, err)
		}
	} else {
		err := fmt.Errorf("Failed to find Job <%s> in Session <%s> index when pipeline.",
			task.Job, s.ssn.UID)
		klog.Errorf("%v", err)
		errInfos = append(errInfos, err)
	}

	task.NodeName = hostname
	task.EvictionOccurred = evictionOccurred
	if node, found := s.ssn.Nodes[hostname]; found {
		if err := node.AddTask(task); err != nil {
			klog.Errorf("Failed to add task <%v/%v> to node <%v> when pipeline in Session <%v>: %v",
				task.Namespace, task.Name, hostname, s.ssn.UID, err)
			errInfos = append(errInfos, err)
		}
		klog.V(3).Infof("After pipelined Task <%v/%v> to Node <%v>: idle <%v>, used <%v>, releasing <%v>",
			task.Namespace, task.Name, node.Name, node.Idle, node.Used, node.Releasing)
	} else {
		err := fmt.Errorf("Failed to find Node <%s> in Session <%s> index when pipeline.",
			hostname, s.ssn.UID)
		klog.Errorf("%v", err)
		errInfos = append(errInfos, err)
	}

	for _, eh := range s.ssn.eventHandlers {
		if eh.AllocateFunc != nil {
			eventInfo := &Event{
				Task: task,
			}
			eh.AllocateFunc(eventInfo)
			if eventInfo.Err != nil {
				klog.Errorf("Failed to exec allocate callback functions for task <%v/%v> to node <%v> when pipeline in Session <%v>: %v",
					task.Namespace, task.Name, hostname, s.ssn.UID, eventInfo.Err)
				errInfos = append(errInfos, eventInfo.Err)
			}
		}
	}

	if len(errInfos) != 0 {
		return fmt.Errorf("Task(%s/%s) pipeline to node(%s) error and errInfos num is %d, UnPipeline will be called later to roll back the resources and status of the task.",
			task.Namespace, task.Name, hostname, len(errInfos))
	} else {
		s.operations = append(s.operations, operation{
			name: Pipeline,
			task: task,
		})
	}

	return nil
}

func (s *Statement) pipeline(task *api.TaskInfo) {
	// Set pipelined annotations on pod for cross-session state transfer
	if task.Pod != nil {
		api.SetPipelinedAnnotations(task.Pod, task.NodeName, task.EvictionOccurred)

		// Update pod annotations to Kubernetes cluster safely
		if err := s.updatePodAnnotationsSafely(task.Pod); err != nil {
			klog.Errorf("Failed to update pod annotations for <%v/%v>: %v", task.Namespace, task.Name, err)
			// Don't return error here to avoid breaking the pipeline flow, but log the error
		}
	}
}

func (s *Statement) UnPipeline(task *api.TaskInfo) error {
	job, found := s.ssn.Jobs[task.Job]
	if found {
		if err := job.UpdateTaskStatus(task, api.Pending); err != nil {
			klog.Errorf("Failed to update task <%v/%v> status to %v when unpipeline in Session <%v>: %v",
				task.Namespace, task.Name, api.Pending, s.ssn.UID, err)
		}
	} else {
		klog.Errorf("Failed to find Job <%s> in Session <%s> index when unpipeline.", task.Job, s.ssn.UID)
	}

	if node, found := s.ssn.Nodes[task.NodeName]; found {
		if err := node.RemoveTask(task); err != nil {
			klog.Errorf("Failed to remove task <%v/%v> to node <%v> when unpipeline in Session <%v>: %v",
				task.Namespace, task.Name, task.NodeName, s.ssn.UID, err)
		}
		klog.V(3).Infof("After unpipelined Task <%v/%v> to Node <%v>: idle <%v>, used <%v>, releasing <%v>",
			task.Namespace, task.Name, node.Name, node.Idle, node.Used, node.Releasing)
	} else {
		klog.Errorf("Failed to find Node <%s> in Session <%s> index when unpipeline.",
			task.NodeName, s.ssn.UID)
	}

	for _, eh := range s.ssn.eventHandlers {
		if eh.DeallocateFunc != nil {
			eventInfo := &Event{
				Task: task,
			}
			eh.DeallocateFunc(eventInfo)
			if eventInfo.Err != nil {
				klog.Errorf("Failed to exec deallocate callback functions for task <%v/%v> to node <%v> when pipeline in Session <%v>: %v",
					task.Namespace, task.Name, task.NodeName, s.ssn.UID, eventInfo.Err)
			}
		}
	}

	// Clear pipelined annotations from pod
	if task.Pod != nil {
		api.ClearPipelinedAnnotations(task.Pod)

		// Update pod annotations to Kubernetes cluster
		if _, err := s.ssn.cache.GetStatusUpdater().UpdatePodAnnotations(task.Pod); err != nil {
			klog.Errorf("Failed to update pod annotations for <%v/%v> when unpipelining: %v", task.Namespace, task.Name, err)
			// Don't return error here to avoid breaking the unpipeline flow, but log the error
		}
	}

	task.NodeName = ""
	task.JobAllocatedHyperNode = ""

	return nil
}

// Allocate the task to node
func (s *Statement) Allocate(task *api.TaskInfo, nodeInfo *api.NodeInfo) (err error) {
	errInfos := make([]error, 0)
	hostname := nodeInfo.Name
	klog.V(4).Infof("WucyDebug:Allocate before %v", task)

	// Special handling for pipelined tasks - they are already validated and on the node
	isPipelinedTask := task.Status == api.Pipelined
	if isPipelinedTask {
		klog.V(3).Infof("WucyDebug: Fast-tracking pipelined task <%v/%v> allocation to node <%s>", task.Namespace, task.Name, hostname)
	}

	task.Pod.Spec.NodeName = hostname
	klog.V(4).Infof("WucyDebug:Allocate after %v", task)

	// Only update status in session
	job, found := s.ssn.Jobs[task.Job]
	if found {
		if err := job.UpdateTaskStatus(task, api.Allocated); err != nil {
			klog.Errorf("Failed to update task <%v/%v> status from %v to %v when allocating in Session <%v>: %v",
				task.Namespace, task.Name, task.Status, api.Allocated, s.ssn.UID, err)
			errInfos = append(errInfos, err)
			return fmt.Errorf("critical: failed to update task status from %v to Allocated: %v", task.Status, err)
		}

		// Verify status change
		if task.Status != api.Allocated {
			err := fmt.Errorf("task status update failed: expected Allocated, got %v", task.Status)
			klog.Errorf("Task <%v/%v> status verification failed: %v", task.Namespace, task.Name, err)
			errInfos = append(errInfos, err)
			return err
		}
	} else {
		err := fmt.Errorf("Failed to find Job <%s> in Session <%s> index when allocating.",
			task.Job, s.ssn.UID)
		klog.Errorf("%v", err)
		errInfos = append(errInfos, err)
		return err
	}

	task.NodeName = hostname
	if node, found := s.ssn.Nodes[hostname]; found {
		// For pipelined tasks, the task is already on the node, so we skip AddTask
		if !isPipelinedTask {
			if err := node.AddTask(task); err != nil {
				klog.Errorf("Failed to add task <%v/%v> to node <%v> when allocating in Session <%v>: %v",
					task.Namespace, task.Name, hostname, s.ssn.UID, err)
				errInfos = append(errInfos, err)
			}
		} else {
			klog.V(3).Infof("WucyDebug: Skipping AddTask for pipelined task <%v/%v> - already on node <%v>", task.Namespace, task.Name, hostname)
		}
		klog.V(3).Infof("After allocated Task <%v/%v> to Node <%v>: idle <%v>, used <%v>, releasing <%v>",
			task.Namespace, task.Name, node.Name, node.Idle, node.Used, node.Releasing)
	} else {
		err := fmt.Errorf("Failed to find Node <%s> in Session <%s> index when allocating.",
			hostname, s.ssn.UID)
		klog.Errorf("%v", err)
		errInfos = append(errInfos, err)
	}

	// For pipelined tasks, skip event handler callbacks as they were already executed during pipelining
	if !isPipelinedTask {
		// Callbacks
		for _, eh := range s.ssn.eventHandlers {
			if eh.AllocateFunc != nil {
				eventInfo := &Event{
					Task: task,
				}
				eh.AllocateFunc(eventInfo)
				if eventInfo.Err != nil {
					klog.Errorf("Failed to exec allocate callback functions for task <%v/%v> to node <%v> when allocating in Session <%v>: %v",
						task.Namespace, task.Name, hostname, s.ssn.UID, eventInfo.Err)
					errInfos = append(errInfos, eventInfo.Err)
				}
			}
		}
	} else {
		klog.V(3).Infof("WucyDebug: Skipping event handler callbacks for pipelined task <%v/%v> - already executed during pipelining", task.Namespace, task.Name)
	}

	if len(errInfos) != 0 {
		return fmt.Errorf("Task %s/%s allocate to node %s error and errInfos num is %d, UnAllocate will be called later to roll back the resources and status of the task.",
			task.Namespace, task.Name, hostname, len(errInfos))
	} else {
		// Update status in session
		klog.V(3).Info("Allocating operations ...")
		s.operations = append(s.operations, operation{
			name: Allocate,
			task: task,
		})
	}

	return nil
}

// UnAllocate the pod for task
func (s *Statement) UnAllocate(task *api.TaskInfo) error {
	return s.unallocate(task)
}

func (s *Statement) allocate(task *api.TaskInfo) error {
	klog.V(3).Infof("WucyDebug: Starting allocate for task <%s> to node <%s>, current status: %s",
		task.Name, task.NodeName, task.Status)

	// Step 1: Create bind context
	klog.V(3).Infof("WucyDebug: Creating bind context for task <%s>", task.Name)
	bindContext := s.ssn.CreateBindContext(task)
	if bindContext == nil {
		err := fmt.Errorf("failed to create bind context for task %s", task.Name)
		klog.Errorf("WucyDebug: %v", err)
		return err
	}
	klog.V(3).Infof("WucyDebug: Successfully created bind context for task <%s>", task.Name)

	// Step 2: Add bind task to cache
	klog.V(3).Infof("WucyDebug: Adding bind task <%s> to cache", task.Name)
	if err := s.ssn.cache.AddBindTask(bindContext); err != nil {
		klog.Errorf("WucyDebug: Failed to add bind task <%s> to cache: %v", task.Name, err)
		return err
	}
	klog.V(3).Infof("WucyDebug: Successfully added bind task <%s> to cache", task.Name)

	// Step 3: Update task status to Binding
	klog.V(3).Infof("WucyDebug: Updating task <%s> status from %s to Binding", task.Name, task.Status)
	if job, found := s.ssn.Jobs[task.Job]; found {
		if err := job.UpdateTaskStatus(task, api.Binding); err != nil {
			klog.Errorf("WucyDebug: Failed to update task <%v/%v> status to Binding in Session <%v>: %v",
				task.Namespace, task.Name, s.ssn.UID, err)
			return err
		}
		klog.V(3).Infof("WucyDebug: Successfully updated task <%s> status to Binding", task.Name)
	} else {
		err := fmt.Errorf("failed to find job %s in session when updating task status to Binding", task.Job)
		klog.Errorf("WucyDebug: %v", err)
		return err
	}

	// Step 4: Clear pipelined annotations when task is successfully allocated
	// This handles the Pipeline → Allocated transition
	if task.Pod != nil && task.Status == api.Pipelined {
		klog.V(3).Infof("WucyDebug: Clearing pipelined annotations for allocated task <%v/%v>", task.Namespace, task.Name)
		api.ClearPipelinedAnnotations(task.Pod)

		// Update pod annotations to Kubernetes cluster
		if _, err := s.ssn.cache.GetStatusUpdater().UpdatePodAnnotations(task.Pod); err != nil {
			klog.Errorf("WucyDebug: Failed to clear pipelined annotations for <%v/%v>: %v", task.Namespace, task.Name, err)
			// Don't return error here to avoid breaking the allocation flow, but log the error
		} else {
			klog.V(3).Infof("WucyDebug: Successfully cleared pipelined annotations for task <%s>", task.Name)
		}
	}

	// Step 5: Update metrics
	klog.V(3).Infof("WucyDebug: Updating scheduling metrics for task <%s>", task.Name)
	metrics.UpdateTaskScheduleDuration(metrics.Duration(task.Pod.CreationTimestamp.Time))

	klog.V(3).Infof("WucyDebug: allocate completed successfully for task <%s>", task.Name)
	return nil
}

// unallocate the pod for task
func (s *Statement) unallocate(task *api.TaskInfo) error {
	// Check if this allocation operation was for a pipelined task by looking at the operations
	wasPipelined := false
	for _, op := range s.operations {
		if op.name == Allocate && op.task.UID == task.UID {
			// Check if the task had pipelined annotations before allocation
			if task.Pod != nil {
				if _, exists := task.Pod.Annotations[api.VolcanoPipelinedNodeAnnotation]; exists {
					wasPipelined = true
					klog.V(3).Infof("WucyDebug: Detected task <%v/%v> was pipelined based on annotations", task.Namespace, task.Name)
				}
			}
			break
		}
	}

	// Update status in session
	job, found := s.ssn.Jobs[task.Job]
	if found {
		// If it was pipelined, restore to Pipelined status, otherwise to Pending
		targetStatus := api.Pending
		if wasPipelined {
			targetStatus = api.Pipelined
			klog.V(3).Infof("WucyDebug: Restoring pipelined task <%v/%v> to Pipelined status during unallocation", task.Namespace, task.Name)
		}

		if err := job.UpdateTaskStatus(task, targetStatus); err != nil {
			klog.Errorf("Failed to update task <%v/%v> status to %v when unallocating in Session <%v>: %v",
				task.Namespace, task.Name, targetStatus, s.ssn.UID, err)
		}
	} else {
		klog.Errorf("Failed to find Job <%s> in Session <%s> index when unallocating.",
			task.Job, s.ssn.UID)
	}

	// For pipelined tasks, don't remove from node as they should remain pipelined
	if !wasPipelined {
		if node, found := s.ssn.Nodes[task.NodeName]; found {
			klog.V(3).Infof("Remove Task <%v> on node <%v>", task.Name, task.NodeName)
			err := node.RemoveTask(task)
			if err != nil {
				klog.Errorf("Failed to remove Task <%v> on node <%v> when unallocating: %s", task.Name, task.NodeName, err.Error())
			}
		}
	} else {
		klog.V(3).Infof("WucyDebug: Skipping RemoveTask for pipelined task <%v/%v> - should remain on node <%v>", task.Namespace, task.Name, task.NodeName)
	}

	// For pipelined tasks, skip event handler callbacks to avoid double resource deallocation
	if !wasPipelined {
		for _, eh := range s.ssn.eventHandlers {
			if eh.DeallocateFunc != nil {
				eh.DeallocateFunc(&Event{
					Task: task,
				})
			}
		}
	} else {
		klog.V(3).Infof("WucyDebug: Skipping deallocate event handlers for pipelined task <%v/%v> - resources were not allocated during this session", task.Namespace, task.Name)
	}

	// For non-pipelined tasks, clear the node assignment
	if !wasPipelined {
		task.NodeName = ""
		task.JobAllocatedHyperNode = ""
	}

	return nil
}

// Discard operation for evict, pipeline and allocate
func (s *Statement) Discard() {
	klog.V(3).Infof("WucyDebug: Discarding operations, total operations count: %d", len(s.operations))
	for i := len(s.operations) - 1; i >= 0; i-- {
		op := s.operations[i]
		klog.V(3).Infof("WucyDebug: Discarding operation %d: %s task <%s> from node <%s>",
			i, op.name.String(), op.task.Name, op.task.NodeName)
		op.task.GenerateLastTxContext()
		switch op.name {
		case Evict:
			klog.V(3).Infof("WucyDebug: Unevicting task <%s> from node <%s>", op.task.Name, op.task.NodeName)
			err := s.unevict(op.task)
			if err != nil {
				klog.Errorf("Failed to unevict task: %s", err.Error())
			}
		case Pipeline:
			klog.V(3).Infof("WucyDebug: Unpipelining task <%s> from node <%s>", op.task.Name, op.task.NodeName)
			err := s.UnPipeline(op.task)
			if err != nil {
				klog.Errorf("Failed to unpipeline task: %s", err.Error())
			}
		case Allocate:
			klog.V(3).Infof("WucyDebug: Unallocating task <%s> from node <%s> (this will cause Remove Task log)", op.task.Name, op.task.NodeName)
			err := s.unallocate(op.task)
			if err != nil {
				klog.Errorf("Failed to unallocate task: %s", err.Error())
			}
		}
	}
	klog.V(3).Info("WucyDebug: Discard operations completed")
}

// Commit operation for evict and pipeline
func (s *Statement) Commit() {
	klog.V(3).Infof("WucyDebug: Committing operations, total count: %d", len(s.operations))
	for i, op := range s.operations {
		klog.V(3).Infof("WucyDebug: Committing operation %d: %s task <%s>", i, op.name.String(), op.task.Name)
		op.task.ClearLastTxContext()
		switch op.name {
		case Evict:
			klog.V(3).Infof("WucyDebug: Executing evict for task <%s>", op.task.Name)
			err := s.evict(op.task, op.reason)
			if err != nil {
				klog.Errorf("WucyDebug: Failed to evict task <%s>: %s", op.task.Name, err.Error())
			} else {
				klog.V(3).Infof("WucyDebug: Successfully evicted task <%s>", op.task.Name)
			}
		case Pipeline:
			klog.V(3).Infof("WucyDebug: Executing pipeline for task <%s>", op.task.Name)
			s.pipeline(op.task)
			klog.V(3).Infof("WucyDebug: Successfully pipelined task <%s>", op.task.Name)
		case Allocate:
			klog.V(3).Infof("WucyDebug: Executing allocate for task <%s> to node <%s>", op.task.Name, op.task.NodeName)
			err := s.allocate(op.task)
			if err != nil {
				klog.Errorf("WucyDebug: Failed to allocate task <%s> to node <%s>: %v", op.task.Name, op.task.NodeName, err)
				if e := s.unallocate(op.task); e != nil {
					klog.Errorf("Failed to unallocate task <%v/%v>: %v.", op.task.Namespace, op.task.Name, e)
				}
			} else {
				klog.V(3).Infof("WucyDebug: Successfully allocated task <%s> to node <%s>", op.task.Name, op.task.NodeName)
			}
		}
	}
	klog.V(3).Info("WucyDebug: Commit operations completed")
}

func (s *Statement) SaveOperations() *Statement {
	s.outputOperations("Save operations: ", 4)

	stmtTmp := &Statement{}
	for _, op := range s.operations {
		stmtTmp.operations = append(stmtTmp.operations, operation{
			name:   op.name,
			task:   op.task.Clone(),
			reason: op.reason,
		})
	}
	return stmtTmp
}

func (s *Statement) RecoverOperations(stmt *Statement) error {
	if stmt == nil {
		return errors.New("statement is nil")
	}
	s.outputOperations("Recover operations: ", 4)
	for _, op := range stmt.operations {
		switch op.name {
		case Evict:
			err := s.Evict(op.task, op.reason)
			if err != nil {
				klog.Errorf("Failed to evict task: %s", err.Error())
				return err
			}
		case Pipeline:
			err := s.Pipeline(op.task, op.task.NodeName, false)
			if err != nil {
				klog.Errorf("Failed to pipeline task: %s", err.Error())
				return err
			}
		case Allocate:
			node := s.ssn.Nodes[op.task.NodeName]
			err := s.Allocate(op.task, node)
			if err != nil {
				if e := s.unallocate(op.task); e != nil {
					klog.Errorf("Failed to unallocate task <%v/%v>: %v", op.task.Namespace, op.task.Name, e)
				}
				klog.Errorf("Failed to allocate task <%v/%v>: %v", op.task.Namespace, op.task.Name, err)
				return err
			}
		}
	}
	return nil
}

func (s *Statement) outputOperations(msg string, level klog.Level) {
	if !klog.V(level).Enabled() {
		return
	}

	var buffer string
	for _, op := range s.operations {
		switch op.name {
		case Evict:
			buffer += fmt.Sprintf("task %s evict from node %s ", op.task.Name, op.task.NodeName)
		case Pipeline:
			buffer += fmt.Sprintf("task %s pipeline from node %s ", op.task.Name, op.task.NodeName)
		case Allocate:
			buffer += fmt.Sprintf("task %s allocate from node %s ", op.task.Name, op.task.NodeName)
		}
	}
	klog.V(level).Info(msg, buffer)
}

// updatePodAnnotationsSafely updates pod annotations without modifying other fields
func (s *Statement) updatePodAnnotationsSafely(pod *v1.Pod) error {
	// Get the current pod to avoid conflicts
	currentPod, err := s.ssn.cache.Client().CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get current pod: %v", err)
	}

	// Only update annotations, preserve all other fields
	podCopy := currentPod.DeepCopy()
	if podCopy.Annotations == nil {
		podCopy.Annotations = make(map[string]string)
	}

	// Copy only the annotations we want to update
	for key, value := range pod.Annotations {
		podCopy.Annotations[key] = value
	}

	// Use the status updater to update only annotations
	_, err = s.ssn.cache.GetStatusUpdater().UpdatePodAnnotations(podCopy)
	return err
}
