// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"context"
	"testing"

	"github.com/pingcap/tidb/dist-task/proto"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/testkit/testsetup"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	testsetup.SetupForCommonTest()
	opts := []goleak.Option{
		goleak.IgnoreTopFunction("github.com/golang/glog.(*loggingT).flushDaemon"),
		goleak.IgnoreTopFunction("github.com/lestrrat-go/httprc.runFetchWorker"),
		goleak.IgnoreTopFunction("go.etcd.io/etcd/client/pkg/v3/logutil.(*MergeLogger).outputLoop"),
	}
	goleak.VerifyTestMain(m, opts...)
}

func TestGlobalTaskTable(t *testing.T) {
	store := testkit.CreateMockStore(t)

	tk := testkit.NewTestKit(t, store)

	gm := NewGlobalTaskManager(context.Background(), tk.Session())

	SetGlobalTaskManager(gm)
	gm, err := GetGlobalTaskManager()
	require.NoError(t, err)

	id, err := gm.AddNewTask("test", 4, (&proto.SimpleNumberGTaskMeta{}).Serialize())
	require.NoError(t, err)
	require.Equal(t, int64(1), id)

	task, err := gm.GetNewTask()
	require.NoError(t, err)
	require.Equal(t, int64(1), task.ID)
	require.Equal(t, "test", task.Type)
	require.Equal(t, proto.TaskStatePending, task.State)
	require.Equal(t, uint64(4), task.Concurrency)
	require.Equal(t, &proto.SimpleNumberGTaskMeta{}, task.Meta)

	task2, err := gm.GetTaskByID(1)
	require.NoError(t, err)
	require.Equal(t, task, task2)

	task3, err := gm.GetTasksInStates(proto.TaskStatePending)
	require.NoError(t, err)
	require.Len(t, task3, 1)
	require.Equal(t, task, task3[0])

	task4, err := gm.GetTasksInStates(proto.TaskStatePending, proto.TaskStateRunning)
	require.NoError(t, err)
	require.Len(t, task4, 1)
	require.Equal(t, task, task4[0])

	task.State = proto.TaskStateRunning
	err = gm.UpdateTask(task)
	require.NoError(t, err)

	task5, err := gm.GetTasksInStates(proto.TaskStateRunning)
	require.NoError(t, err)
	require.Len(t, task5, 1)
	require.Equal(t, task, task5[0])
}

func TestSubTaskTable(t *testing.T) {
	store := testkit.CreateMockStore(t)

	tk := testkit.NewTestKit(t, store)

	sm := NewSubTaskManager(context.Background(), tk.Session())

	SetSubTaskManager(sm)
	sm, err := GetSubTaskManager()
	require.NoError(t, err)

	err = sm.AddNewTask(1, "tidb1", (&proto.SimpleNumberSTaskMeta{}).Serialize(), "test", false)
	require.NoError(t, err)

	nilTask, err := sm.GetSubtaskInStates("tidb2", 1, proto.TaskStatePending)
	require.NoError(t, err)
	require.Nil(t, nilTask)

	task, err := sm.GetSubtaskInStates("tidb1", 1, proto.TaskStatePending)
	require.NoError(t, err)
	require.Equal(t, "test", task.Type)
	require.Equal(t, int64(1), task.TaskID)
	require.Equal(t, proto.TaskStatePending, task.State)
	require.Equal(t, "tidb1", task.SchedulerID)
	require.Equal(t, &proto.SimpleNumberSTaskMeta{}, task.Meta)
}
