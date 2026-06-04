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

		// Gang feasibility pre-check (dry-run, no side effects).
		//
		// Before evicting ANY victim for this gang job, simulate—on cloned node
		// snapshots—whether the WHOLE job can reach its minMember this cycle by
		// reclaiming available victims. If it cannot, skip the job entirely and do
		// NOT evict a single victim.
		//
		// Rationale: stmt.Evict() has external side effects (it fires
		// DeallocateFunc with ExternalResources=true, which can release a victim's
		// DRA ResourceClaim / hostPort and trigger pod teardown) even before the
		// statement is committed. If the gang ultimately can't be assembled, those
		// victims are torn down in vain and respawn, producing a reclaim/respawn
		// loop. The dry-run touches only cloned NodeInfos, so it never evicts or
		// fires any handler; we only proceed to real eviction when the gang is
		// proven satisfiable.
		if !canReclaimGangForJob(ssn, job, task, totalNodesForJob(ssn, task)) {
			klog.V(3).Infof("Reclaim skipped for Job <%s/%s>: gang cannot be satisfied this cycle by reclaiming victims; not evicting any victim.",
				job.Namespace, job.Name)
			// Re-queue is unnecessary: the job stays Inqueue and will be
			// re-evaluated next session when resources may have changed.
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

			// Job-level (all-or-nothing) victim selection.
			//
			// Reclaiming only SOME pods of a low-priority gang job breaks that
			// job's gang (its surviving pods get torn down and the whole job is
			// recreated), wasting work. So once we decide to reclaim a victim job,
			// we expand the victim set to ALL of that job's reclaimable running
			// pods, evicting it as a whole. This keeps reclaim victim selection
			// consistent with gang semantics and avoids killing half a job.
			victims = expandVictimsToWholeJobs(ssn, job, victims)

			if err := util.ValidateVictims(task, n, victims); err != nil {
				klog.V(3).Infof("No validated victims on Node <%s>: %v", n.Name, err)
				continue
			}

			victimsQueue := ssn.BuildVictimsPriorityQueue(victims, task)

			resreq := task.InitResreq.Clone()
			reclaimed := api.EmptyResource()
			// Track victims staged for eviction on this node so we can roll them
			// back if the preemptor still can't be placed after eviction.
			evicted := make([]*api.TaskInfo, 0)

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
				evicted = append(evicted, reclaimee)
				reclaimed.Add(reclaimee.Resreq)
				// If reclaimed enough resources, break loop to avoid Sub panic.
				if resreq.LessEqual(reclaimed, api.Zero) {
					break
				}
			}

			klog.V(3).Infof("Reclaimed <%v> for task <%s/%s> requested <%v>.",
				reclaimed, task.Namespace, task.Name, task.InitResreq)

			// rollbackEvictions rolls back all victims staged on this node, so a
			// failed attempt never kills victims for real (eviction only happens
			// on stmt.Commit()).
			rollbackEvictions := func() {
				for i := len(evicted) - 1; i >= 0; i-- {
					if err := stmt.UnEvict(evicted[i]); err != nil {
						klog.Errorf("Failed to roll back eviction of Task <%s/%s> on Node <%s>: %v",
							evicted[i].Namespace, evicted[i].Name, n.Name, err)
					}
				}
			}

			if !task.InitResreq.LessEqual(reclaimed, api.Zero) {
				// Not enough reclaimed on this node; roll back and try next node.
				rollbackEvictions()
				continue
			}

			// validate-then-evict gate: the victims above were staged (in-session)
			// to free their resources on this node. Re-run the preempt-action
			// predicate to confirm the preemptor can be placed here after those
			// victims are gone, BEFORE we let the eviction be committed.
			//
			// We deliberately use PredicateForPreemptAction (not the full
			// PredicateFn) so that failures which are only resolvable across
			// scheduling cycles are still treated as resolvable:
			//   - DRA "cannot allocate all claims": a victim's ResourceClaim is
			//     released only after the pod is actually deleted, not when it is
			//     marked Releasing in-session, so the DRA Filter still reports this
			//     in the same cycle even though it WILL be satisfiable next cycle.
			//   - hostPort conflicts: the port frees up only after the victim pod
			//     terminates.
			// Using the full PredicateFn here would mis-classify these as fatal and
			// abort every candidate node, effectively disabling reclaim. The
			// preempt-action predicate still rejects truly unresolvable constraints
			// (e.g. NodeAffinity) and insufficient resources after eviction, which
			// is what we need to avoid evicting victims in vain.
			if err := ssn.PredicateForPreemptAction(task, n); err != nil {
				klog.V(3).Infof("Reclaim aborted on Node <%s> for Task <%s/%s>: not schedulable after evicting victims: %v",
					n.Name, task.Namespace, task.Name, err)
				rollbackEvictions()
				continue
			}

			if err := stmt.Pipeline(task, n.Name, true); err != nil {
				klog.Errorf("Failed to pipeline Task <%s/%s> on Node <%s>",
					task.Namespace, task.Name, n.Name)
			}

			// Ignore error of pipeline, will be corrected in next scheduling loop.
			assigned = true

			break
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

// totalNodesForJob returns the candidate node set used both by the gang
// feasibility pre-check and the real reclaim pass, so the dry-run and the apply
// phase reason over the same nodes.
func totalNodesForJob(ssn *framework.Session, task *api.TaskInfo) []*api.NodeInfo {
	return ssn.FilterOutUnschedulableAndUnresolvableNodesForTask(task)
}

