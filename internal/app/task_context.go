package app

import (
	"github.com/stayradiated/mosscap/internal/protocol"
)

func (a App) taskContextTyped(selector string, threadLimit int, jsonOut bool) error {
	if threadLimit < 0 || threadLimit > 100 {
		return usage("--thread-limit must be between 1 and 100")
	}
	c, err := loadCommandConfig()
	if err != nil {
		return err
	}
	params := protocol.TaskContextParams{Task: selector, ThreadLimit: threadLimit}
	if err := checkTaskFrame(c, "context", "req_task_context", params, false, false, a.Version); err != nil {
		return err
	}
	out, err := a.gatewayCall(c, "task.context", "req_task_context", params, protocol.CapabilityTaskContext)
	if err != nil {
		return mutationJSONError(err, false, jsonOut, "req_task_context")
	}
	return a.render(out, jsonOut)
}
