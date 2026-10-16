package workerapi

const TurnReceiptsPending = "turn_receipts_pending"

// AgentOperationMethod identifies a host-to-control-plane operation. The guest
// protobuf method is translated by the host at the transport boundary.
type AgentOperationMethod string

const (
	AgentMethodAsk              AgentOperationMethod = "ask"
	AgentMethodCloseProcessing  AgentOperationMethod = "close_processing"
	AgentMethodControlSession   AgentOperationMethod = "control_session"
	AgentMethodEnqueue          AgentOperationMethod = "enqueue"
	AgentMethodFail             AgentOperationMethod = "fail"
	AgentMethodFinalize         AgentOperationMethod = "finalize"
	AgentMethodHold             AgentOperationMethod = "hold"
	AgentMethodInspectSession   AgentOperationMethod = "inspect_session"
	AgentMethodListSessions     AgentOperationMethod = "list_sessions"
	AgentMethodOutput           AgentOperationMethod = "output"
	AgentMethodRegisterMessages AgentOperationMethod = "register_messages"
	AgentMethodRespond          AgentOperationMethod = "respond"
	AgentMethodRuntimeMCP       AgentOperationMethod = "runtime_mcp"
	AgentMethodSendMessage      AgentOperationMethod = "send_message"
	AgentMethodSpawn            AgentOperationMethod = "spawn"
	AgentMethodStart            AgentOperationMethod = "start"
	AgentMethodWaitAsk          AgentOperationMethod = "wait_ask"
	AgentMethodWaitTurn         AgentOperationMethod = "wait_turn"
	AgentMethodWithdrawAsk      AgentOperationMethod = "withdraw_ask"
)
