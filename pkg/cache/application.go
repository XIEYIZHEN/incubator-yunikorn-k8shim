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

package cache

import (
	"fmt"
	"sort"
	"sync"

	"github.com/looplab/fsm"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/apache/incubator-yunikorn-core/pkg/api"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/apis/yunikorn.apache.org/v1alpha1"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/appmgmt/interfaces"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/constants"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/events"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/utils"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/conf"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/dispatcher"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/log"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

type Application struct {
	applicationID              string
	queue                      string
	partition                  string
	user                       string
	taskMap                    map[string]*Task
	tags                       map[string]string
	schedulingPolicy           v1alpha1.SchedulingPolicy
	taskGroups                 []v1alpha1.TaskGroup
	placeholderOwnerReferences []metav1.OwnerReference
	sm                         *fsm.FSM
	lock                       *sync.RWMutex
	schedulerAPI               api.SchedulerAPI
	placeholderAsk             *si.Resource // total placeholder request for the app (all task groups)
	placeholderTimeoutInSec    int64
}

func (app *Application) String() string {
	return fmt.Sprintf("applicationID: %s, queue: %s, partition: %s,"+
		" totalNumOfTasks: %d, currentState: %s",
		app.applicationID, app.queue, app.partition, len(app.taskMap), app.GetApplicationState())
}

func NewApplication(appID, queueName, user string, tags map[string]string, scheduler api.SchedulerAPI) *Application {
	taskMap := make(map[string]*Task)
	app := &Application{
		applicationID:           appID,
		queue:                   queueName,
		partition:               constants.DefaultPartition,
		user:                    user,
		taskMap:                 taskMap,
		tags:                    tags,
		schedulingPolicy:        v1alpha1.SchedulingPolicy{},
		taskGroups:              make([]v1alpha1.TaskGroup, 0),
		lock:                    &sync.RWMutex{},
		schedulerAPI:            scheduler,
		placeholderTimeoutInSec: 0,
	}

	var states = events.States().Application
	app.sm = fsm.NewFSM(
		states.New,
		fsm.Events{
			{Name: string(events.SubmitApplication),
				Src: []string{states.New},
				Dst: states.Submitted},
			{Name: string(events.RecoverApplication),
				Src: []string{states.New},
				Dst: states.Recovering},
			{Name: string(events.AcceptApplication),
				Src: []string{states.Submitted, states.Recovering},
				Dst: states.Accepted},
			{Name: string(events.TryReserve),
				Src: []string{states.Accepted},
				Dst: states.Reserving},
			{Name: string(events.UpdateReservation),
				Src: []string{states.Reserving},
				Dst: states.Reserving},
			{Name: string(events.RunApplication),
				Src: []string{states.Accepted, states.Reserving, states.Running},
				Dst: states.Running},
			{Name: string(events.ReleaseAppAllocation),
				Src: []string{states.Running},
				Dst: states.Running},
			{Name: string(events.ReleaseAppAllocation),
				Src: []string{states.Failed},
				Dst: states.Failed},
			{Name: string(events.ReleaseAppAllocationAsk),
				Src: []string{states.Running, states.Accepted, states.Reserving},
				Dst: states.Running},
			{Name: string(events.ReleaseAppAllocationAsk),
				Src: []string{states.Failed},
				Dst: states.Failed},
			{Name: string(events.CompleteApplication),
				Src: []string{states.Running},
				Dst: states.Completed},
			{Name: string(events.RejectApplication),
				Src: []string{states.Submitted},
				Dst: states.Rejected},
			{Name: string(events.FailApplication),
				Src: []string{states.Submitted, states.Rejected, states.Accepted, states.Running, states.Reserving},
				Dst: states.Failed},
			{Name: string(events.KillApplication),
				Src: []string{states.Accepted, states.Running, states.Reserving},
				Dst: states.Killing},
			{Name: string(events.KilledApplication),
				Src: []string{states.Killing},
				Dst: states.Killed},
		},
		fsm.Callbacks{
			string(events.SubmitApplication):       app.handleSubmitApplicationEvent,
			string(events.RecoverApplication):      app.handleRecoverApplicationEvent,
			string(events.RejectApplication):       app.handleRejectApplicationEvent,
			string(events.CompleteApplication):     app.handleCompleteApplicationEvent,
			string(events.FailApplication):         app.handleFailApplicationEvent,
			string(events.UpdateReservation):       app.onReservationStateChange,
			events.States().Application.Reserving:  app.onReserving,
			string(events.ReleaseAppAllocation):    app.handleReleaseAppAllocationEvent,
			string(events.ReleaseAppAllocationAsk): app.handleReleaseAppAllocationAskEvent,
			events.EnterState:                      app.enterState,
		},
	)

	return app
}

func (app *Application) handle(ev events.ApplicationEvent) error {
	// Locking mechanism:
	// 1) when handle event transitions, we first obtain the object's lock,
	//    this helps us to place a pre-check before entering here, in case
	//    we receive some invalidate events. If this introduces performance
	//    regression, a possible way to optimize is to use a separate lock
	//    to protect the transition phase.
	// 2) Note, state machine calls those callbacks here, we must ensure
	//    they are lock-free calls. Otherwise the callback will be blocked
	//    because the lock is already held here.
	app.lock.Lock()
	defer app.lock.Unlock()
	err := app.sm.Event(string(ev.GetEvent()), ev.GetArgs()...)
	// handle the same state transition not nil error (limit of fsm).
	if err != nil && err.Error() != "no transition" {
		return err
	}
	return nil
}

func (app *Application) canHandle(ev events.ApplicationEvent) bool {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.sm.Can(string(ev.GetEvent()))
}

func (app *Application) GetTask(taskID string) (interfaces.ManagedTask, error) {
	app.lock.RLock()
	defer app.lock.RUnlock()
	if task, ok := app.taskMap[taskID]; ok {
		return task, nil
	}
	return nil, fmt.Errorf("task %s doesn't exist in application %s",
		taskID, app.applicationID)
}

func (app *Application) GetApplicationID() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.applicationID
}

