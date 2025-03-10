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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apis "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/apache/incubator-yunikorn-core/pkg/api"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/apis/yunikorn.apache.org/v1alpha1"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/client"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/constants"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/events"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/utils"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/conf"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/dispatcher"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

type recorderTime struct {
	time int64
	lock *sync.RWMutex
}

func TestNewApplication(t *testing.T) {
	app := NewApplication("app00001", "root.queue", "testuser", map[string]string{}, newMockSchedulerAPI())
	assert.Equal(t, app.GetApplicationID(), "app00001")
	assert.Equal(t, app.GetApplicationState(), events.States().Application.New)
	assert.Equal(t, app.partition, constants.DefaultPartition)
	assert.Equal(t, len(app.taskMap), 0)
	assert.Equal(t, app.GetApplicationState(), events.States().Application.New)
	assert.Equal(t, app.queue, "root.queue")
}

func TestSubmitApplication(t *testing.T) {
	app := NewApplication("app00001", "root.abc", "testuser", map[string]string{}, newMockSchedulerAPI())

	err := app.handle(NewSubmitApplicationEvent(app.applicationID))
	assert.NilError(t, err)
	assertAppState(t, app, events.States().Application.Submitted, 10*time.Second)

	// app already submitted
	err = app.handle(NewSubmitApplicationEvent(app.applicationID))
	if err == nil {
		// this should give an error
		t.Error("expecting error got 'nil'")
	}
	assertAppState(t, app, events.States().Application.Submitted, 10*time.Second)
}

func TestRunApplication(t *testing.T) {
	ms := &mockSchedulerAPI{}
	ms.updateFn = func(request *si.UpdateRequest) error {
		assert.Equal(t, len(request.NewApplications), 1)
		assert.Equal(t, request.NewApplications[0].ApplicationID, "app00001")
		assert.Equal(t, request.NewApplications[0].QueueName, "root.abc")
		return nil
	}

	app := NewApplication("app00001", "root.abc", "testuser", map[string]string{}, ms)

	// app must be submitted before being able to run
	err := app.handle(NewRunApplicationEvent(app.applicationID))
	if err == nil {
		// this should give an error
		t.Error("expecting error got 'nil'")
	}
	assertAppState(t, app, events.States().Application.New, 3*time.Second)

	// submit the app
	err = app.handle(NewSubmitApplicationEvent(app.applicationID))
	assert.NilError(t, err)
	assertAppState(t, app, events.States().Application.Submitted, 3*time.Second)

	// app must be accepted first
	err = app.handle(NewRunApplicationEvent(app.applicationID))
	if err == nil {
		// this should give an error
		t.Error("expecting error got 'nil'")
	}
	assertAppState(t, app, events.States().Application.Submitted, 3*time.Second)
}

