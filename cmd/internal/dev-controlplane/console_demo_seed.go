package main

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	demoSeedEnvironmentID   = "00000000-0000-7000-8000-000000000403"
	demoSeedDeploymentID    = "00000000-0000-7000-8000-000000000501"
	demoSeedSpecID          = "00000000-0000-7000-8000-000000000504"
	demoSeedAgentID         = "00000000-0000-7000-8000-000000000511"
	demoSeedTriageAgentID   = "00000000-0000-7000-8000-000000000512"
	demoSeedReporterAgentID = "00000000-0000-7000-8000-000000000513"
	demoSeedTriggerID       = "00000000-0000-7000-8000-000000000521"
	demoSeedComputerID      = "00000000-0000-7000-8000-000000000601"
	demoSeedSessionID       = "00000000-0000-7000-8000-000000000701"
	demoSeedHeldSessionID   = "00000000-0000-7000-8000-000000000702"
	demoSeedFailedSessionID = "00000000-0000-7000-8000-000000000703"
	demoSeedClosedSessionID = "00000000-0000-7000-8000-000000000704"
	demoSeedTurnID          = "00000000-0000-7000-8000-000000000711"
	demoSeedFailedTurnID    = "00000000-0000-7000-8000-000000000713"
	demoSeedClosedTurnID    = "00000000-0000-7000-8000-000000000714"
	demoSeedHoldID          = "00000000-0000-7000-8000-000000000722"
)

// The demo Environment illustrates the Console with retained, not executable,
// work: a promoted Deployment of three Agents and a Template with no bundle
// bytes, an open Session held for approval, an open Session whose latest Turn
// failed, and closed history. Nothing is queued or running and no Session has a
// process, lease or Save, so no seeded row becomes Worker demand; the cron
// trigger only fires where a trigger scheduler runs, which this control plane
// does not. A pending question needs a running Turn on a live Computer lease,
// so the demo has none.
func seedDemoEnvironmentData(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO deployments(environment_id,id,bundle_digest,created_at)
VALUES($1::uuid,$2::uuid,$9::text,now()-interval '3 days');
INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed,created_at)
VALUES($1::uuid,$3::uuid,$9::text,'{}','{}',now()-interval '3 days');
INSERT INTO templates(environment_id,id,key) VALUES($1::uuid,$3::uuid,'demo-computer');
INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes)
VALUES($1::uuid,$2::uuid,$3::uuid,$3::uuid,1000,536870912);
INSERT INTO agents(environment_id,id,key) VALUES
($1::uuid,$4::uuid,'demo-agent'),($1::uuid,$10::uuid,'demo-triage'),($1::uuid,$11::uuid,'demo-reporter');
INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) VALUES
($1::uuid,$4::uuid,$2::uuid,$3::uuid,false),($1::uuid,$10::uuid,$2::uuid,$3::uuid,false),($1::uuid,$11::uuid,$2::uuid,$3::uuid,false);
INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input)
VALUES($1::uuid,$4::uuid,$2::uuid,'daily','0 9 * * *','UTC','[{"type":"text","text":"Demo input"}]');
INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,next_fire_at)
VALUES($1::uuid,$5::uuid,$4::uuid,$2::uuid,'daily',now()-interval '2 days',
 (date_trunc('day',now() AT TIME ZONE 'UTC')+CASE WHEN (now() AT TIME ZONE 'UTC')::time<'09:00' THEN interval '9 hours' ELSE interval '33 hours' END) AT TIME ZONE 'UTC');