func (app *Application) GetQueue() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.queue
}

func (app *Application) GetUser() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.user
}

func (app *Application) setSchedulingPolicy(policy v1alpha1.SchedulingPolicy) {
	app.lock.Lock()
	defer app.lock.Unlock()
	app.schedulingPolicy = policy
}

func (app *Application) getSchedulingPolicy() v1alpha1.SchedulingPolicy {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.schedulingPolicy
}

func (app *Application) setTaskGroups(taskGroups []v1alpha1.TaskGroup) {
	app.lock.Lock()
	defer app.lock.Unlock()
	app.taskGroups = taskGroups
	for _, taskGroup := range app.taskGroups {
		app.placeholderAsk = common.Add(app.placeholderAsk, common.GetTGResource(taskGroup.MinResource, int64(taskGroup.MinMember)))
	}
}

func (app *Application) getPlaceholderAsk() *si.Resource {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.placeholderAsk
}

func (app *Application) getTaskGroups() []v1alpha1.TaskGroup {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.taskGroups
}

func (app *Application) setOwnReferences(ref []metav1.OwnerReference) {
	app.lock.RLock()
	defer app.lock.RUnlock()
	app.placeholderOwnerReferences = ref
}

func (app *Application) addTask(task *Task) {
	app.lock.Lock()
	defer app.lock.Unlock()
	if _, ok := app.taskMap[task.taskID]; ok {
		// skip adding duplicate task
		return
	}
	app.taskMap[task.taskID] = task
}

func (app *Application) removeTask(taskID string) error {
	app.lock.Lock()
	defer app.lock.Unlock()
	if _, ok := app.taskMap[taskID]; ok {
		delete(app.taskMap, taskID)
		log.Logger().Info("task removed",
			zap.String("appID", app.applicationID),
			zap.String("taskID", taskID))
		return nil
	}
	return fmt.Errorf("task %s is not found in application %s",
		taskID, app.applicationID)
}

func (app *Application) GetApplicationState() string {
	return app.sm.Current()
}

