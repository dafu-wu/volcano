/*
Copyright 2018 The Kubernetes Authors.
Copyright 2018-2025 The Volcano Authors.

Modifications made by Volcano authors:
- Added job validation and preemption policy support
- Enhanced victim selection with priority queue ordering
- Added PrePredicate validation and node filtering

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

package reclaim

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/util"
)

type Action struct{}

func New() *Action {
	return &Action{}
}

func (ra *Action) Name() string {
	return "reclaim"
}

func (ra *Action) Initialize() {}

func (ra *Action) Execute(ssn *framework.Session) {
	klog.V(5).Infof("Enter Reclaim ...")
	pendingStmts := map[api.JobID]*framework.Statement{}
	defer func() {
		for jobID, stmt := range pendingStmts {
			klog.V(3).Infof("Discarding partial reclaim operations for Job <%s>; job did not reach pipelined state.", jobID)
			stmt.Discard()
		}
		// DRA safety net at action exit. Predicate checks in this action may create
		// Reserve() inFlight entries before a reclaim statement is committed.
		// Sweep stale entries here to prevent cross-action poisoning.
		ssn.SweepStaleDRAInFlightAllocations("reclaim.Execute.defer")
		klog.V(5).Infof("Leaving Reclaim ...")
	}()

	// DRA safety net at action entry.
	ssn.SweepStaleDRAInFlightAllocations("reclaim.Execute")

	queues := util.NewPriorityQueue(ssn.QueueOrderFn)
	queueMap := map[api.QueueID]*api.QueueInfo{}

	preemptorsMap := map[api.QueueID]*util.PriorityQueue{}
	preemptorTasks := map[api.JobID]*util.PriorityQueue{}

	klog.V(3).Infof("There are <%d> Jobs and <%d> Queues in total for scheduling.",
		len(ssn.Jobs), len(ssn.Queues))

	for _, job := range ssn.Jobs {
		if job.IsPending() {
			continue
		}

		if vr := ssn.JobValid(job); vr != nil && !vr.Pass {
			klog.V(4).Infof("Job <%s/%s> Queue <%s> skip reclaim, reason: %v, message %v", job.Namespace, job.Name, job.Queue, vr.Reason, vr.Message)
			continue
		}

		if queue, found := ssn.Queues[job.Queue]; !found {
			klog.Errorf("Failed to find Queue <%s> for Job <%s/%s>", job.Queue, job.Namespace, job.Name)
			continue
		} else if _, existed := queueMap[queue.UID]; !existed {
			klog.V(4).Infof("Added Queue <%s> for Job <%s/%s>", queue.Name, job.Namespace, job.Name)
			queueMap[queue.UID] = queue
			queues.Push(queue)
		}

		if ssn.JobStarving(job) {
			if _, found := preemptorsMap[job.Queue]; !found {
				preemptorsMap[job.Queue] = util.NewPriorityQueue(ssn.JobOrderFn)
			}
			preemptorsMap[job.Queue].Push(job)
			preemptorTasks[job.UID] = util.NewPriorityQueue(ssn.TaskOrderFn)
			for _, task := range job.TaskStatusIndex[api.Pending] {
				if task.SchGated {
					continue
				}
				preemptorTasks[job.UID].Push(task)
			}
		}
	}

	for {
		// If no queues, break
		if queues.Empty() {
			break
		}

		var job *api.JobInfo
		var task *api.TaskInfo

		queue := queues.Pop().(*api.QueueInfo)
		if ssn.Overused(queue) {
			klog.V(3).Infof("Queue <%s> is overused, ignore it.", queue.Name)
			continue
		}

		// Found "high" priority job
		jobs, found := preemptorsMap[queue.UID]
		if !found || jobs.Empty() {
			continue
		} else {
			job = jobs.Pop().(*api.JobInfo)
		}

		// Found "high" priority task to reclaim others
		if tasks, found := preemptorTasks[job.UID]; !found || tasks.Empty() || !ssn.JobStarving(job) {
			continue
		} else {
			task = tasks.Pop().(*api.TaskInfo)
		}

		stmt, found := pendingStmts[job.UID]
		if !found {
			stmt = framework.NewStatement(ssn)
			pendingStmts[job.UID] = stmt
		}

		if task.Pod.Spec.PreemptionPolicy != nil && *task.Pod.Spec.PreemptionPolicy == v1.PreemptNever {
			klog.V(3).Infof("Task %s/%s is not eligible to preempt other tasks due to preemptionPolicy is Never", task.Namespace, task.Name)
			// TODO: In order to avoid blocking other tasks in the job or other jobs in the queue to reclaim resources, the job and queue need
			// to be pushed back to the priority queue. Need to refactor the framework of reclaim action, see issue: https://github.com/volcano-sh/volcano/issues/3738
			jobs.Push(job)
			queues.Push(queue)
			continue
		}

		//In allocate action we need check all the ancestor queues' capability but in reclaim action we should just check current queue's capability, and reclaim happens when queue not allocatable so we just need focus on the reclaim here.
		//So it's more descriptive to user preempt related semantics.
		if !ssn.Preemptive(queue, task) {
			klog.V(3).Infof("Queue <%s> cannot reclaim by preempting others when considering task <%s/%s>: Preemptive check failed (check plugin configurations for preemptive policies)",
				queue.Name, task.Namespace, task.Name)
			continue
		}

		if err := ssn.PrePredicateFn(task); err != nil {
			klog.V(3).Infof("PrePredicate for task %s/%s failed for: %v", task.Namespace, task.Name, err)
			continue
		}

		assigned := false
		// we should filter out those nodes that are UnschedulableAndUnresolvable status got in allocate action
		totalNodes := ssn.FilterOutUnschedulableAndUnresolvableNodesForTask(task)
		for _, n := range totalNodes {
			// When filtering candidate nodes, need to consider the node statusSets instead of the err information.
			// refer to kube-scheduler preemption code: https://github.com/kubernetes/kubernetes/blob/9d87fa215d9e8020abdc17132d1252536cd752d2/pkg/scheduler/framework/preemption/preemption.go#L422
			if err := ssn.PredicateForPreemptAction(task, n); err != nil {
				klog.V(4).Infof("Reclaim predicate for task %s/%s on node %s return error %v ", task.Namespace, task.Name, n.Name, err)
				continue
			}

			klog.V(3).Infof("Considering Task <%s/%s> on Node <%s>.", task.Namespace, task.Name, n.Name)

			var reclaimees []*api.TaskInfo
			for _, task := range n.Tasks {
				// Ignore non running task.
				if task.Status != api.Running {
					klog.V(4).Infof("Task <%s/%s> on Node <%s> cannot be preempted: task status is %s (not Running)",
						task.Namespace, task.Name, n.Name, task.Status)
					continue
				}
				if !task.Preemptable {
					klog.V(4).Infof("Task <%s/%s> on Node <%s> cannot be preempted: task is not preemptable",
						task.Namespace, task.Name, n.Name)
					continue
				}

				if j, found := ssn.Jobs[task.Job]; !found {
					klog.V(4).Infof("Task <%s/%s> on Node <%s> cannot be preempted: job not found",
						task.Namespace, task.Name, n.Name)
					continue
				} else if j.Queue != job.Queue {
					q := ssn.Queues[j.Queue]
					if !q.Reclaimable() {
						klog.V(4).Infof("Task <%s/%s> on Node <%s> cannot be preempted: queue <%s> is not reclaimable",
							task.Namespace, task.Name, n.Name, q.Name)
						continue
					}
					// Clone task to avoid modify Task's status on node.
					klog.V(4).Infof("Task <%s/%s> on Node <%s> can be preempted: task is running, preemptable, and from reclaimable queue <%s>",
						task.Namespace, task.Name, n.Name, q.Name)
					reclaimees = append(reclaimees, task.Clone())
				} else {
					klog.V(4).Infof("Task <%s/%s> on Node <%s> cannot be preempted: task is from the same queue <%s> as preemptor",
						task.Namespace, task.Name, n.Name, j.Queue)
				}
			}

			if len(reclaimees) == 0 {
				klog.V(4).Infof("No reclaimees on Node <%s>.", n.Name)
				continue
			}

			victims := ssn.Reclaimable(task, reclaimees)

			if err := util.ValidateVictims(task, n, victims); err != nil {
				klog.V(3).Infof("No validated victims on Node <%s>: %v", n.Name, err)
				continue
			}

			victimsQueue := ssn.BuildVictimsPriorityQueue(victims, task)

			resreq := task.InitResreq.Clone()
			reclaimed := api.EmptyResource()

			// Reclaim victims for tasks.
			for !victimsQueue.Empty() {
				reclaimee := victimsQueue.Pop().(*api.TaskInfo)
				klog.Errorf("Try to reclaim Task <%s/%s> for Tasks <%s/%s>",
					reclaimee.Namespace, reclaimee.Name, task.Namespace, task.Name)
				if err := stmt.Evict(reclaimee, "reclaim"); err != nil {
					klog.Errorf("Failed to reclaim Task <%s/%s> for Tasks <%s/%s>: %v",
						reclaimee.Namespace, reclaimee.Name, task.Namespace, task.Name, err)
					continue
				}
				reclaimed.Add(reclaimee.Resreq)
				// If reclaimed enough resources, break loop to avoid Sub panic.
				if resreq.LessEqual(reclaimed, api.Zero) {
					break
				}
			}

			klog.V(3).Infof("Reclaimed <%v> for task <%s/%s> requested <%v>.",
				reclaimed, task.Namespace, task.Name, task.InitResreq)

			if task.InitResreq.LessEqual(reclaimed, api.Zero) {
				if err := stmt.Pipeline(task, n.Name, true); err != nil {
					klog.Errorf("Failed to pipeline Task <%s/%s> on Node <%s>",
						task.Namespace, task.Name, n.Name)
				}

				// Ignore error of pipeline, will be corrected in next scheduling loop.
				assigned = true

				break
			}
		}

		if assigned {
			if ssn.JobPipelined(job) {
				klog.V(3).Infof("Job <%s/%s> reached pipelined state after reclaim; committing reclaim operations.", job.Namespace, job.Name)
				stmt.Commit()
				delete(pendingStmts, job.UID)
			} else {
				jobs.Push(job)
			}
		}
		queues.Push(queue)
	}
}

func (ra *Action) UnInitialize() {
}
