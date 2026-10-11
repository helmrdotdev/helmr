package main

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	demoSeedEnvironmentID = "00000000-0000-7000-8000-000000000403"
	demoSeedDeploymentID  = "00000000-0000-7000-8000-000000000501"
	demoSeedSpecID        = "00000000-0000-7000-8000-000000000504"
	demoSeedAgentID       = "00000000-0000-7000-8000-000000000511"
	demoSeedScheduleID    = "00000000-0000-7000-8000-000000000521"
	demoSeedComputerID    = "00000000-0000-7000-8000-000000000601"
	demoSeedSessionID     = "00000000-0000-7000-8000-000000000701"
	demoSeedTurnID        = "00000000-0000-7000-8000-000000000711"
)

// This is illustrative terminal history, not an executable Deployment. It has
// no bundle bytes, VM, lease, process, Save or checkpoint. Revocation and the
// ended schedule prevent synthetic inputs from becoming Worker demand.
func seedDemoEnvironmentData(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO deployments(environment_id,id,bundle_digest,created_at,execution_revoked_at)
VALUES($1,$2,$9,now()-interval '3 days',now());
UPDATE environments SET current_deployment_id=$2 WHERE id=$1;
INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed,created_at)
VALUES($1,$3,$9,'{}','{}',now()-interval '3 days');
INSERT INTO computer_definitions(environment_id,deployment_id,definition_key,preparation_spec_id,resources)
VALUES($1,$2,'demo-computer',$3,'{"milliCpu":1000,"memoryMiB":512}');
INSERT INTO agents(environment_id,id,name) VALUES($1,$4,'demo-agent');
INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers)
VALUES($1,$4,$2,'demo-agent','demo-computer',false,'{}');
INSERT INTO agent_schedules(environment_id,id,agent_id,deployment_id,trigger_key,cron,timezone,input,active_from,active_until,next_fire_at,lateness_tolerance_ms)
VALUES($1,$5,$4,$2,'daily','0 9 * * 1-5','UTC','[{"type":"text","text":"Demo input"}]',now()-interval '2 days',now()-interval '1 day',now(),60000);
INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at,preparation_failed_at)
VALUES($1,$6,$3,now()-interval '1 hour',now()-interval '1 hour');
INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,status,authority_generation,next_turn_seq,next_event_seq,created_at)
VALUES('until_environment_deletion',$1,$7,$4,$2,$6,$7,0,'closed',2,2,4,now()-interval '2 hours');
INSERT INTO turns(environment_id,id,session_id,computer_id,seq,status,caller_kind,caller_id,admission_method,target_id,retry_key,request_digest,input,processing_closed_at,terminal_at)
VALUES($1,$8,$7,$6,1,'failed','user','00000000-0000-7000-8000-000000000101','start',$4,'demo-start',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Demo input"}]',now()-interval '1 hour',now()-interval '1 hour');
INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data,created_at) VALUES
($1,$7,1,$8,'turn.queued','{}',now()-interval '2 hours'),
($1,$7,2,$8,'turn.failed','{"code":"preparation_deadline"}',now()-interval '1 hour'),
($1,$7,3,NULL,'session.closed','{}',now()-interval '1 hour');
`, pgx.QueryExecModeSimpleProtocol, demoSeedEnvironmentID, demoSeedDeploymentID, demoSeedSpecID,
		demoSeedAgentID, demoSeedScheduleID, demoSeedComputerID, demoSeedSessionID, demoSeedTurnID, demoDigest("terminal-history"))
	return err
}

func demoDigest(label string) string {
	sum := sha256.Sum256([]byte("helmr.dev.console-demo-seed\x00" + label))
	return fmt.Sprintf("sha256:%x", sum)
}