func (app *Application) GetPendingTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.Pending)
}

func (app *Application) GetNewTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.New)
}

func (app *Application) GetAllocatedTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.Allocated)
}

func (app *Application) getTasks(state string) []*Task {
	taskList := make([]*Task, 0)
	if len(app.taskMap) > 0 {
		for _, task := range app.taskMap {
			if task.GetTaskState() == state {
				taskList = append(taskList, task)
			}
		}
	}

	// sort the task based on creation time
	sort.Slice(taskList, func(i, j int) bool {
		l := taskList[i]
		r := taskList[j]
		return l.createTime.Before(r.createTime)
	})

	return taskList
}

func (app *Application) GetTags() map[string]string {
	return app.tags
}

func (app *Application) getNonTerminatedTaskAlias() []string {
	var nonTerminatedTaskAlias []string
	for _, task := range app.taskMap {
		if !task.isTerminated() {
			nonTerminatedTaskAlias = append(nonTerminatedTaskAlias, task.alias)
		}
	}
	return nonTerminatedTaskAlias
}

// SetState is only for testing
// this is just used for testing, it is not supposed to change state like this
func (app *Application) SetState(state string) {
	app.lock.Lock()
	defer app.lock.Unlock()
	app.sm.SetState(state)
}

func (app *Application) TriggerAppRecovery() error {
	return app.handle(NewSimpleApplicationEvent(app.applicationID, events.RecoverApplication))
}

// Schedule is called in every scheduling interval,
// we are not using dispatcher here because we want to
// make state transition in sync mode in order to prevent
// generating too many duplicate events. However, it must
// ensure non of these calls is expensive, usually, they
// do nothing more than just triggering the state transition.
func (app *Application) Schedule() {
	var states = events.States().Application
	switch app.GetApplicationState() {
	case states.New:
		ev := NewSubmitApplicationEvent(app.GetApplicationID())
		if err := app.handle(ev); err != nil {
			log.Logger().Warn("failed to handle SUBMIT app event",
				zap.Error(err))
		}
	case states.Accepted:
		// once the app is accepted by the scheduler core,
		// the next step is to send requests for scheduling
		// the app state could be transited to Reserving or Running
		// depends on if the app has gang members
		app.postAppAccepted()
	case states.Reserving:
		// during the Reserving state, only the placeholders
		// can be scheduled
		app.scheduleTasks(func(t *Task) bool {
			return t.placeholder
		})
	case states.Running:
		// during the Running state, only the regular pods
		// can be scheduled
		app.scheduleTasks(func(t *Task) bool {
			return !t.placeholder
		})
	default:
		log.Logger().Debug("skipping scheduling application",
			zap.String("appState", app.GetApplicationState()),
			zap.String("appID", app.GetApplicationID()),
			zap.String("appState", app.GetApplicationState()))
	}
}

func (app *Application) scheduleTasks(taskScheduleCondition func(t *Task) bool) {
	for _, task := range app.GetNewTasks() {
		if taskScheduleCondition(task) {
			// for each new task, we do a sanity check before moving the state to Pending_Schedule
			if err := task.sanityCheckBeforeScheduling(); err == nil {
				// note, if we directly trigger submit task event, it may spawn too many duplicate
				// events, because a task might be submitted multiple times before its state transits to PENDING.
				if handleErr := task.handle(
					NewSimpleTaskEvent(task.applicationID, task.taskID, events.InitTask)); handleErr != nil {
					// something goes wrong when transit task to PENDING state,
					// this should not happen because we already checked the state
					// before calling the transition. Nowhere to go, just log the error.
					log.Logger().Warn("init task failed", zap.Error(err))
				}
			} else {
				events.GetRecorder().Event(task.GetTaskPod(), v1.EventTypeWarning, "FailedScheduling", err.Error())
				log.Logger().Debug("task is not ready for scheduling",
					zap.String("appID", task.applicationID),
					zap.String("taskID", task.taskID),
					zap.Error(err))
			}
		}
	}
}