func TestFailApplication(t *testing.T) {
	context := initContextForTest()
	dispatcher.RegisterEventHandler(dispatcher.EventTypeApp, context.ApplicationEventHandler())
	dispatcher.Start()
	defer dispatcher.Stop()

	// inject the mocked clients to the placeholder manager
	createdPods := newThreadSafePodsMap()
	mockedAPIProvider := client.NewMockedAPIProvider()
	mockedAPIProvider.MockCreateFn(func(pod *v1.Pod) (*v1.Pod, error) {
		createdPods.add(pod)
		return pod, nil
	})
	mgr := NewPlaceholderManager(mockedAPIProvider.GetAPIs())
	mgr.Start()
	defer mgr.Stop()

	rt := &recorderTime{
		time: int64(0),
		lock: &sync.RWMutex{},
	}
	ms := &mockSchedulerAPI{}
	// set test mode
	conf.GetSchedulerConf().SetTestMode(true)
	// set Recorder to mocked type
	mr := events.NewMockedRecorder()
	mr.OnEventf = func() {
		rt.lock.Lock()
		defer rt.lock.Unlock()
		rt.time++
	}
	events.SetRecorderForTest(mr)
	resources := make(map[v1.ResourceName]resource.Quantity)
	containers := make([]v1.Container, 0)
	containers = append(containers, v1.Container{
		Name: "container-01",
		Resources: v1.ResourceRequirements{
			Requests: resources,
		},
	})
	pod := &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "pod-test-00001",
			UID:  "UID-00001",
		},
		Spec: v1.PodSpec{
			Containers: containers,
		},
	}
	appID := "app-test-001"
	app := NewApplication(appID, "root.abc", "testuser", map[string]string{}, ms)
	task1 := NewTask("task01", app, context, pod)
	task2 := NewTask("task02", app, context, pod)
	task3 := NewTask("task03", app, context, pod)
	task4 := NewTask("task04", app, context, pod)
	// set task states to new/pending/scheduling/running
	task1.sm.SetState(events.States().Task.New)
	task2.sm.SetState(events.States().Task.Pending)
	task3.sm.SetState(events.States().Task.Scheduling)
	task4.sm.SetState(events.States().Task.Allocated)
	app.addTask(task1)
	app.addTask(task2)
	app.addTask(task3)
	app.addTask(task4)
	app.SetState(events.States().Application.Accepted)
	errMess := "Test Error Message"
	err := app.handle(NewFailApplicationEvent(app.applicationID, errMess))
	assert.NilError(t, err)
	assertAppState(t, app, events.States().Application.Failed, 3*time.Second)
	assert.Equal(t, rt.time, int64(3))
	// reset time to 0
	rt.time = 0
	appID2 := "app-test-002"
	app2 := NewApplication(appID2, "root.abc", "testuser", map[string]string{}, ms)
	app2.SetState(events.States().Application.New)
	err = app2.handle(NewFailApplicationEvent(app2.applicationID, errMess))
	if err == nil {
		t.Error("expecting error got 'nil'")
	}
	assertAppState(t, app2, events.States().Application.New, 3*time.Second)
	app2.SetState(events.States().Application.Submitted)
	err = app2.handle(NewFailApplicationEvent(app2.applicationID, errMess))
	assert.NilError(t, err)
	assertAppState(t, app2, events.States().Application.Failed, 3*time.Second)
	assert.Equal(t, rt.time, int64(0))
	// Test over, set Recorder back fake type
	events.SetRecorderForTest(record.NewFakeRecorder(1024))
}

func TestReleaseAppAllocation(t *testing.T) {
	context := initContextForTest()
	ms := &mockSchedulerAPI{}
	resources := make(map[v1.ResourceName]resource.Quantity)
	containers := make([]v1.Container, 0)
	containers = append(containers, v1.Container{
		Name: "container-01",
		Resources: v1.ResourceRequirements{
			Requests: resources,
		},
	})
	pod := &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "pod-test-00001",
			UID:  "UID-00001",
		},
		Spec: v1.PodSpec{
			Containers: containers,
		},
	}
	appID := "app-test-001"
	UUID := "testUUID001"
	app := NewApplication(appID, "root.abc", "testuser", map[string]string{}, ms)
	task := NewTask("task01", app, context, pod)
	app.addTask(task)
	task.allocationUUID = UUID
	// app must be running states
	err := app.handle(NewReleaseAppAllocationEvent(appID, si.TerminationType_TIMEOUT, UUID))
	if err == nil {
		// this should give an error
		t.Error("expecting error got 'nil'")
	}
	// set app states to running, let event can be trigger
	app.SetState(events.States().Application.Running)
	assertAppState(t, app, events.States().Application.Running, 3*time.Second)
	err = app.handle(NewReleaseAppAllocationEvent(appID, si.TerminationType_TIMEOUT, UUID))
	assert.NilError(t, err)
	// after handle release event the states of app must be running
	assertAppState(t, app, events.States().Application.Running, 3*time.Second)
}

