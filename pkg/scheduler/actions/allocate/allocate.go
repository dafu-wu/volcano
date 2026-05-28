/*
 Copyright 2021 The Volcano Authors.

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

package allocate

import (
	"time"

	"k8s.io/klog/v2"

	"volcano.sh/apis/pkg/apis/scheduling"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/conf"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/metrics"
	"volcano.sh/volcano/pkg/scheduler/util"
)

type Action struct {
	session *framework.Session
	// configured flag for error cache
	enablePredicateErrorCache bool

	// hyperNodeScoresByJob stores job total score for all available hyperNodes, this is used for accumulate
	// all nodes' scores in each available hyperNode only when job has hard network topology constrains
	// jobUID -> hyperNodeName -> score
	hyperNodeScoresByJob map[string]map[string]float64
}

func New() *Action {
	return &Action{
		enablePredicateErrorCache: true, // default to enable it
		hyperNodeScoresByJob:      make(map[string]map[string]float64),
	}
}

func (alloc *Action) Name() string {
	return "allocate"
}

func (alloc *Action) Initialize() {}

func (alloc *Action) parseArguments(ssn *framework.Session) {
	arguments := framework.GetArgOfActionFromConf(ssn.Configurations, alloc.Name())
	arguments.GetBool(&alloc.enablePredicateErrorCache, conf.EnablePredicateErrCacheKey)
}

func (alloc *Action) Execute(ssn *framework.Session) {
	klog.V(5).Infof("Enter Allocate ...")
	defer klog.V(5).Infof("Leaving Allocate ...")

	// DRA safety net: sweep any stale inFlightAllocations leaked from a previous action
	// (or a previous session not yet cleaned). This is cheap (O(claims)) and guarantees
	// that allocate starts with a clean DRA state, regardless of which prior path
	// (allocate/preempt/reclaim) may have left orphans.
	ssn.SweepStaleDRAInFlightAllocations("allocate.Execute")

	alloc.parseArguments(ssn)

	// the allocation for pod may have many stages
	// 1. pick a queue named Q (using ssn.QueueOrderFn)
	// 2. pick a job named J from Q (using ssn.JobOrderFn)
	// 3. pick a task T from J (using ssn.TaskOrderFn)
	// 4. use predicateFn to filter out node that T can not be allocated on.
	// 5. use ssn.NodeOrderFn to judge the best node and assign it to T

	// queues sort queues by QueueOrderFn.
	queues := util.NewPriorityQueue(ssn.QueueOrderFn)
	// jobsMap is used to find job with the highest priority in given queue.
	jobsMap := map[api.QueueID]*util.PriorityQueue{}

	alloc.session = ssn
	alloc.pickUpQueuesAndJobs(queues, jobsMap)
	klog.V(3).Infof("Try to allocate resource to %d Queues", len(jobsMap))
	alloc.allocateResources(queues, jobsMap)
}

func (alloc *Action) pickUpQueuesAndJobs(queues *util.PriorityQueue, jobsMap map[api.QueueID]*util.PriorityQueue) {
	ssn := alloc.session
	for _, job := range ssn.Jobs {
		// If not config enqueue action, change Pending pg into Inqueue state to avoid blocking job scheduling.
		if job.IsPending() {
			if conf.EnabledActionMap["enqueue"] {
				klog.V(4).Infof("Job <%s/%s> Queue <%s> skip allocate, reason: job status is pending.",
					job.Namespace, job.Name, job.Queue)
				continue
			} else {
				klog.V(4).Infof("Job <%s/%s> Queue <%s> status update from pending to inqueue, reason: no enqueue action is configured.",
					job.Namespace, job.Name, job.Queue)
				job.PodGroup.Status.Phase = scheduling.PodGroupInqueue
			}
		}

		if vr := ssn.JobValid(job); vr != nil && !vr.Pass {
			klog.V(4).Infof("Job <%s/%s> Queue <%s> skip allocate, reason: %v, message %v", job.Namespace, job.Name, job.Queue, vr.Reason, vr.Message)
			continue
		}

		if _, found := ssn.Queues[job.Queue]; !found {
			klog.Warningf("Skip adding Job <%s/%s> because its queue %s is not found",
				job.Namespace, job.Name, job.Queue)
			continue
		}

		if _, found := jobsMap[job.Queue]; !found {
			jobsMap[job.Queue] = util.NewPriorityQueue(ssn.JobOrderFn)
			queues.Push(ssn.Queues[job.Queue])
		}

		klog.V(4).Infof("Added Job <%s/%s> into Queue <%s>", job.Namespace, job.Name, job.Queue)
		jobsMap[job.Queue].Push(job)
	}
}

// allocateResources primarily accomplishes two steps:
// 1. picks up tasks.
// 2. allocates resources to these tasks. (this step is carried out by the allocateResourcesForTasks method.)
func (alloc *Action) allocateResources(queues *util.PriorityQueue, jobsMap map[api.QueueID]*util.PriorityQueue) {
	ssn := alloc.session
	pendingTasks := map[api.JobID]*util.PriorityQueue{}

	allNodes := ssn.NodeList

	// To pick <namespace, queue> tuple for job, we choose to pick namespace firstly.
	// Because we believe that number of queues would less than namespaces in most case.
	// And, this action would make the resource usage among namespace balanced.
	for {
		if queues.Empty() {
			break
		}

		queue := queues.Pop().(*api.QueueInfo)

		if ssn.Overused(queue) {
			klog.V(3).Infof("Queue <%s> is overused, ignore it.", queue.Name)
			continue
		}

		klog.V(3).Infof("Try to allocate resource to Jobs in Queue <%s>", queue.Name)

		jobs, found := jobsMap[queue.UID]
		if !found || jobs.Empty() {
			klog.V(4).Infof("Can not find jobs for queue %s.", queue.Name)
			continue
		}

		job := jobs.Pop().(*api.JobInfo)
		if _, found = pendingTasks[job.UID]; !found {
			tasks := util.NewPriorityQueue(ssn.TaskOrderFn)
			for _, status := range []api.TaskStatus{api.Pipelined, api.Pending} {
				for _, task := range job.TaskStatusIndex[status] {
					// Skip tasks whose pod are scheduling gated
					if task.SchGated {
						continue
					}

					// Skip BestEffort task in 'allocate' action.
					if task.Resreq.IsEmpty() {
						klog.V(4).Infof("Task <%v/%v> is BestEffort task, skip it.",
							task.Namespace, task.Name)
						continue
					}

					tasks.Push(task)
				}
			}
			pendingTasks[job.UID] = tasks
		}
		tasks := pendingTasks[job.UID]

		if tasks.Empty() {
			// put queue back again and try other jobs in this queue
			queues.Push(queue)
			continue
		}

		klog.V(3).Infof("Try to allocate resource to %d tasks of Job <%v/%v>",
			tasks.Len(), job.Namespace, job.Name)

		hardMode, highestAllowedTier := job.IsHardTopologyMode()
		var stmt *framework.Statement
		var tasksQueue *util.PriorityQueue
		if hardMode {
			if !alloc.session.HyperNodesReadyToSchedule {
				klog.ErrorS(nil, "RealNodesList not completely populated and not ready to schedule, please check logs for more details", "job", job.UID)
				continue
			}
			stmt, tasksQueue = alloc.allocateResourceForTasksWithTopology(tasks, job, queue, highestAllowedTier)
			// There are still left tasks that need to be allocated when min available < replicas, put the job back and set pending tasks.
			if tasksQueue != nil {
				jobs.Push(job)
				pendingTasks[job.UID] = tasksQueue
			}
		} else {
			stmt = alloc.allocateResourcesForTasks(tasks, job, queue, allNodes, "")
			// There are still left tasks that need to be allocated when min available < replicas, put the job back
			if tasks.Len() > 0 {
				jobs.Push(job)
			}
		}

		if stmt != nil {
			stmt.Commit()
		}

		// Put back the queue to priority queue after job's resource allocating finished,
		// To ensure that the priority of the queue is calculated based on the latest resource allocation situation.
		queues.Push(queue)
	}
}

func (alloc *Action) allocateResourceForTasksWithTopology(tasks *util.PriorityQueue, job *api.JobInfo, queue *api.QueueInfo, highestAllowedTier int) (*framework.Statement, *util.PriorityQueue) {
	jobStmtsByTier := make(map[int]map[string]*framework.Statement)
	hyperNodesWithLeftTasks := make(map[string]*util.PriorityQueue)
	ssn := alloc.session
	selectedTier := 0
	LCAHyperNodeMap := map[string]string{}
	jobAllocatedHyperNode := job.PodGroup.Annotations[api.JobAllocatedHyperNode]

	// Find a suitable hyperNode in one tier from down to top everytime to ensure that the selected hyperNode spans the least tier.
	for _, tier := range ssn.HyperNodesTiers {
		if tier > highestAllowedTier {
			klog.V(4).ErrorS(nil, "Skip search for higher tier cause highest allowed tier reached", "jobName", job.UID, "highestAllowedTier", highestAllowedTier, "tier", tier)
			break
		}
		if len(jobStmtsByTier) > 0 {
			klog.V(4).InfoS("Skip search for higher tier cause has found a suitable one", "tier", tier)
			break
		}
		for hyperNodeName := range ssn.HyperNodesSetByTier[tier] {
			nodes, ok := ssn.RealNodesList[hyperNodeName]
			if !ok {
				klog.ErrorS(nil, "HyperNode not exists.", "jobName", job.UID, "name", hyperNodeName, "tier", tier)
				continue
			}
			LCAHyperNodeMap[hyperNodeName] = hyperNodeName
			// The job still has remaining tasks to be scheduled, check whether the least common ancestor hyperNode still meets the requirement of the highest allowed tier
			if jobAllocatedHyperNode != "" {
				LCAHyperNode := ssn.HyperNodes.GetLCAHyperNode(hyperNodeName, jobAllocatedHyperNode)
				klog.V(3).InfoS("Get LCAHyperNode for job", "jobName", job.UID, "hyperNodeName", hyperNodeName, "jobAllocatedHyperNode", jobAllocatedHyperNode, "tier", tier, "LCAHyperNode", LCAHyperNode)
				hyperNodeInfo, ok := ssn.HyperNodes[LCAHyperNode]
				if !ok {
					continue
				}
				if hyperNodeInfo.Tier() > highestAllowedTier {
					klog.V(4).InfoS("Current tier of LCAHyperNode is higher than highestAllowedTier, skipping", "jobName", job.UID, "highestAllowedTier", highestAllowedTier, "tier", hyperNodeInfo.Tier())
					continue
				}
				LCAHyperNodeMap[hyperNodeName] = LCAHyperNode
			}
			// Clone tasks queue and rest job's fit err to make sure it's a clean cache when everytime filter a hyperNode and do not affect each other between hyperNodes.
			tasksQueue := tasks.Clone()
			job.ResetFitErr()
			klog.V(3).InfoS("Try to allocate resource for job in hyperNode", "jobName", job.UID, "hyperNodeName", hyperNodeName, "tier", tier)
			stmt := alloc.allocateResourcesForTasks(tasksQueue, job, queue, nodes, hyperNodeName)
			if stmt == nil {
				klog.V(4).InfoS("Cannot allocate resources for job with network topology constrains", "jobName", job.UID, "hyperNodeName", hyperNodeName, "tier", tier)
				continue
			}
			// If the hyperNode of selected has no enough resources stmt.Operations are empty when minavailiable < replicas and tasks in job rescheduled, skip it.
			if len(stmt.Operations()) == 0 {
				klog.V(4).InfoS("Cannot allocate resources for job with network topology constrains when the stmt.Operations be empty", "jobName", job.UID, "hyperNodeName", hyperNodeName, "tier", tier)
				continue
			}
			// Find an available hyperNode.
			if _, ok = jobStmtsByTier[tier]; !ok {
				jobStmtsByTier[tier] = make(map[string]*framework.Statement)
			}
			selectedTier = tier
			// Just cache the allocation result because we haven't chosen the best hyperNode.
			jobStmtsByTier[tier][hyperNodeName] = stmt.SaveOperations()
			// Rollback current statement and try next hyperNode.
			stmt.Discard()

			// If there are still unallocated tasks in the task queue, return and continue scheduling later.
			if tasksQueue.Len() > 0 {
				hyperNodesWithLeftTasks[hyperNodeName] = tasksQueue
			}
		}
	}

	if len(jobStmtsByTier) > 0 {
		hyperNodes := make([]string, 0, len(jobStmtsByTier[selectedTier]))
		for hyperNodeName := range jobStmtsByTier[selectedTier] {
			hyperNodes = append(hyperNodes, hyperNodeName)
		}
		klog.V(4).InfoS("Find available hyperNodes for job", "jobName", job.UID, "tier", selectedTier, "hyperNodes", hyperNodes)
	}
	stmt, hyperNode := alloc.selectBestHyperNode(jobStmtsByTier[selectedTier], job)
	if jobNewHyperNode, ok := LCAHyperNodeMap[hyperNode]; ok {
		if jobNewHyperNode != jobAllocatedHyperNode {
			job.PodGroup.GetAnnotations()[api.JobAllocatedHyperNode] = jobNewHyperNode
		}
	}
	return stmt, hyperNodesWithLeftTasks[hyperNode]
}

// selectBestStmt return a stmt and best hyperNode related to the stmt, it will
// score and select the best hyperNode among all available hyperNodes.
func (alloc *Action) selectBestHyperNode(jobStmts map[string]*framework.Statement, job *api.JobInfo) (*framework.Statement, string) {
	var bestStmt *framework.Statement
	bestHyperNodeName := ""
	ssn := alloc.session

	switch {
	case len(jobStmts) == 0:
		klog.V(3).InfoS("Failed to allocate resource for job, no available hyperNode is under highest allowed tier", "jobName", job.UID)
		return nil, bestHyperNodeName
	case len(jobStmts) == 1:
		for hyperNodeName, stmt := range jobStmts {
			bestStmt = stmt
			bestHyperNodeName = hyperNodeName
			break
		}
	case len(jobStmts) > 1:
		candidateHyperNodeGroups := make(map[string][]*api.NodeInfo)
		for hyperNodeName := range jobStmts {
			candidateHyperNodeGroups[hyperNodeName] = ssn.RealNodesList[hyperNodeName]
		}

		hyperNodeScores, err := util.PrioritizeHyperNodes(candidateHyperNodeGroups, alloc.hyperNodeScoresByJob[string(job.UID)], job, ssn.HyperNodeOrderMapFn)
		if err != nil {
			klog.V(3).ErrorS(err, "Failed to allocate resource for job", "jobName", job.UID)
			return nil, bestHyperNodeName
		}

		bestHyperNodeName = util.SelectBestHyperNode(hyperNodeScores)

		var exists bool
		bestStmt, exists = jobStmts[bestHyperNodeName]
		if !exists {
			klog.ErrorS(nil, "Couldn't find best hyperNode in statements", "jobName", job.UID, "hyperNode", bestHyperNodeName)
			return nil, bestHyperNodeName
		}
	}

	// Recover the stmt and return.
	if bestStmt == nil || bestHyperNodeName == "" {
		return nil, bestHyperNodeName
	}
	finalStmt := framework.NewStatement(ssn)
	err := finalStmt.RecoverOperations(bestStmt)
	if err != nil {
		klog.ErrorS(err, "Failed to recover operations", "jobName", job.UID, "hyperNode", bestHyperNodeName)
		return nil, bestHyperNodeName
	}
	klog.V(3).InfoS("Allocate job to hyperNode", "jobName", job.UID, "hyperNode", bestHyperNodeName)
	return finalStmt, bestHyperNodeName
}

func (alloc *Action) allocateResourcesForTasks(tasks *util.PriorityQueue, job *api.JobInfo, queue *api.QueueInfo, allNodes []*api.NodeInfo, hyperNode string) *framework.Statement {
	ssn := alloc.session
	stmt := framework.NewStatement(ssn)
	ph := util.NewPredicateHelper()
	// For TopologyNetworkSoftMode
	jobNewAllocatedHyperNode := job.PodGroup.GetAnnotations()[api.JobAllocatedHyperNode]

	for !tasks.Empty() {
		task := tasks.Pop().(*api.TaskInfo)
		skipNominatedNode := false
		restorePipelinedNode := ""
		if task.Status == api.Pipelined {
			var prepared bool
			prepared, skipNominatedNode, restorePipelinedNode = alloc.preparePipelinedTask(stmt, task)
			if !prepared {
				continue
			}
		}
		restorePipelinedTask := func(reason string) {
			if restorePipelinedNode == "" {
				return
			}
			alloc.restorePreparedPipelinedTask(stmt, task, restorePipelinedNode, reason)
			restorePipelinedNode = ""
		}

		if !ssn.Allocatable(queue, task) {
			klog.V(3).Infof("Queue <%s> is overused when considering task <%s>, ignore it.", queue.Name, task.Name)
			// A queue-overused result means the existing reservation is no longer
			// legal under current quota accounting. Do not restore it here; letting
			// the task fall back to Pending clears stale nominatedNodeName state and
			// prevents old partial gang reservations from blocking real idle nodes.
			continue
		}

		// check if the task with its spec has already predicates failed
		if job.TaskHasFitErrors(task) {
			klog.V(5).Infof("Task %s with role spec %s has already predicated failed, skip", task.Name, task.TaskRole)
			restorePipelinedTask("cached fit errors")
			continue
		}

		klog.V(3).Infof("There are <%d> nodes for Job <%v/%v>", len(ssn.Nodes), job.Namespace, job.Name)

		if err := ssn.PrePredicateFn(task); err != nil {
			klog.V(3).Infof("PrePredicate for task %s/%s failed for: %v", task.Namespace, task.Name, err)
			fitErrors := api.NewFitErrors()
			for _, ni := range allNodes {
				fitErrors.SetNodeError(ni.Name, err)
			}
			job.NodesFitErrors[task.UID] = fitErrors
			restorePipelinedTask("pre-predicate failure")
			break
		}

		var predicateNodes []*api.NodeInfo
		var fitErrors *api.FitErrors

		// "NominatedNodeName" can potentially be set in a previous scheduling cycle as a result of preemption.
		// This node is likely the only candidate that will fit the pod, and hence we try it first before iterating over all nodes.
		if !skipNominatedNode && len(task.Pod.Status.NominatedNodeName) > 0 {
			if nominatedNodeInfo, ok := ssn.Nodes[task.Pod.Status.NominatedNodeName]; ok && task.InitResreq.LessEqual(nominatedNodeInfo.FutureIdle(), api.Zero) {
				predicateNodes, fitErrors = ph.PredicateNodes(task, []*api.NodeInfo{nominatedNodeInfo}, alloc.predicate, alloc.enablePredicateErrorCache)
			}
		}

		// If the nominated node is not found or the nominated node is not suitable for the task, we need to find a suitable node for the task from all nodes.
		if len(predicateNodes) == 0 {
			predicateNodes, fitErrors = ph.PredicateNodes(task, allNodes, alloc.predicate, alloc.enablePredicateErrorCache)
		}

		if len(predicateNodes) == 0 {
			// TODO: Need to add PostFilter extension point implementation here. For example, the DRA plugin includes the PostFilter extension point,
			// but the DRA's PostFilter only occurs in extreme error conditions: Suppose a pod uses two claims. In the first scheduling attempt,
			// a node is picked and PreBind manages to update the first claim so that it is allocated and reserved for the pod.
			// But then updating the second claim fails (e.g., apiserver down) and the scheduler has to retry. During the next pod scheduling attempt,
			// the original node is no longer usable for other reasons. Other nodes are not usable either because of the allocated claim.
			// The DRA scheduler plugin detects that and then when scheduling fails (= no node passed filtering), it recovers by de-allocating the allocated claim in PostFilter.
			job.NodesFitErrors[task.UID] = fitErrors
			// Assume that all left tasks are allocatable, but can not meet gang-scheduling min member,
			// so we should break from continuously allocating.
			// otherwise, should continue to find other allocatable task
			if job.NeedContinueAllocating() {
				restorePipelinedTask("predicate failure")
				continue
			} else {
				restorePipelinedTask("predicate failure")
				break
			}
		}

		if job.IsSoftTopologyMode() {
			task.JobAllocatedHyperNode = jobNewAllocatedHyperNode
		}

		bestNode, highestScore := alloc.prioritizeNodes(ssn, task, predicateNodes)
		if bestNode == nil {
			restorePipelinedTask("no best node")
			continue
		}

		alloc.sumNodeScoresInHyperNode(string(job.UID), hyperNode, highestScore)

		if err := alloc.allocateResourcesForTask(stmt, task, bestNode, job); err == nil {
			jobNewAllocatedHyperNode = getJobNewAllocatedHyperNode(ssn, bestNode.Name, job, jobNewAllocatedHyperNode)
		} else {
			restorePipelinedTask("allocation failure")
		}

		if ssn.JobReady(job) && !tasks.Empty() {
			break
		}
	}

	if ssn.JobReady(job) {
		klog.V(3).InfoS("Job ready, return statement", "jobName", job.UID)
		updateJobAllocatedHyperNode(job, jobNewAllocatedHyperNode)
		return stmt
	} else {
		if !ssn.JobPipelined(job) {
			stmt.Discard()
		} else {
			stmt.DiscardAllocations()
			// When JobPipelined=true but not Ready, we preserve the Pipelined state
			// (for reclaim/preempt visibility and event reporting), but we must clean up
			// DRA inFlightAllocations to prevent leaks across jobs in the same session.
			//
			// Without this cleanup, the DRA Reserve plugin's inFlightAllocation entries
			// persist in the shared DRAManager, causing ListAllAllocatedDevices() to
			// return false "device in use" results. This makes the DRA Filter incorrectly
			// report "cannot allocate all claims" for subsequent jobs because it thinks
			// devices (e.g., channel-0) are occupied when they are not.
			ssn.CleanupDRAInFlightAllocations(stmt, job)
		}
		return nil
	}
}

// getJobNewAllocatedHyperNode Obtain the newly allocated hyperNode for the job in soft topology mode
func getJobNewAllocatedHyperNode(ssn *framework.Session, bestNode string, job *api.JobInfo, jobAllocatedHyperNode string) string {
	if !job.IsSoftTopologyMode() {
		return ""
	}

	jobNewAllocatedHyperNode := jobAllocatedHyperNode
	hyperNode := util.FindHyperNodeForNode(bestNode, ssn.RealNodesList, ssn.HyperNodesTiers, ssn.HyperNodesSetByTier)
	if hyperNode != "" {
		if jobNewAllocatedHyperNode == "" {
			jobNewAllocatedHyperNode = hyperNode
		} else {
			jobNewAllocatedHyperNode = ssn.HyperNodes.GetLCAHyperNode(hyperNode, jobNewAllocatedHyperNode)
		}
	}
	return jobNewAllocatedHyperNode
}

// updateJobAllocatedHyperNode update job allocated hyperNode in soft topology mode
func updateJobAllocatedHyperNode(job *api.JobInfo, jobNewAllocatedHyperNode string) {
	if !job.IsSoftTopologyMode() {
		return
	}

	if job.PodGroup.GetAnnotations()[api.JobAllocatedHyperNode] != jobNewAllocatedHyperNode {
		job.PodGroup.GetAnnotations()[api.JobAllocatedHyperNode] = jobNewAllocatedHyperNode
	}
}

func (alloc *Action) sumNodeScoresInHyperNode(jobUID, hyperNode string, score float64) {
	// normal vc job without networkTopology has no hyperNode, skip node scores accumulation.
	if hyperNode == "" {
		return
	}
	if alloc.hyperNodeScoresByJob[jobUID] == nil {
		alloc.hyperNodeScoresByJob[jobUID] = make(map[string]float64)
	}

	alloc.hyperNodeScoresByJob[jobUID][hyperNode] += score
}

// prioritizeNodes selects the highest score node.
func (alloc *Action) prioritizeNodes(ssn *framework.Session, task *api.TaskInfo, predicateNodes []*api.NodeInfo) (*api.NodeInfo, float64) {
	// Candidate nodes are divided into two gradients:
	// - the first gradient node: a list of free nodes that satisfy the task resource request;
	// - The second gradient node: the node list whose sum of node idle resources and future idle meets the task resource request;
	// Score the first gradient node first. If the first gradient node meets the requirements, ignore the second gradient node list,
	// otherwise, score the second gradient node and select the appropriate node.
	var candidateNodes [][]*api.NodeInfo
	var idleCandidateNodes []*api.NodeInfo
	var futureIdleCandidateNodes []*api.NodeInfo
	for _, n := range predicateNodes {
		if task.InitResreq.LessEqual(n.Idle, api.Zero) {
			idleCandidateNodes = append(idleCandidateNodes, n)
		} else if task.InitResreq.LessEqual(n.FutureIdle(), api.Zero) {
			futureIdleCandidateNodes = append(futureIdleCandidateNodes, n)
		} else {
			klog.V(5).Infof("Predicate filtered node %v, idle: %v and future idle: %v do not meet the requirements of task: %v",
				n.Name, n.Idle, n.FutureIdle(), task.Name)
		}
	}
	candidateNodes = append(candidateNodes, idleCandidateNodes)
	candidateNodes = append(candidateNodes, futureIdleCandidateNodes)

	var bestNode *api.NodeInfo
	var higestScore float64
	for index, nodes := range candidateNodes {
		if klog.V(5).Enabled() {
			for _, node := range nodes {
				klog.V(5).Infof("node %v, idle: %v, future idle: %v", node.Name, node.Idle, node.FutureIdle())
			}
		}
		switch {
		case len(nodes) == 0:
			klog.V(5).Infof("Task: %v, no matching node is found in the candidateNodes（index: %d） list.", task.Name, index)
		case len(nodes) == 1: // If only one node after predicate, just use it.
			bestNode = nodes[0]
		case len(nodes) > 1: // If more than one node after predicate, using "the best" one
			nodeScores := util.PrioritizeNodes(task, nodes, ssn.BatchNodeOrderFn, ssn.NodeOrderMapFn, ssn.NodeOrderReduceFn)

			bestNode = ssn.BestNodeFn(task, nodeScores)
			if bestNode == nil {
				bestNode, higestScore = util.SelectBestNodeAndScore(nodeScores)
			}
		}

		// If a proper node is found in idleCandidateNodes, skip futureIdleCandidateNodes and directly return the node information.
		if bestNode != nil {
			break
		}
	}
	return bestNode, higestScore
}

func (alloc *Action) allocateResourcesForTask(stmt *framework.Statement, task *api.TaskInfo, node *api.NodeInfo, job *api.JobInfo) (err error) {
	// Allocate idle resource to the task.
	if task.InitResreq.LessEqual(node.Idle, api.Zero) {
		klog.V(3).Infof("Binding Task <%v/%v> to node <%v>", task.Namespace, task.Name, node.Name)
		if err = stmt.Allocate(task, node); err != nil {
			klog.Errorf("Failed to bind Task %v on %v in Session %v, err: %v",
				task.UID, node.Name, alloc.session.UID, err)
			if rollbackErr := stmt.UnAllocate(task); rollbackErr != nil {
				klog.Errorf("Failed to unallocate Task %v on %v in Session %v for %v.",
					task.UID, node.Name, alloc.session.UID, rollbackErr)
			}
		} else {
			metrics.UpdateE2eSchedulingDurationByJob(job.Name, string(job.Queue), job.Namespace, metrics.Duration(job.CreationTimestamp.Time))
			metrics.UpdateE2eSchedulingLastTimeByJob(job.Name, string(job.Queue), job.Namespace, time.Now())
		}
		return
	}

	klog.V(3).Infof("Predicates failed in allocate for task <%s/%s> on node <%s> with limited resources",
		task.Namespace, task.Name, node.Name)

	// Allocate releasing resource to the task if any.
	if task.InitResreq.LessEqual(node.FutureIdle(), api.Zero) {
		klog.V(3).Infof("Pipelining Task <%v/%v> to node <%v> for <%v> on <%v>",
			task.Namespace, task.Name, node.Name, task.InitResreq, node.Releasing)
		if err = stmt.Pipeline(task, node.Name, true); err != nil {
			klog.Errorf("Failed to pipeline Task %v on %v in Session %v for %v.",
				task.UID, node.Name, alloc.session.UID, err)
		} else {
			metrics.UpdateE2eSchedulingDurationByJob(job.Name, string(job.Queue), job.Namespace, metrics.Duration(job.CreationTimestamp.Time))
			metrics.UpdateE2eSchedulingLastTimeByJob(job.Name, string(job.Queue), job.Namespace, time.Now())
		}
	}
	return
}

func (alloc *Action) preparePipelinedTask(stmt *framework.Statement, task *api.TaskInfo) (bool, bool, string) {
	if task.Status != api.Pipelined {
		return true, false, ""
	}

	node, found := alloc.session.Nodes[task.NodeName]
	if !found {
		originalNodeName := task.NodeName
		klog.V(3).Infof("Pipelined Task <%s/%s> nominated node <%s> no longer exists, unpipeline and retry scheduling.",
			task.Namespace, task.Name, originalNodeName)
		if err := stmt.UnPipeline(task); err != nil {
			klog.Errorf("Failed to unpipeline Task <%s/%s> from missing node <%s>: %v",
				task.Namespace, task.Name, originalNodeName, err)
			return false, false, ""
		}
		return true, true, ""
	}

	originalNodeName := node.Name
	if !task.InitResreq.LessEqual(node.Idle, api.Zero) {
		klog.V(3).Infof("Pipelined Task <%s/%s> on Node <%s> still waits for idle <%v> to satisfy <%v>, retry scheduling across all nodes.",
			task.Namespace, task.Name, node.Name, node.Idle, task.InitResreq)
		if err := stmt.UnPipeline(task); err != nil {
			klog.Errorf("Failed to unpipeline Task <%s/%s> on Node <%s> before retry: %v",
				task.Namespace, task.Name, node.Name, err)
			return false, false, ""
		}
		return true, true, originalNodeName
	}

	klog.V(3).Infof("Pipelined Task <%s/%s> on Node <%s> now has idle resource, retry binding.",
		task.Namespace, task.Name, node.Name)
	if err := stmt.UnPipeline(task); err != nil {
		klog.Errorf("Failed to unpipeline Task <%s/%s> on Node <%s> before binding: %v",
			task.Namespace, task.Name, node.Name, err)
		return false, false, ""
	}
	return true, false, originalNodeName
}

func (alloc *Action) restorePreparedPipelinedTask(stmt *framework.Statement, task *api.TaskInfo, nodeName, reason string) {
	if nodeName == "" || task.Status != api.Pending || task.NodeName != "" {
		return
	}
	klog.V(3).Infof("Restoring Pipelined Task <%s/%s> to Node <%s> after %s.",
		task.Namespace, task.Name, nodeName, reason)
	if err := stmt.Pipeline(task, nodeName, true); err != nil {
		klog.Errorf("Failed to restore Pipelined Task <%s/%s> on Node <%s> after %s: %v",
			task.Namespace, task.Name, nodeName, reason, err)
	}
}

func (alloc *Action) predicate(task *api.TaskInfo, node *api.NodeInfo) error {
	// Check for Resource Predicate
	var statusSets api.StatusSets
	if ok, resources := task.InitResreq.LessEqualWithResourcesName(node.FutureIdle(), api.Zero); !ok {
		statusSets = append(statusSets, &api.Status{Code: api.Unschedulable, Reason: api.WrapInsufficientResourceReason(resources)})
		return api.NewFitErrWithStatus(task, node, statusSets...)
	}
	return alloc.session.PredicateForAllocateAction(task, node)
}

func (alloc *Action) UnInitialize() {}
