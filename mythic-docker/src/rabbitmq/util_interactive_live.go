package rabbitmq

import (
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/its-a-feature/Mythic/database"
	"github.com/its-a-feature/Mythic/database/enums/InteractiveTask"
	databaseStructs "github.com/its-a-feature/Mythic/database/structs"
	"github.com/its-a-feature/Mythic/logging"
)

// Interactive output goes to anyone watching the task as soon as it arrives from the agent, and is saved to the
// database in batches so the live view never waits on it. A batch is saved once it's this old or this big.
const (
	interactiveLiveSaveDelay = 100 * time.Millisecond
	interactiveLiveSaveSize  = 64 * 1024
	interactiveLiveIdleTime  = 10 * time.Minute
)

// InteractiveLiveViewer receives a task's output as it arrives. Calls are made in order with the task locked, so
// they must not block. Returning false stops the viewer watching the task.
type InteractiveLiveViewer interface {
	InteractiveOutput(messageType InteractiveTask.MessageType, data []byte) bool
	// InteractiveSaved means all output sent before it is now saved, ending with the response responseID
	InteractiveSaved(responseID int) bool
}

type interactiveLiveOutput struct {
	at   time.Time
	data []byte
}

type interactiveLiveTask struct {
	sync.Mutex
	task        databaseStructs.Task
	pending     []interactiveLiveOutput
	pendingType InteractiveTask.MessageType
	pendingSize int
	saveTimer   *time.Timer
	viewers     map[InteractiveLiveViewer]bool
	lastActive  time.Time
}

var interactiveLiveTasks = struct {
	sync.Mutex
	tasks        map[int]*interactiveLiveTask
	agentTaskIDs map[string]int
}{tasks: make(map[int]*interactiveLiveTask), agentTaskIDs: make(map[string]int)}

// replaced in tests
var insertInteractiveLiveResponse = func(response *databaseStructs.Response) error {
	statement, err := database.DB.PrepareNamed(`INSERT INTO response
		("timestamp", task_id, response, is_error, interactive_task_type, operation_id, eventstepinstance_id, apitokens_id)
		VALUES (:timestamp, :task_id, :response, :is_error, :interactive_task_type, :operation_id, :eventstepinstance_id, :apitokens_id)
		RETURNING id`)
	if err != nil {
		return err
	}
	defer statement.Close()
	if err = statement.Get(&response.ID, response); err != nil {
		return err
	}
	go emitResponseLog(response.ID)
	return nil
}
var trackInteractiveLiveScreen = trackInteractiveScreen

func getInteractiveLiveTask(taskID int, agentTaskID string) (*interactiveLiveTask, error) {
	interactiveLiveTasks.Lock()
	defer interactiveLiveTasks.Unlock()
	if agentTaskID != "" {
		if id, ok := interactiveLiveTasks.agentTaskIDs[agentTaskID]; ok {
			taskID = id
		}
	}
	if liveTask, ok := interactiveLiveTasks.tasks[taskID]; ok {
		return liveTask, nil
	}
	task := databaseStructs.Task{}
	query := `SELECT id, agent_task_id, operation_id, eventstepinstance_id, apitokens_id FROM task WHERE id=$1`
	var lookup interface{} = taskID
	if agentTaskID != "" {
		query = `SELECT id, agent_task_id, operation_id, eventstepinstance_id, apitokens_id FROM task WHERE agent_task_id=$1`
		lookup = agentTaskID
	}
	if err := database.DB.Get(&task, query, lookup); err != nil {
		return nil, err
	}
	liveTask := &interactiveLiveTask{task: task, viewers: make(map[InteractiveLiveViewer]bool), lastActive: time.Now()}
	interactiveLiveTasks.tasks[task.ID] = liveTask
	interactiveLiveTasks.agentTaskIDs[task.AgentTaskID] = task.ID
	return liveTask, nil
}

func handleAgentMessagePostResponseInteractiveOutput(agentResponses *[]agentMessagePostResponseInteractive, operationID int) {
	for _, agentResponse := range *agentResponses {
		if len(agentResponse.Data) == 0 {
			continue
		}
		liveTask, err := getInteractiveLiveTask(0, agentResponse.TaskUUID)
		if err != nil || liveTask.task.OperationID != operationID {
			logging.LogError(err, "Failed to find task")
			continue
		}
		output, err := base64.StdEncoding.DecodeString(agentResponse.Data)
		if err != nil {
			logging.LogError(err, "Failed to decode interactive task output")
			continue
		}
		liveTask.addOutput(agentResponse.MessageType, output)
	}
}