func newMockSchedulerAPI() *mockSchedulerAPI {
	return &mockSchedulerAPI{
		registerFn: func(request *si.RegisterResourceManagerRequest, callback api.ResourceManagerCallback) (response *si.RegisterResourceManagerResponse, e error) {
			return nil, nil
		},
		updateFn: func(request *si.UpdateRequest) error {
			return nil
		},
	}
}

type mockSchedulerAPI struct {
	callback   api.ResourceManagerCallback //nolint:structcheck,unused
	registerFn func(request *si.RegisterResourceManagerRequest,
		callback api.ResourceManagerCallback) (*si.RegisterResourceManagerResponse, error)
	updateFn func(request *si.UpdateRequest) error
}

func (ms *mockSchedulerAPI) RegisterResourceManager(request *si.RegisterResourceManagerRequest,
	callback api.ResourceManagerCallback) (*si.RegisterResourceManagerResponse, error) {
	return ms.registerFn(request, callback)
}

func (ms *mockSchedulerAPI) Update(request *si.UpdateRequest) error {
	return ms.updateFn(request)
}

func (ms *mockSchedulerAPI) ReloadConfiguration(rmID string) error {
	return nil
}

func assertAppState(t *testing.T, app *Application, expectedState string, duration time.Duration) {
	deadline := time.Now().Add(duration)
	for {
		if app.sm.Current() == expectedState {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for app %s reach to state %s", app.applicationID, expectedState)
		}
	}
}

func TestGetNonTerminatedTaskAlias(t *testing.T) {
	context := initContextForTest()
	appID := "app00001"
	app := NewApplication(appID, "root.a", "testuser", map[string]string{}, newMockSchedulerAPI())
	context.applications[appID] = app
	// app doesn't have any task
	res := app.getNonTerminatedTaskAlias()
	assert.Equal(t, len(res), 0)

	pod1 := &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "test-00001",
			UID:  "UID-00001",
		},
	}
	pod2 := &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "test-00002",
			UID:  "UID-00002",
		},
	}
	// set two task to non-terminated states
	taskID1 := "task01"
	task1 := NewTask(taskID1, app, context, pod1)
	app.taskMap[taskID1] = task1
	task1.sm.SetState(events.States().Task.Pending)
	taskID2 := "task02"
	task2 := NewTask(taskID2, app, context, pod2)
	app.taskMap[taskID2] = task2
	task2.sm.SetState(events.States().Task.Pending)
	// check the tasks both in non-terminated states
	// res should return both task's alias
	res = app.getNonTerminatedTaskAlias()
	assert.Equal(t, len(res), 2)
	assert.Assert(t, is.Contains(res, "/test-00001"))
	assert.Assert(t, is.Contains(res, "/test-00002"))

	// set two tasks to terminated states
	task1.sm.SetState(events.States().Task.Rejected)
	task2.sm.SetState(events.States().Task.Rejected)
	// check the tasks both in terminated states
	// res should retuen empty
	res = app.getNonTerminatedTaskAlias()
	assert.Equal(t, len(res), 0)

	// set two tasks to one is terminated, another is non-terminated
	task1.sm.SetState(events.States().Task.Rejected)
	task2.sm.SetState(events.States().Task.Allocated)
	// check the task, should only return task2's alias
	res = app.getNonTerminatedTaskAlias()
	assert.Equal(t, len(res), 1)
	assert.Equal(t, res[0], "/test-00002")
}