func (app *Application) handleSubmitApplicationEvent(event *fsm.Event) {
	log.Logger().Info("handle app submission",
		zap.String("app", app.String()),
		zap.String("clusterID", conf.GetSchedulerConf().ClusterID))
	err := app.schedulerAPI.Update(
		&si.UpdateRequest{
			NewApplications: []*si.AddApplicationRequest{
				{
					ApplicationID: app.applicationID,
					QueueName:     app.queue,
					PartitionName: app.partition,
					Ugi: &si.UserGroupInformation{
						User: app.user,
					},
					Tags:                         app.tags,
					PlaceholderAsk:               app.placeholderAsk,
					ExecutionTimeoutMilliSeconds: app.placeholderTimeoutInSec * 1000,
				},
			},
			RmID: conf.GetSchedulerConf().ClusterID,
		})

	if err != nil {
		// submission failed
		log.Logger().Warn("failed to submit app", zap.Error(err))
		dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID, err.Error()))
	}
}

func (app *Application) handleRecoverApplicationEvent(event *fsm.Event) {
	log.Logger().Info("handle app recovering",
		zap.String("app", app.String()),
		zap.String("clusterID", conf.GetSchedulerConf().ClusterID))
	err := app.schedulerAPI.Update(
		&si.UpdateRequest{
			NewApplications: []*si.AddApplicationRequest{
				{
					ApplicationID: app.applicationID,
					QueueName:     app.queue,
					PartitionName: app.partition,
					Ugi: &si.UserGroupInformation{
						User: app.user,
					},
					Tags:                         app.tags,
					ExecutionTimeoutMilliSeconds: app.placeholderTimeoutInSec * 1000,
				},
			},
			RmID: conf.GetSchedulerConf().ClusterID,
		})

	if err != nil {
		// submission failed
		log.Logger().Warn("failed to submit app", zap.Error(err))
		dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID, err.Error()))
	}
}

func (app *Application) postAppAccepted() {
	// if app has taskGroups defined, and it has no allocated tasks,
	// it goes to the Reserving state before getting to Running.
	// app could have allocated tasks upon a recovery, and in that case,
	// the reserving phase has already passed, no need to trigger that again.
	var ev events.SchedulingEvent
	log.Logger().Debug("postAppAccepted on cached app",
		zap.String("appID", app.applicationID),
		zap.Int("numTaskGroups", len(app.taskGroups)),
		zap.Int("numAllocatedTasks", len(app.getTasks(events.States().Task.Allocated))))
	if len(app.taskGroups) != 0 &&
		len(app.getTasks(events.States().Task.Allocated)) == 0 {
		ev = NewSimpleApplicationEvent(app.applicationID, events.TryReserve)
		log.Logger().Info("app has taskGroups defined, trying to reserve resources for gang members")
		dispatcher.Dispatch(ev)
	} else {
		ev = NewRunApplicationEvent(app.applicationID)
	}
	dispatcher.Dispatch(ev)
}

func (app *Application) onReserving(event *fsm.Event) {
	go func() {
		// while doing reserving
		if err := getPlaceholderManager().createAppPlaceholders(app); err != nil {
			// creating placeholder failed
			// put the app into recycling queue and turn the app to running state
			getPlaceholderManager().cleanUp(app)
			ev := NewRunApplicationEvent(app.applicationID)
			dispatcher.Dispatch(ev)
		}
	}()
}

func (app *Application) onReservationStateChange(event *fsm.Event) {
	// this event is called when there is a add or release of placeholders
	desireCounts := utils.NewTaskGroupInstanceCountMap()
	for _, tg := range app.taskGroups {
		desireCounts.Add(tg.Name, tg.MinMember)
	}

	actualCounts := utils.NewTaskGroupInstanceCountMap()
	for _, t := range app.getTasks(events.States().Task.Bound) {
		if t.placeholder {
			actualCounts.AddOne(t.taskGroupName)
		}
	}

	// min member all satisfied
	if desireCounts.Equals(actualCounts) {
		ev := NewRunApplicationEvent(app.applicationID)
		dispatcher.Dispatch(ev)
	}
}