// collectReclaimableVictimResreq returns the total resource of tasks on the node
// that the given preemptor job is allowed to reclaim (running, preemptable, and
// belonging to a different, reclaimable queue).
func collectReclaimableVictimResreq(ssn *framework.Session, job *api.JobInfo, node *api.NodeInfo) *api.Resource {
	total := api.EmptyResource()
	for _, t := range node.Tasks {
		if t.Status != api.Running || !t.Preemptable {
			continue
		}
		j, found := ssn.Jobs[t.Job]
		if !found || j.Queue == job.Queue {
			continue
		}
		if q, ok := ssn.Queues[j.Queue]; !ok || !q.Reclaimable() {
			continue
		}
		total.Add(t.Resreq)
	}
	return total
}

// canReclaimGangForJob dry-runs reclaim for the whole job on cloned node
// snapshots and reports whether the job can reach minMember this cycle by
// reclaiming victims.
//
// It performs NO real eviction and fires NO event handlers: all mutations happen
// on cloned NodeInfos, so victims are never disturbed. This lets the caller skip
// a gang entirely (evicting nothing) when it cannot be assembled, preventing the
// reclaim/respawn loop caused by evicting victims for a gang that won't fit.
func canReclaimGangForJob(ssn *framework.Session, job *api.JobInfo, sampleTask *api.TaskInfo, candidateNodes []*api.NodeInfo) bool {
	// Tasks still needed to satisfy the gang's minMember.
	needed := int(job.MinAvailable) - int(job.ReadyTaskNum()) - int(job.WaitingTaskNum())
	if needed <= 0 {
		// Already satisfied/pipelined enough; reclaim has nothing to gate on.
		return true
	}

	// Pending tasks of this job that still need placement, in scheduling order.
	pending := make([]*api.TaskInfo, 0, len(job.TaskStatusIndex[api.Pending]))
	for _, t := range job.TaskStatusIndex[api.Pending] {
		if t.SchGated {
			continue
		}
		pending = append(pending, t)
	}
	if len(pending) == 0 {
		return false
	}

	// Clone candidate node snapshots so the simulation has no side effects, and
	// pre-compute each node's reclaimable victim resources.
	type simNode struct {
		node       *api.NodeInfo
		victimRes  *api.Resource
		usedByPlan bool
	}
	sims := make([]*simNode, 0, len(candidateNodes))
	for _, n := range candidateNodes {
		sims = append(sims, &simNode{
			node:      n.Clone(),
			victimRes: collectReclaimableVictimResreq(ssn, job, n),
		})
	}

	placed := 0
	for _, t := range pending {
		if placed >= needed {
			break
		}
		for _, sn := range sims {
			if sn.usedByPlan {
				continue
			}
			// Only consider nodes the preemptor task can be placed on under the
			// preempt-action predicate (keeps DRA/hostPort resolvable, rejects
			// truly unresolvable constraints), consistent with the apply phase.
			if err := ssn.PredicateForPreemptAction(t, sn.node); err != nil {
				continue
			}
			// Available capacity after reclaiming this node's victims.
			avail := sn.node.FutureIdle().Clone().Add(sn.victimRes)
			if t.InitResreq.LessEqual(avail, api.Zero) {
				sn.usedByPlan = true
				placed++
				break
			}
		}
	}

	return placed >= needed
}

// expandVictimsToWholeJobs expands a set of per-pod victims into whole-job
// victim sets: for every victim job referenced by `victims`, ALL of that job's
// reclaimable running pods are included.
//
// Reclaim is gang-aware on the preemptor side; it must be gang-aware on the
// victim side too. Evicting only part of a low-priority gang job breaks its gang
// (the surviving pods are torn down and the whole job is recreated), which
// wastes work and can cause churn. Selecting victims at job granularity
// (all-or-nothing) ensures that whenever we touch a victim job we free its
// entire footprint cleanly.
//
// Only running, preemptable pods from a different, reclaimable queue are
// included (the same eligibility used when collecting per-pod victims), and
// tasks are cloned so node/job state is not mutated.
func expandVictimsToWholeJobs(ssn *framework.Session, preemptor *api.JobInfo, victims []*api.TaskInfo) []*api.TaskInfo {
	if len(victims) == 0 {
		return victims
	}

	// Collect the distinct victim jobs.
	victimJobIDs := make(map[api.JobID]struct{}, len(victims))
	for _, v := range victims {
		victimJobIDs[v.Job] = struct{}{}
	}

	expanded := make([]*api.TaskInfo, 0, len(victims))
	seen := make(map[api.TaskID]struct{}, len(victims))
	for jobID := range victimJobIDs {
		vjob, found := ssn.Jobs[jobID]
		if !found {
			continue
		}
		// Safety: never reclaim from the preemptor's own queue.
		if vjob.Queue == preemptor.Queue {
			continue
		}
		if q, ok := ssn.Queues[vjob.Queue]; !ok || !q.Reclaimable() {
			continue
		}
		for _, t := range vjob.Tasks {
			if t.Status != api.Running || !t.Preemptable {
				continue
			}
			if _, dup := seen[t.UID]; dup {
				continue
			}
			seen[t.UID] = struct{}{}
			expanded = append(expanded, t.Clone())
		}
	}

	if len(expanded) == 0 {
		// Fall back to the original victims if expansion produced nothing
		// (should not normally happen), to avoid losing reclaim capability.
		return victims
	}
	return expanded
}
