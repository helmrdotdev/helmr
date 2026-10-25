package main

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/jackc/pgx/v5"
)

// Permission fixtures contain people and policy only. Agent registrations have
// no deployment or executable bundle and cannot create Worker demand.
func seedPermissionData(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy) error {
	if err := org.SeedStandardRoles(ctx, tx, policy, mustUUID("00000000-0000-7000-8000-000000000201"), audit.System("local-development")); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, permissionSeedSQL)
	return err
}

const permissionSeedSQL = `
INSERT INTO users(id,display_name,primary_email) VALUES
 ('00000000-0000-7000-8000-000000000111','Casey Employee','casey@example.test'),
 ('00000000-0000-7000-8000-000000000112','Robin Observer','robin@example.test'),
 ('00000000-0000-7000-8000-000000000113','Sam Release','sam@example.test'),
 ('00000000-0000-7000-8000-000000000114','Alex Operator','alex@example.test'),
 ('00000000-0000-7000-8000-000000000115','Morgan Owner','morgan@example.test'),
 ('00000000-0000-7000-8000-000000000116','Jamie New Member','jamie@example.test');
INSERT INTO org_members(id,org_id,user_id,is_owner)
SELECT ('00000000-0000-7000-8000-' || lpad((right(id::text,3)::int+1000)::text,12,'0'))::uuid,'00000000-0000-7000-8000-000000000201',id,id='00000000-0000-7000-8000-000000000115'::uuid FROM users
WHERE id IN ('00000000-0000-7000-8000-000000000111','00000000-0000-7000-8000-000000000112','00000000-0000-7000-8000-000000000113','00000000-0000-7000-8000-000000000114','00000000-0000-7000-8000-000000000115','00000000-0000-7000-8000-000000000116');
INSERT INTO permission_groups(id,org_id,name) VALUES
 ('00000000-0000-7000-8000-000000002001','00000000-0000-7000-8000-000000000201','Business team'),
 ('00000000-0000-7000-8000-000000002002','00000000-0000-7000-8000-000000000201','Operations'),
 ('00000000-0000-7000-8000-000000002003','00000000-0000-7000-8000-000000000201','Release team');
INSERT INTO permission_group_members(org_id,group_id,member_id) VALUES
 ('00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002001','00000000-0000-7000-8000-000000001111'),
 ('00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002001','00000000-0000-7000-8000-000000001112'),
 ('00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002002','00000000-0000-7000-8000-000000001112'),
 ('00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002002','00000000-0000-7000-8000-000000001114'),
 ('00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002003','00000000-0000-7000-8000-000000001113');
INSERT INTO agents(id,environment_id,key) VALUES
 ('00000000-0000-7000-8000-000000002101','00000000-0000-7000-8000-000000000401','invoice-checker'),
 ('00000000-0000-7000-8000-000000002102','00000000-0000-7000-8000-000000000401','internal-faq');
INSERT INTO roles(id,org_id,name,description,permissions) VALUES
 ('00000000-0000-7000-8000-000000002201','00000000-0000-7000-8000-000000000201','Production promoter','Activate previously registered production deployments without uploading code or reading run logs.',ARRAY['deployments.promote']);
-- These example assignments are local fixtures, never product defaults.
INSERT INTO role_grants(id,org_id,group_id,role_id,environment_id,all_agents,created_by)
SELECT uuidv7(),'00000000-0000-7000-8000-000000000201',g.group_id::uuid,r.id,g.environment_id::uuid,g.all_agents,'00000000-0000-7000-8000-000000000101'
FROM (VALUES
 ('00000000-0000-7000-8000-000000002001','agent_user','00000000-0000-7000-8000-000000000401',false),
 ('00000000-0000-7000-8000-000000002002','session_viewer','00000000-0000-7000-8000-000000000401',true),
 ('00000000-0000-7000-8000-000000002002','session_operator','00000000-0000-7000-8000-000000000402',true),
 ('00000000-0000-7000-8000-000000002002','session_viewer','00000000-0000-7000-8000-000000000403',true),
 ('00000000-0000-7000-8000-000000002003','release_manager','00000000-0000-7000-8000-000000000402',false)
) AS g(group_id,role_key,environment_id,all_agents)
JOIN roles r ON r.org_id='00000000-0000-7000-8000-000000000201' AND r.builtin_key=g.role_key;
INSERT INTO role_grant_agents(grant_id,environment_id,agent_id)
SELECT g.id,g.environment_id,a.id FROM role_grants g JOIN agents a ON a.environment_id=g.environment_id
WHERE g.group_id='00000000-0000-7000-8000-000000002001' AND a.key IN ('invoice-checker','internal-faq');
INSERT INTO role_grants(id,org_id,group_id,role_id,environment_id,created_by) VALUES
 ('00000000-0000-7000-8000-000000002301','00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002003','00000000-0000-7000-8000-000000002201','00000000-0000-7000-8000-000000000401','00000000-0000-7000-8000-000000000101');
INSERT INTO role_grants(id,org_id,member_id,role_id,created_by)
SELECT uuidv7(),org_id,'00000000-0000-7000-8000-000000001101',id,'00000000-0000-7000-8000-000000000101' FROM roles
WHERE org_id='00000000-0000-7000-8000-000000000201' AND builtin_key IN ('access_administrator','organization_structure_administrator');
INSERT INTO role_grants(id,org_id,member_id,role_id,environment_id,all_agents,created_by)
SELECT uuidv7(),r.org_id,'00000000-0000-7000-8000-000000001101',r.id,e.id,true,'00000000-0000-7000-8000-000000000101' FROM roles r JOIN environments e ON e.org_id=r.org_id
WHERE r.org_id='00000000-0000-7000-8000-000000000201' AND r.builtin_key='environment_resource_administrator';
INSERT INTO role_grants(id,org_id,member_id,role_id,environment_id,all_agents,created_by)
SELECT uuidv7(),org_id,'00000000-0000-7000-8000-000000001111',id,'00000000-0000-7000-8000-000000000401',false,'00000000-0000-7000-8000-000000000101' FROM roles
WHERE org_id='00000000-0000-7000-8000-000000000201' AND builtin_key='agent_user';
INSERT INTO role_grant_agents(grant_id,environment_id,agent_id)
SELECT id,environment_id,'00000000-0000-7000-8000-000000002101' FROM role_grants
WHERE member_id='00000000-0000-7000-8000-000000001111';
-- The same custom Role has deliberately different Agent selections per environment.
INSERT INTO agents(id,environment_id,key) VALUES
 ('00000000-0000-7000-8000-000000002103','00000000-0000-7000-8000-000000000402','invoice-checker'),
 ('00000000-0000-7000-8000-000000002104','00000000-0000-7000-8000-000000000402','internal-faq');
INSERT INTO roles(id,org_id,name,description,permissions) VALUES
 ('00000000-0000-7000-8000-000000002202','00000000-0000-7000-8000-000000000201','Agent and secrets operator','Use selected Agents and manage secrets in the assigned environment.',ARRAY['agents.use','secrets.read','secrets.create','secrets.rotate','secrets.revoke']);
INSERT INTO role_grants(id,org_id,group_id,role_id,environment_id,all_agents,created_by) VALUES
 ('00000000-0000-7000-8000-000000002302','00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002003','00000000-0000-7000-8000-000000002202','00000000-0000-7000-8000-000000000401',false,'00000000-0000-7000-8000-000000000101'),
 ('00000000-0000-7000-8000-000000002303','00000000-0000-7000-8000-000000000201','00000000-0000-7000-8000-000000002003','00000000-0000-7000-8000-000000002202','00000000-0000-7000-8000-000000000402',false,'00000000-0000-7000-8000-000000000101');
INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) VALUES
 ('00000000-0000-7000-8000-000000002302','00000000-0000-7000-8000-000000000401','00000000-0000-7000-8000-000000002102'),
 ('00000000-0000-7000-8000-000000002303','00000000-0000-7000-8000-000000000402','00000000-0000-7000-8000-000000002103');
`