func TestSetTaskGroupsAndSchedulingPolicy(t *testing.T) {
	app := NewApplication("app01", "root.a", "test-user", map[string]string{}, newMockSchedulerAPI())
	assert.Assert(t, app.getSchedulingPolicy().Type == "")
	assert.Equal(t, len(app.getTaskGroups()), 0)

	app.setSchedulingPolicy(v1alpha1.SchedulingPolicy{
		Type: v1alpha1.TryReserve,
		Parameters: map[string]string{
			"option-1": "value-1",
			"option-2": "value-2",
		},
	})

	assert.Equal(t, app.getSchedulingPolicy().Type, v1alpha1.TryReserve)
	assert.Equal(t, len(app.getSchedulingPolicy().Parameters), 2)
	assert.Equal(t, app.getSchedulingPolicy().Parameters["option-1"], "value-1", "incorrect parameter value")
	assert.Equal(t, app.getSchedulingPolicy().Parameters["option-2"], "value-2", "incorrect parameter value")

	duration := int64(3000)
	app.setTaskGroups([]v1alpha1.TaskGroup{
		{
			Name:      "test-group-1",
			MinMember: 10,
			MinResource: map[string]resource.Quantity{
				v1.ResourceCPU.String():    resource.MustParse("500m"),
				v1.ResourceMemory.String(): resource.MustParse("500Mi"),
			},
		},
		{
			Name:      "test-group-2",
			MinMember: 20,
			MinResource: map[string]resource.Quantity{
				v1.ResourceCPU.String():    resource.MustParse("1000m"),
				v1.ResourceMemory.String(): resource.MustParse("1000Mi"),
			},
			NodeSelector: map[string]string{
				"locate": "west",
			},
			Tolerations: []v1.Toleration{
				{
					Key:               "nodeType",
					Operator:          v1.TolerationOpEqual,
					Value:             "infra",
					Effect:            v1.TaintEffectNoSchedule,
					TolerationSeconds: &duration,
				},
			},
		},
	})

	assert.Assert(t, app.getTaskGroups() != nil)
	assert.Equal(t, len(app.getTaskGroups()), 2)

	// sort the slice to give us a stable order
	sort.Slice(app.getTaskGroups(), func(i, j int) bool {
		return strings.Compare(app.getTaskGroups()[i].Name, app.getTaskGroups()[j].Name) < 0
	})

	tg1 := app.getTaskGroups()[0]
	assert.Equal(t, tg1.Name, "test-group-1")
	assert.Equal(t, tg1.MinMember, int32(10))
	assert.Equal(t, tg1.MinResource[v1.ResourceCPU.String()], resource.MustParse("500m"))
	assert.Equal(t, tg1.MinResource[v1.ResourceMemory.String()], resource.MustParse("500Mi"))

	tg2 := app.getTaskGroups()[1]
	assert.Equal(t, tg2.Name, "test-group-2")
	assert.Equal(t, tg2.MinMember, int32(20))
	assert.Equal(t, len(tg2.Tolerations), 1)
	assert.Equal(t, tg2.Tolerations[0].Key, "nodeType")
	assert.Equal(t, tg2.Tolerations[0].Value, "infra")
	assert.Equal(t, tg2.Tolerations[0].Operator, v1.TolerationOpEqual)
	assert.Equal(t, tg2.Tolerations[0].Effect, v1.TaintEffectNoSchedule)
	assert.Equal(t, tg2.Tolerations[0].TolerationSeconds, &duration)

	// This is a weird calculation for memory as the request is rounded up before multiplying.
	// TG1: 500Mi, 10 members: each member 525M -> total 5250M
	// TG2: 1000Mi, 20 members: each member 1049M -> total 20980
	// overall usage 5250M + 20980M -> 26230M. This will also be the queue usage so the correct handling
	// CPU is normal as it specifies milli cpu to start with
	expectedPlaceholderAsk := common.NewResourceBuilder().AddResource(constants.Memory, 26230).AddResource(constants.CPU, 25000).Build()
	actualPlaceholderAsk := app.getPlaceholderAsk()
	assert.DeepEqual(t, actualPlaceholderAsk, expectedPlaceholderAsk)
}

type threadSafePodsMap struct {
	pods map[string]*v1.Pod
	sync.RWMutex
}

