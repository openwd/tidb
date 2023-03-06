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

package dispatcher

import (
	"github.com/pingcap/tidb/distribute_framework/proto"
)

// GTaskFlowHandle is used to control the process operations for each global task.
type GTaskFlowHandle interface {
	Progress(d Dispatch, gTask *proto.Task, fromPending bool) (finished bool, subtasks []*proto.Subtask, err error)
	HandleError(d Dispatch, gTask *proto.Task, receive string) (meta proto.SubTaskMeta, err error)
}

var taskDispatcherHandleMap = make(map[string]GTaskFlowHandle)

// RegisterGTaskFlowHandle is used to register the handle.
func RegisterGTaskFlowHandle(taskType string, dispatcherHandle GTaskFlowHandle) {
	taskDispatcherHandleMap[taskType] = dispatcherHandle
}

// GetGTaskFlowHandle is used to get the handle.
func GetGTaskFlowHandle(taskType string) GTaskFlowHandle {
	return taskDispatcherHandleMap[taskType]
}

var MockTiDBId []string
