package agent

// NoSessionHoldsSQL is true for a Session row s when neither it nor an
// ancestor holding its subtree has an unreleased hold.
const NoSessionHoldsSQL = `NOT EXISTS(WITH RECURSIVE ancestors AS (
 SELECT s.id,s.parent_session_id
 UNION ALL SELECT parent.id,parent.parent_session_id FROM sessions parent JOIN ancestors a ON parent.id=a.parent_session_id WHERE parent.environment_id=s.environment_id)
 SELECT 1 FROM session_holds hold JOIN ancestors a ON a.id=hold.session_id WHERE hold.environment_id=s.environment_id AND hold.released_at IS NULL AND (hold.session_id=s.id OR hold.scope='subtree'))`