func newThreadSafePodsMap() *threadSafePodsMap {
	return &threadSafePodsMap{
		pods: make(map[string]*v1.Pod),
	}
}

func (t *threadSafePodsMap) add(pod *v1.Pod) {
	t.Lock()
	defer t.Unlock()
	t.pods[pod.Name] = pod
}

func (t *threadSafePodsMap) count() int {
	t.RLock()
	defer t.RUnlock()
	return len(t.pods)
}

func TestTryReserve(t *testing.T) {
	context := initContextForTest()
	dispatcher.RegisterEventHandler(dispatcher.EventTypeApp, context.ApplicationEventHandler())
	dispatcher.Start()
	defer dispatcher.Stop()

	// inject the mocked clients to the placeholder manager
	createdPods := newThreadSafePodsMap()
	mockedAPIProvider := client.NewMockedAPIProvider()
	mockedAPIProvider.MockCreateFn(func(pod *v1.Pod) (*v1.Pod, error) {
		createdPods.add(pod)
		return pod, nil
	})
	mgr := NewPlaceholderManager(mockedAPIProvider.GetAPIs())
	mgr.Start()
	defer mgr.Stop()

	// create a new app
	app := NewApplication("app00001", "root.abc", "test-user",
		map[string]string{}, mockedAPIProvider.GetAPIs().SchedulerAPI)
	context.applications[app.applicationID] = app

	// set app scheduling policy
	app.setSchedulingPolicy(v1alpha1.SchedulingPolicy{
		Type: v1alpha1.TryReserve,
		Parameters: map[string]string{
			"option-1": "value-1",
			"option-2": "value-2",
		},
	})

	// set taskGroups
	app.setTaskGroups([]v1alpha1.TaskGroup{
		{
			Name:      "test-group-1",
			MinMember: 10,
			MinResource: map[string]resource.Quantity{
				v1.ResourceCPU.String():    resource.MustParse("500m"),
				v1.ResourceMemory.String(): resource.MustParse("500Mi"),
			},
		},
		{
			Name:      "test-group-2",
			MinMember: 20,
			MinResource: map[string]resource.Quantity{
				v1.ResourceCPU.String():    resource.MustParse("1000m"),
				v1.ResourceMemory.String(): resource.MustParse("1000Mi"),
			},
		},
	})

	// submit the app
	err := app.handle(NewSubmitApplicationEvent(app.applicationID))
	assert.NilError(t, err)
	assertAppState(t, app, events.States().Application.Submitted, 3*time.Second)

	// accepted the app
	err = app.handle(NewSimpleApplicationEvent(app.GetApplicationID(), events.AcceptApplication))
	assert.NilError(t, err)

	// run app schedule
	app.Schedule()

	// since this app has taskGroups defined,
	// once the app is accepted, it is expected to see this app goes to Reserving state
	assertAppState(t, app, events.States().Application.Reserving, 3*time.Second)

	// under Reserving state, the app will need to acquire all the placeholders it asks for
	err = utils.WaitForCondition(func() bool {
		return createdPods.count() == 30
	}, 100*time.Millisecond, 3*time.Second)
	assert.NilError(t, err, "placeholders are not created")
}

