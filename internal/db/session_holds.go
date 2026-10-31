package db

// SessionHoldsSQL selects the unreleased holds that stop Session row s: its own
// holds and interrupts placed on any ancestor. Every other hold kind is local.
const SessionHoldsSQL = `WITH RECURSIVE ancestors AS (
 SELECT s.id,s.parent_session_id
 UNION ALL SELECT parent.id,parent.parent_session_id FROM sessions parent JOIN ancestors a ON parent.id=a.parent_session_id WHERE parent.environment_id=s.environment_id)
 SELECT hold.* FROM session_holds hold JOIN ancestors a ON a.id=hold.session_id WHERE hold.environment_id=s.environment_id AND hold.released_at IS NULL AND (hold.session_id=s.id OR hold.kind='interrupt')`

// SessionHeldSQL is true for a Session row s with an effective hold.
const SessionHeldSQL = `EXISTS(` + SessionHoldsSQL + `)`

// NoSessionHoldsSQL is true for a Session row s without an effective hold.
const NoSessionHoldsSQL = `NOT ` + SessionHeldSQL