func (app *Application) handleRejectApplicationEvent(event *fsm.Event) {
	log.Logger().Info("app is rejected by scheduler", zap.String("appID", app.applicationID))
	// for rejected apps, we directly move them to failed state
	dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID,
		fmt.Sprintf("application %s is rejected by scheduler", app.applicationID)))
}

func (app *Application) handleCompleteApplicationEvent(event *fsm.Event) {
	// TODO app lifecycle updates
	go func() {
		getPlaceholderManager().cleanUp(app)
	}()
}

func (app *Application) handleFailApplicationEvent(event *fsm.Event) {
	go func() {
		getPlaceholderManager().cleanUp(app)
	}()
	eventArgs := make([]string, 1)
	if err := events.GetEventArgsAsStrings(eventArgs, event.Args); err != nil {
		log.Logger().Error("fail to parse event arg", zap.Error(err))
		return
	}
	errMess := eventArgs[0]
	// unallocated task states include New, Pending and Scheduling
	unalloc := app.getTasks(events.States().Task.New)
	unalloc = append(unalloc, app.getTasks(events.States().Task.Pending)...)
	unalloc = append(unalloc, app.getTasks(events.States().Task.Scheduling)...)
	// publish pod level event to unallocated pods
	for _, task := range unalloc {
		events.GetRecorder().Eventf(task.GetTaskPod(), v1.EventTypeWarning, "ApplicationFailed",
			"Application %s scheduling failed, reason: %s", app.applicationID, errMess)
	}
}

func (app *Application) handleReleaseAppAllocationEvent(event *fsm.Event) {
	eventArgs := make([]string, 2)
	if err := events.GetEventArgsAsStrings(eventArgs, event.Args); err != nil {
		log.Logger().Error("fail to parse event arg", zap.Error(err))
		return
	}
	allocUUID := eventArgs[0]
	terminationTypeStr := eventArgs[1]
	log.Logger().Info("try to release pod from application",
		zap.String("appID", app.applicationID),
		zap.String("allocationUUID", allocUUID),
		zap.String("terminationType", terminationTypeStr))

	for _, task := range app.taskMap {
		if task.allocationUUID == allocUUID {
			task.setTaskTerminationType(terminationTypeStr)
			err := task.DeleteTaskPod(task.pod)
			if err != nil {
				log.Logger().Error("failed to release allocation from application", zap.Error(err))
			}
		}
	}
}

func (app *Application) handleReleaseAppAllocationAskEvent(event *fsm.Event) {
	eventArgs := make([]string, 2)
	if err := events.GetEventArgsAsStrings(eventArgs, event.Args); err != nil {
		log.Logger().Error("fail to parse event arg", zap.Error(err))
		return
	}
	taskID := eventArgs[0]
	terminationTypeStr := eventArgs[1]
	log.Logger().Info("try to release pod from application",
		zap.String("appID", app.applicationID),
		zap.String("taskID", taskID),
		zap.String("terminationType", terminationTypeStr))
	if task, ok := app.taskMap[taskID]; ok {
		task.setTaskTerminationType(terminationTypeStr)
		if task.IsPlaceholder() {
			err := task.DeleteTaskPod(task.pod)
			if err != nil {
				log.Logger().Error("failed to release allocation ask from application", zap.Error(err))
			}
		} else {
			log.Logger().Warn("skip to release allocation ask, ask is not a placeholder",
				zap.String("appID", app.applicationID),
				zap.String("taskID", taskID))
		}
	} else {
		log.Logger().Warn("task not found",
			zap.String("appID", app.applicationID),
			zap.String("taskID", taskID))
	}
}

func (app *Application) enterState(event *fsm.Event) {
	log.Logger().Debug("shim app state transition",
		zap.String("app", app.applicationID),
		zap.String("source", event.Src),
		zap.String("destination", event.Dst),
		zap.String("event", event.Event))
}

func (app *Application) SetPlaceholderTimeout(timeout int64) {
	app.lock.Lock()
	defer app.lock.Unlock()
	app.placeholderTimeoutInSec = timeout
}