func TestTryReservePostRestart(t *testing.T) {
	context := initContextForTest()
	dispatcher.RegisterEventHandler(dispatcher.EventTypeApp, context.ApplicationEventHandler())
	dispatcher.Start()
	defer dispatcher.Stop()

	// inject the mocked clients to the placeholder manager
	createdPods := newThreadSafePodsMap()
	mockedAPIProvider := client.NewMockedAPIProvider()
	mockedAPIProvider.MockCreateFn(func(pod *v1.Pod) (*v1.Pod, error) {
		createdPods.add(pod)
		return pod, nil
	})
	mgr := NewPlaceholderManager(mockedAPIProvider.GetAPIs())
	mgr.Start()
	defer mgr.Stop()

	// create a new app
	app := NewApplication("app00001", "root.abc", "test-user",
		map[string]string{}, mockedAPIProvider.GetAPIs().SchedulerAPI)
	context.applications[app.applicationID] = app

	// set taskGroups
	app.setTaskGroups([]v1alpha1.TaskGroup{
		{
			Name:      "test-group-1",
			MinMember: 10,
			MinResource: map[string]resource.Quantity{
				v1.ResourceCPU.String():    resource.MustParse("500m"),
				v1.ResourceMemory.String(): resource.MustParse("500Mi"),
			},
		},
		{
			Name:      "test-group-2",
			MinMember: 20,
			MinResource: map[string]resource.Quantity{
				v1.ResourceCPU.String():    resource.MustParse("1000m"),
				v1.ResourceMemory.String(): resource.MustParse("1000Mi"),
			},
		},
	})

	// submit the app
	err := app.handle(NewSubmitApplicationEvent(app.applicationID))
	assert.NilError(t, err)
	assertAppState(t, app, events.States().Application.Submitted, 3*time.Second)

	// accepted the app
	err = app.handle(NewSimpleApplicationEvent(app.GetApplicationID(), events.AcceptApplication))
	assert.NilError(t, err)

	// simulate some tasks are recovered during the restart
	// create 3 pods, 1 of them is Allocated and the other 2 are New
	resources := make(map[v1.ResourceName]resource.Quantity)
	containers := make([]v1.Container, 0)
	containers = append(containers, v1.Container{
		Name: "container-01",
		Resources: v1.ResourceRequirements{
			Requests: resources,
		},
	})
	task0 := NewTask("task00", app, context, &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "pod-test-00000",
			UID:  "UID-00000",
		},
		Spec: v1.PodSpec{
			Containers: containers,
		},
	})
	task1 := NewTask("task01", app, context, &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "pod-test-00001",
			UID:  "UID-00001",
		},
		Spec: v1.PodSpec{
			Containers: containers,
		},
	})
	task2 := NewTask("task02", app, context, &v1.Pod{
		TypeMeta: apis.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: apis.ObjectMeta{
			Name: "pod-test-00002",
			UID:  "UID-00002",
		},
		Spec: v1.PodSpec{
			Containers: containers,
		},
	})
	task0.setAllocated("fake-host", string(task0.pod.UID))
	app.addTask(task0)
	app.addTask(task1)
	app.addTask(task2)

	// there should be 1 Allocated task, i.e task0
	// there should be 2 New tasks, i.e task1 and task2
	assert.Equal(t, len(app.getTasks(events.States().Task.Allocated)), 1)
	assert.Equal(t, len(app.getTasks(events.States().Task.New)), 2)

	// run app schedule
	app.Schedule()

	// since this app has Allocated tasks, the Reserving state will be skipped
	assertAppState(t, app, events.States().Application.Running, 3*time.Second)

	// verify there will be no placeholders created
	time.Sleep(time.Second)
	assert.Equal(t, createdPods.count(), 0)
}

func TestTriggerAppRecovery(t *testing.T) {
	// Trigger app recovery should be successful if the app is in New state
	app := NewApplication("app00001", "root.abc", "test-user",
		map[string]string{}, newMockSchedulerAPI())
	err := app.TriggerAppRecovery()
	assert.NilError(t, err)
	assert.Equal(t, app.GetApplicationState(), events.States().Application.Recovering)

	// Trigger app recovery should be failed if the app already leaves New state
	app = NewApplication("app00001", "root.abc", "test-user",
		map[string]string{}, newMockSchedulerAPI())
	err = app.handle(NewSubmitApplicationEvent(app.applicationID))
	assert.NilError(t, err)
	assertAppState(t, app, events.States().Application.Submitted, 3*time.Second)
	err = app.TriggerAppRecovery()
	assert.ErrorContains(t, err, "event RecoverApplication inappropriate in current state Submitted")
}