UPDATE environments SET current_deployment_id=$2::uuid WHERE id=$1::uuid;
INSERT INTO deployment_events(environment_id,deployment_id,kind,message,accepted_at) VALUES
($1::uuid,$2::uuid,'deployment.registered','Deployment registered',now()-interval '3 days'),
($1::uuid,$2::uuid,'deployment.promoted','Deployment promoted',now()-interval '3 days');
INSERT INTO computers(environment_id,id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes,image_spec_id,preparation_deadline_at,preparation_failed_at)
VALUES($1::uuid,$6::uuid,$3::uuid,$2::uuid,1000,536870912,34359738368,$3::uuid,now()-interval '1 hour',now()-interval '1 hour');
INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,status,authority_generation,next_turn_seq,next_event_seq,created_at) VALUES
($1::uuid,$7::uuid,$4::uuid,$2::uuid,$6::uuid,$7::uuid,0,'closed',2,2,4,now()-interval '2 hours'),
($1::uuid,$12::uuid,$10::uuid,$2::uuid,$6::uuid,$12::uuid,0,'open',1,1,1,now()-interval '90 minutes'),
($1::uuid,$13::uuid,$11::uuid,$2::uuid,$6::uuid,$13::uuid,0,'open',1,2,3,now()-interval '50 minutes'),
($1::uuid,$14::uuid,$10::uuid,$2::uuid,$6::uuid,$14::uuid,0,'closed',2,2,4,now()-interval '1 day');
INSERT INTO session_holds(environment_id,id,session_id,scope,reason,created_at)
VALUES($1::uuid,$17::uuid,$12::uuid,'local','Waiting for approval before changing the demo billing plan',now()-interval '80 minutes');
INSERT INTO turns(environment_id,id,session_id,computer_id,seq,first_event_seq,status,caller_kind,caller_id,admission_method,target_id,idempotency_key,request_digest,input,error_code,error_message,processing_closed_at,terminal_at) VALUES
($1::uuid,$8::uuid,$7::uuid,$6::uuid,1,1,'failed','user','00000000-0000-7000-8000-000000000101','create',$4::uuid,'demo-create',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Demo input"}]',NULL,NULL,now()-interval '1 hour',now()-interval '1 hour'),
($1::uuid,$15::uuid,$13::uuid,$6::uuid,1,1,'failed','user','00000000-0000-7000-8000-000000000101','create',$11::uuid,'demo-report',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Summarize this week''s demo tickets"}]','tool_error',convert_to('The demo ticket export timed out.','UTF8'),now()-interval '30 minutes',now()-interval '30 minutes'),
($1::uuid,$16::uuid,$14::uuid,$6::uuid,1,1,'cancelled','user','00000000-0000-7000-8000-000000000101','create',$10::uuid,'demo-triage',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Triage the demo inbox"}]',NULL,NULL,now()-interval '23 hours',now()-interval '23 hours');
INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data,created_at) VALUES
($1::uuid,$7::uuid,1,$8::uuid,'turn.queued','{}',now()-interval '2 hours'),
($1::uuid,$7::uuid,2,$8::uuid,'turn.failed','{"code":"preparation_deadline"}',now()-interval '1 hour'),
($1::uuid,$7::uuid,3,NULL,'session.closed','{}',now()-interval '1 hour'),
($1::uuid,$13::uuid,1,$15::uuid,'turn.queued','{}',now()-interval '50 minutes'),
($1::uuid,$13::uuid,2,$15::uuid,'turn.failed','{"code":"tool_error"}',now()-interval '30 minutes'),
($1::uuid,$14::uuid,1,$16::uuid,'turn.queued','{}',now()-interval '1 day'),
($1::uuid,$14::uuid,2,$16::uuid,'turn.cancelled','{}',now()-interval '23 hours'),
($1::uuid,$14::uuid,3,NULL,'session.closed','{}',now()-interval '23 hours');
`, pgx.QueryExecModeSimpleProtocol, demoSeedEnvironmentID, demoSeedDeploymentID, demoSeedSpecID,
		demoSeedAgentID, demoSeedTriggerID, demoSeedComputerID, demoSeedSessionID, demoSeedTurnID, demoDigest("terminal-history"),
		demoSeedTriageAgentID, demoSeedReporterAgentID, demoSeedHeldSessionID, demoSeedFailedSessionID, demoSeedClosedSessionID,
		demoSeedFailedTurnID, demoSeedClosedTurnID, demoSeedHoldID)
	return err
}

func demoDigest(label string) string {
	sum := sha256.Sum256([]byte("helmr.dev.console-demo-seed\x00" + label))
	return fmt.Sprintf("sha256:%x", sum)
}
