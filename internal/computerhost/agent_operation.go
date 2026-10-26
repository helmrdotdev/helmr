package computerhost

import (
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func agentOperationMethod(method agentv1.Operation_Method) workerapi.AgentOperationMethod {
	switch method {
	case agentv1.Operation_METHOD_ASK:
		return workerapi.AgentMethodAsk
	case agentv1.Operation_METHOD_CLOSE_PROCESSING:
		return workerapi.AgentMethodCloseProcessing
	case agentv1.Operation_METHOD_CONTROL_SESSION:
		return workerapi.AgentMethodControlSession
	case agentv1.Operation_METHOD_ENQUEUE:
		return workerapi.AgentMethodEnqueue
	case agentv1.Operation_METHOD_FAIL:
		return workerapi.AgentMethodFail
	case agentv1.Operation_METHOD_FINALIZE:
		return workerapi.AgentMethodFinalize
	case agentv1.Operation_METHOD_HOLD:
		return workerapi.AgentMethodHold
	case agentv1.Operation_METHOD_INSPECT_SESSION:
		return workerapi.AgentMethodInspectSession
	case agentv1.Operation_METHOD_LIST_SESSIONS:
		return workerapi.AgentMethodListSessions
	case agentv1.Operation_METHOD_OUTPUT:
		return workerapi.AgentMethodOutput
	case agentv1.Operation_METHOD_REGISTER_MESSAGES:
		return workerapi.AgentMethodRegisterMessages
	case agentv1.Operation_METHOD_RESPOND:
		return workerapi.AgentMethodRespond
	case agentv1.Operation_METHOD_GET_MCP_CONNECTION:
		return workerapi.AgentMethodGetMCPConnection
	case agentv1.Operation_METHOD_RUNTIME_MCP:
		return workerapi.AgentMethodRuntimeMCP
	case agentv1.Operation_METHOD_SEND_MESSAGE:
		return workerapi.AgentMethodSendMessage
	case agentv1.Operation_METHOD_SPAWN:
		return workerapi.AgentMethodSpawn
	case agentv1.Operation_METHOD_START:
		return workerapi.AgentMethodStart
	case agentv1.Operation_METHOD_WAIT_ASK:
		return workerapi.AgentMethodWaitAsk
	case agentv1.Operation_METHOD_WAIT_TURN:
		return workerapi.AgentMethodWaitTurn
	case agentv1.Operation_METHOD_WITHDRAW_ASK:
		return workerapi.AgentMethodWithdrawAsk
	default:
		return ""
	}
}