func (liveTask *interactiveLiveTask) addOutput(messageType InteractiveTask.MessageType, output []byte) {
	liveTask.Lock()
	defer liveTask.Unlock()
	liveTask.lastActive = time.Now()
	// each response has a single type, so output of another type starts a new one
	if len(liveTask.pending) > 0 && liveTask.pendingType != messageType {
		liveTask.save()
	}
	for viewer := range liveTask.viewers {
		if !viewer.InteractiveOutput(messageType, output) {
			delete(liveTask.viewers, viewer)
		}
	}
	liveTask.pending = append(liveTask.pending, interactiveLiveOutput{at: time.Now().UTC(), data: output})
	liveTask.pendingType = messageType
	liveTask.pendingSize += len(output)
	if liveTask.pendingSize >= interactiveLiveSaveSize ||
		(messageType != InteractiveTask.Output && messageType != InteractiveTask.Error) {
		liveTask.save()
	} else if liveTask.saveTimer == nil {
		liveTask.saveTimer = time.AfterFunc(interactiveLiveSaveDelay, func() {
			liveTask.Lock()
			defer liveTask.Unlock()
			liveTask.save()
		})
	}
}

// save writes the pending output as one response. The task must be locked.
func (liveTask *interactiveLiveTask) save() {
	if liveTask.saveTimer != nil {
		liveTask.saveTimer.Stop()
		liveTask.saveTimer = nil
	}
	if len(liveTask.pending) == 0 {
		return
	}
	pending := liveTask.pending
	liveTask.pending = nil
	liveTask.pendingSize = 0
	data := make([]byte, 0, len(pending[0].data))
	for _, output := range pending {
		data = append(data, output.data...)
	}
	response := databaseStructs.Response{
		Timestamp:   pending[0].at,
		TaskID:      liveTask.task.ID,
		IsError:     InteractiveTask.IsError(liveTask.pendingType),
		Response:    data,
		OperationID: liveTask.task.OperationID,
	}
	response.InteractiveTaskType.Valid = true
	response.InteractiveTaskType.Int64 = int64(liveTask.pendingType)
	response.EventStepInstanceID = liveTask.task.EventStepInstanceID
	response.APITokensID = liveTask.task.APITokensID
	if err := insertInteractiveLiveResponse(&response); err != nil {
		logging.LogError(err, "Failed to insert new interactive task output", "task_id", response.TaskID)
		// response id 0 tells viewers the output since the last save won't be in the database
		response.ID = 0
	} else if liveTask.pendingType == InteractiveTask.Output || liveTask.pendingType == InteractiveTask.Error {
		// recordings keep the time each piece of output arrived rather than when it was saved
		for _, output := range pending {
			trackInteractiveLiveScreen(liveTask.task, response.ID, output.data, output.at)
		}
	}
	for viewer := range liveTask.viewers {
		if !viewer.InteractiveSaved(response.ID) {
			delete(liveTask.viewers, viewer)
		}
	}
}

// WatchInteractiveTask saves the task's pending output, then sends the viewer everything after it. It returns the
// id of the task's latest response so the viewer can load what came before from the database.
func WatchInteractiveTask(taskID int, operationID int, viewer InteractiveLiveViewer) (int, error) {
	liveTask, err := getInteractiveLiveTask(taskID, "")
	if err != nil {
		return 0, err
	}
	liveTask.Lock()
	defer liveTask.Unlock()
	if liveTask.task.OperationID != operationID {
		return 0, errors.New("task isn't in the current operation")
	}
	liveTask.save()
	latestResponseID := 0
	if err = database.DB.Get(&latestResponseID, `SELECT COALESCE(max(id), 0) FROM response WHERE task_id=$1`,
		taskID); err != nil {
		return 0, err
	}
	liveTask.viewers[viewer] = true
	liveTask.lastActive = time.Now()
	return latestResponseID, nil
}

func UnwatchInteractiveTask(taskID int, viewer InteractiveLiveViewer) {
	interactiveLiveTasks.Lock()
	liveTask, ok := interactiveLiveTasks.tasks[taskID]
	interactiveLiveTasks.Unlock()
	if !ok {
		return
	}
	liveTask.Lock()
	delete(liveTask.viewers, viewer)
	liveTask.Unlock()
}

// saveInteractiveLiveTask saves any output still pending for the task, such as when its session ends
func saveInteractiveLiveTask(taskID int) {
	interactiveLiveTasks.Lock()
	liveTask, ok := interactiveLiveTasks.tasks[taskID]
	interactiveLiveTasks.Unlock()
	if !ok {
		return
	}
	liveTask.Lock()
	liveTask.save()
	liveTask.Unlock()
}

// forgetIdleInteractiveLiveTasks drops tasks nobody is watching that haven't had output for a while
func forgetIdleInteractiveLiveTasks() {
	interactiveLiveTasks.Lock()
	defer interactiveLiveTasks.Unlock()
	for taskID, liveTask := range interactiveLiveTasks.tasks {
		liveTask.Lock()
		if len(liveTask.viewers) == 0 && len(liveTask.pending) == 0 && time.Since(liveTask.lastActive) > interactiveLiveIdleTime {
			delete(interactiveLiveTasks.tasks, taskID)
			delete(interactiveLiveTasks.agentTaskIDs, liveTask.task.AgentTaskID)
		}
		liveTask.Unlock()
	}
}
