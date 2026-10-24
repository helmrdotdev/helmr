package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

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
// failed, an open Session with no Turns yet, and closed history. The history
// includes a long demo-triage Session (see seedDemoSessionHistory) with
// completed, failed and interrupted Turns, controls, messages, Questions and
// two child Sessions, a Session started with structured JSON input, and a
// closed Session whose history content has expired.
//
// Nothing is queued or running, no Question is pending, and every Computer
// lease and Session process is fenced, so no seeded row becomes Worker demand
// or reconciler work. The open Sessions use a Computer whose preparation
// failed, and the history ran on a since-deleted Computer whose leases name a
// lost Host in a disabled Worker Group, so allocation never selects either.
// Closed Sessions already carry their history retention clock. The cron
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
INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,status,authority_generation,next_turn_seq,next_event_seq,created_at,admission_mode,origin_kind,origin_user_id) VALUES
($1::uuid,$7::uuid,$4::uuid,$2::uuid,$6::uuid,$7::uuid,0,'closed',2,2,4,now()-interval '2 hours','environment','user','00000000-0000-7000-8000-000000000101'),
($1::uuid,$12::uuid,$10::uuid,$2::uuid,$6::uuid,$12::uuid,0,'open',1,1,1,now()-interval '90 minutes','environment','user','00000000-0000-7000-8000-000000000101'),
($1::uuid,$13::uuid,$11::uuid,$2::uuid,$6::uuid,$13::uuid,0,'open',1,2,3,now()-interval '50 minutes','environment','user','00000000-0000-7000-8000-000000000101'),
($1::uuid,$14::uuid,$10::uuid,$2::uuid,$6::uuid,$14::uuid,0,'closed',2,2,4,now()-interval '1 day','environment','user','00000000-0000-7000-8000-000000000101');
INSERT INTO session_holds(environment_id,id,session_id,scope,reason,created_at)
VALUES($1::uuid,$17::uuid,$12::uuid,'local','Waiting for approval before changing the demo billing plan',now()-interval '80 minutes');
INSERT INTO turns(environment_id,id,session_id,computer_id,seq,status,caller_kind,caller_id,admission_method,target_id,idempotency_key,request_digest,input,error_code,error_message,processing_closed_at,terminal_at) VALUES
($1::uuid,$8::uuid,$7::uuid,$6::uuid,1,'failed','user','00000000-0000-7000-8000-000000000101','create',$4::uuid,'demo-create',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Demo input"}]',NULL,NULL,now()-interval '1 hour',now()-interval '1 hour'),
($1::uuid,$15::uuid,$13::uuid,$6::uuid,1,'failed','user','00000000-0000-7000-8000-000000000101','create',$11::uuid,'demo-report',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Summarize this week''s demo tickets"}]','tool_error',convert_to('The demo ticket export timed out.','UTF8'),now()-interval '30 minutes',now()-interval '30 minutes'),
($1::uuid,$16::uuid,$14::uuid,$6::uuid,1,'cancelled','user','00000000-0000-7000-8000-000000000101','create',$10::uuid,'demo-triage',decode(repeat('00',32),'hex'),'[{"type":"text","text":"Triage the demo inbox"}]',NULL,NULL,now()-interval '23 hours',now()-interval '23 hours');
INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data,created_at) VALUES
($1::uuid,$7::uuid,1,$8::uuid,'turn.queued','{}',now()-interval '2 hours'),
($1::uuid,$7::uuid,2,$8::uuid,'turn.failed','{"code":"preparation_deadline"}',now()-interval '1 hour'),
($1::uuid,$7::uuid,3,NULL,'session.closed','{}',now()-interval '1 hour'),
($1::uuid,$13::uuid,1,$15::uuid,'turn.queued','{}',now()-interval '50 minutes'),
($1::uuid,$13::uuid,2,$15::uuid,'turn.failed','{"code":"tool_error"}',now()-interval '30 minutes'),
($1::uuid,$14::uuid,1,$16::uuid,'turn.queued','{}',now()-interval '1 day'),
($1::uuid,$14::uuid,2,$16::uuid,'turn.cancelled','{}',now()-interval '23 hours'),
($1::uuid,$14::uuid,3,NULL,'session.closed','{}',now()-interval '23 hours');
UPDATE sessions SET history_eligible_at=now()-interval '1 hour' WHERE environment_id=$1::uuid AND id=$7::uuid;
UPDATE sessions SET history_eligible_at=now()-interval '23 hours' WHERE environment_id=$1::uuid AND id=$14::uuid;
`, pgx.QueryExecModeSimpleProtocol, demoSeedEnvironmentID, demoSeedDeploymentID, demoSeedSpecID,
		demoSeedAgentID, demoSeedTriggerID, demoSeedComputerID, demoSeedSessionID, demoSeedTurnID, demoDigest("terminal-history"),
		demoSeedTriageAgentID, demoSeedReporterAgentID, demoSeedHeldSessionID, demoSeedFailedSessionID, demoSeedClosedSessionID,
		demoSeedFailedTurnID, demoSeedClosedTurnID, demoSeedHoldID)
	if err != nil {
		return err
	}
	return seedDemoSessionHistory(ctx, tx)
}

func demoDigest(label string) string {
	sum := sha256.Sum256([]byte("helmr.dev.console-demo-seed\x00" + label))
	return fmt.Sprintf("sha256:%x", sum)
}

const (
	demoSeedHistoryComputerID       = "00000000-0000-7000-8000-000000000602"
	demoSeedLongSessionID           = "00000000-0000-7000-8000-000000000705"
	demoSeedChildSessionID          = "00000000-0000-7000-8000-000000000706"
	demoSeedJSONSessionID           = "00000000-0000-7000-8000-000000000707"
	demoSeedExpiredSessionID        = "00000000-0000-7000-8000-000000000708"
	demoSeedEmptySessionID          = "00000000-0000-7000-8000-000000000709"
	demoSeedCancelledChildSessionID = "00000000-0000-7000-8000-000000000710"
	demoSeedRetiredGroupID          = "00000000-0000-7000-8000-000000000801"
	demoSeedRetiredPoolID           = "00000000-0000-7000-8000-000000000802"
	demoSeedRetiredHostID           = "00000000-0000-7000-8000-000000000803"
	demoSeedRetiredServiceID        = "00000000-0000-7000-8000-000000000804"
)

type demoTicket struct {
	number          int
	subject         string
	queue, priority string
	customer, plan  string
	confidence      float64
	labels          []string
}

var demoTickets = []demoTicket{
	{4801, "Invoices page shows the wrong currency", "billing", "high", "Acme Corp", "enterprise", 0.94, []string{"invoices", "currency"}},
	{4803, "Password reset email never arrives", "identity", "high", "Globex", "team", 0.91, []string{"email", "login"}},
	{4806, "Feature request: dark mode for reports", "product", "low", "Initech", "team", 0.88, []string{"feature-request"}},
	{4808, "Webhook retries flood our endpoint", "integrations", "medium", "Umbrella", "enterprise", 0.83, []string{"webhooks", "retries"}},
	{4812, "CSV export times out for large workspaces", "data", "high", "Acme Corp", "enterprise", 0.9, []string{"export", "timeout"}},
	{4815, "Seat count did not update after upgrade", "billing", "medium", "Hooli", "team", 0.86, []string{"seats", "upgrade"}},
	{4817, "SAML login loops back to the sign-in page", "identity", "urgent", "Globex", "enterprise", 0.95, []string{"sso", "saml"}},
	{4821, "API returns 429 well under the documented limit", "platform", "high", "Stark Industries", "enterprise", 0.8, []string{"api", "rate-limit"}},
	{4824, "Typo in the onboarding checklist", "product", "low", "Pied Piper", "free", 0.97, []string{"copy"}},
	{4826, "Refund requested for a duplicate charge", "billing", "high", "Wayne Enterprises", "team", 0.92, []string{"refund", "duplicate-charge"}},
	{4829, "Slack notifications arrive twice", "integrations", "medium", "Initech", "team", 0.79, []string{"slack", "duplicates"}},
	{4831, "Audit log is missing role changes", "security", "high", "Umbrella", "enterprise", 0.87, []string{"audit-log"}},
	{4834, "Cannot remove a deactivated member", "identity", "medium", "Hooli", "team", 0.84, []string{"members"}},
	{4836, "Dashboard charts render blank in Safari", "product", "medium", "Pied Piper", "free", 0.76, []string{"safari", "charts"}},
	{4839, "Tax ID rejected for an EU customer", "billing", "medium", "Acme Corp", "enterprise", 0.89, []string{"tax", "eu"}},
	{4841, "Scheduled report sent at the wrong hour", "data", "low", "Globex", "team", 0.82, []string{"schedules", "timezone"}},
	{4844, "Two-factor codes rejected after phone change", "identity", "high", "Stark Industries", "enterprise", 0.9, []string{"2fa"}},
	{4846, "Request for a data processing agreement", "legal", "medium", "Wayne Enterprises", "team", 0.93, []string{"dpa", "compliance"}},
	{4848, "Search ignores accented characters", "product", "low", "Initech", "team", 0.81, []string{"search", "i18n"}},
	{4851, "Workspace import stuck at 99%", "data", "high", "Umbrella", "enterprise", 0.85, []string{"import"}},
	{4853, "Invoice PDF missing the purchase order number", "billing", "medium", "Hooli", "team", 0.88, []string{"invoices", "po-number"}},
	{4856, "OAuth app shows an unverified publisher warning", "integrations", "low", "Pied Piper", "free", 0.77, []string{"oauth"}},
	{4858, "Session timeout too short for kiosk devices", "security", "medium", "Acme Corp", "enterprise", 0.8, []string{"sessions", "policy"}},
	{4861, "Bulk edit drops custom field values", "product", "high", "Globex", "team", 0.86, []string{"bulk-edit", "data-loss"}},
	{4863, "Card declined although the bank approved it", "billing", "high", "Stark Industries", "enterprise", 0.84, []string{"payments"}},
	{4866, "Mobile app crashes when opening attachments", "product", "high", "Wayne Enterprises", "team", 0.83, []string{"mobile", "crash"}},
	{4868, "Need a copy of last year's invoices", "billing", "low", "Initech", "team", 0.96, []string{"invoices"}},
	{4871, "IP allowlist blocks the office VPN", "security", "urgent", "Umbrella", "enterprise", 0.9, []string{"ip-allowlist", "vpn"}},
	{4873, "Calendar sync creates duplicate events", "integrations", "medium", "Hooli", "team", 0.78, []string{"calendar", "duplicates"}},
	{4877, "Refund notice needed for a cancelled annual plan", "billing", "medium", "Pied Piper", "free", 0.9, []string{"refund", "annual"}},
	{4880, "Usage report totals do not match invoices", "data", "high", "Acme Corp", "enterprise", 0.82, []string{"usage", "reconciliation"}},
	{4882, "Welcome email links to a staging URL", "product", "medium", "Globex", "team", 0.95, []string{"email", "links"}},
	{4885, "Members cannot accept invitations on Firefox", "identity", "medium", "Stark Industries", "enterprise", 0.79, []string{"invitations", "firefox"}},
	{4887, "Question about data residency in the EU", "legal", "low", "Wayne Enterprises", "team", 0.91, []string{"data-residency"}},
}

// seedDemoSessionHistory adds the Session log demo: Session 705 is a long
// closed demo-triage Session; 706 and 710 are its completed and cancelled child
// Sessions; 707 started from structured JSON input and closed after idling;
// 708 is closed with expired history; 709 is open with no Turns yet.
func seedDemoSessionHistory(ctx context.Context, tx pgx.Tx) error {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return err
	}
	computerCreated := now.Add(-60 * time.Hour)
	computerDeleted := now.Add(-4 * time.Hour)
	platform := demoDigest("vm-platform")
	if _, err := tx.Exec(ctx, `
INSERT INTO worker_group_tokens(id,token_hash) VALUES($1::uuid,$9::bytea);
INSERT INTO worker_groups(id,token_id,region_id,name,description,status,created_at,updated_at)
VALUES($1::uuid,$1::uuid,current_setting('helmr.seed_region_id'),'console-demo-retired','Retired capacity that ran the console demo history','disabled',$6::timestamptz,$7::timestamptz);
INSERT INTO vm_platforms(id,descriptor_digest,firecracker_digest,firecracker_version,snapshot_format_version,host_kernel_release,kernel_digest,initramfs_digest,rootfs_digest,last_seen_at)
VALUES($5,$5,$5,'1.16.1','6.0.0','console-demo',$5,$5,$5,$7::timestamptz);
INSERT INTO worker_pools(id,worker_group_id,name,status,created_at,updated_at) VALUES($2::uuid,$1::uuid,'console-demo-retired','disabled',$6::timestamptz,$7::timestamptz);
INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,epoch_started_at,activated_at,vm_platform_id,observed_at,lost_at,created_at,updated_at)
VALUES($3::uuid,'console-demo-retired-host',$1::uuid,$2::uuid,'lost',1,$4::uuid,$6::timestamptz,$6::timestamptz,$5,$7::timestamptz,$7::timestamptz,$6::timestamptz,$7::timestamptz);
INSERT INTO computers(environment_id,id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,initial_root_digest,initial_payload_retired_at,deleted_at,created_at,updated_at,last_activity_at)
VALUES($8::uuid,$10::uuid,$11::uuid,$12::uuid,1000,536870912,$13::bytea,$7::timestamptz,$7::timestamptz,$6::timestamptz,$7::timestamptz,$7::timestamptz-interval '30 minutes');
`, pgx.QueryExecModeSimpleProtocol, demoSeedRetiredGroupID, demoSeedRetiredPoolID, demoSeedRetiredHostID, demoSeedRetiredServiceID, platform,
		computerCreated, computerDeleted, demoSeedEnvironmentID, demoSHA("retired-group-token"), demoSeedHistoryComputerID, demoSeedSpecID, demoSeedDeploymentID,
		demoSHA("history-computer-root")); err != nil {
		return err
	}
	leases := []struct {
		epoch       int64
		from, until time.Time
	}{{1, now.Add(-59 * time.Hour), now.Add(-56 * time.Hour)}, {2, now.Add(-28*time.Hour - 10*time.Minute), computerDeleted}}
	for _, l := range leases {
		if _, err := tx.Exec(ctx, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,allocation_id,channel_credential_digest,delivered_at,initialized_at,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,status,fenced_at,fence_evidence)
VALUES($1,$2,$3,$4,1,$6::timestamptz+interval '1 minute',$7,$8,$5,$5,536870912,1073741824,$9,1,$9,'releasing',$6,'Computer released after its Sessions settled')`,
			demoSeedEnvironmentID, demoSeedHistoryComputerID, l.epoch, demoSeedRetiredHostID, l.from, l.until, demoUUID(810+int(l.epoch)), demoSHA(fmt.Sprintf("lease-%d", l.epoch)), platform); err != nil {
			return err
		}
	}
	h := &demoHistory{}
	seedDemoExpiredSession(h, now)
	seedDemoLongSession(h, now)
	seedDemoJSONSession(h, now)
	h.sessions = append(h.sessions, &demoSessionRow{id: demoSeedEmptySessionID, agent: demoSeedAgentID, computer: demoSeedComputerID, status: "open", generation: 1, createdAt: now.Add(-10 * time.Minute), nextTurn: 1, nextEvent: 1})
	return h.write(ctx, tx, demoSeedEnvironmentID, computerDeleted)
}

func demoUserCaller(method, target string) demoCaller {
	return demoCaller{kind: "user", id: defaultUserID, method: method, target: target}
}

// triage is one ordinary completed Turn of the long Session.
func (s *demoLog) triage(turn int, t demoTicket) *demoTurn {
	s.wait(26 * time.Minute)
	input := demoContent(fmt.Sprintf("Triage ticket #%d: %s", t.number, t.subject))
	turnRow := s.enqueue(demoUUID(1000+turn), demoUserCaller("enqueue", s.row.id), fmt.Sprintf("triage-%d", t.number), input)
	s.wait(3 * time.Second)
	s.run(turnRow)
	s.wait(18 * time.Second)
	s.output(turnRow, demoText(fmt.Sprintf("Reading ticket #%d from %s (%s plan).", t.number, t.customer, t.plan)))
	s.wait(41 * time.Second)
	s.output(turnRow, demoValue(map[string]any{"ticket": t.number, "queue": t.queue, "priority": t.priority, "confidence": t.confidence, "labels": t.labels}))
	s.wait(12 * time.Second)
	s.complete(turnRow, demoContent(fmt.Sprintf("Routed #%d to %s with %s priority.", t.number, t.queue, t.priority)),
		map[string]any{"ticket": t.number, "queue": t.queue, "priority": t.priority, "assignee": nil})
	return turnRow
}

func seedDemoLongSession(h *demoHistory, now time.Time) {
	session := demoSeedLongSessionID
	s := h.session(&demoSessionRow{id: session, agent: demoSeedTriageAgentID, computer: demoSeedHistoryComputerID, status: "open", generation: 1, createdAt: now.Add(-28 * time.Hour)}, 2)
	user := demoUserCaller("enqueue", session)
	tickets := demoTickets
	next := func() demoTicket {
		t := tickets[0]
		tickets = tickets[1:]
		return t
	}
	// Turn 1 opens the triage pass.
	t := s.enqueue(demoUUID(1001), demoUserCaller("create", demoSeedTriageAgentID), "weekly-triage", demoContent("Start the weekly triage pass on the demo support inbox. Keep billing tickets in their own queue and flag anything that breaches its SLA."))
	s.wait(20 * time.Second)
	s.startProcess()
	s.run(t)
	s.wait(15 * time.Second)
	s.output(t, demoText("Connected to the demo support inbox."))
	s.wait(30 * time.Second)
	s.output(t, demoText("Loaded 120 open tickets; 14 are past their first-response SLA."), demoValue(map[string]any{"open": 120, "breached": 14, "queues": []string{"billing", "data", "identity", "integrations", "legal", "platform", "product", "security"}}))
	s.wait(10 * time.Second)
	s.complete(t, demoContent("Ready to triage 120 open tickets, starting with the oldest."), map[string]any{"open": 120, "breached": 14})
	for turn := 2; turn <= 4; turn++ {
		s.triage(turn, next())
	}
	// Turn 5 returns the full snapshot as one large structured part.
	s.wait(25 * time.Minute)
	t = s.enqueue(demoUUID(1005), user, "snapshot", demoContent("Export the full open-ticket snapshot as structured data so I can compare it with last week."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(25 * time.Second)
	s.output(t, demoText("Building the snapshot from 120 open tickets…"))
	s.wait(70 * time.Second)
	s.output(t, demoValue(demoSnapshot(now)))
	s.wait(8 * time.Second)
	s.complete(t, demoContent("Snapshot exported: 120 tickets across 8 queues."), map[string]any{"tickets": 120, "queues": 8, "format": "snapshot.v2"})
	for turn := 6; turn <= 7; turn++ {
		s.triage(turn, next())
	}
	// Turn 8 asks a choice Question and continues with the answer.
	s.wait(24 * time.Minute)
	t = s.enqueue(demoUUID(1008), user, "duplicates", demoContent("Check whether any of this week's tickets are duplicates of each other."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(40 * time.Second)
	s.output(t, demoText("Tickets #4812, #4819 and #4833 all describe the CSV export timeout."))
	s.wait(5 * time.Second)
	ask := s.ask(t, demoUUID(8001), map[string]any{
		"title":  "Duplicate tickets",
		"type":   "choice",
		"prompt": []any{demoText("Tickets #4812, #4819 and #4833 describe the same CSV export timeout. How should I handle them?")},
		"options": []any{
			map[string]any{"id": "merge", "label": "Merge into #4812", "description": "Close the newer tickets and keep the oldest as the primary.", "value": "merge_into_oldest"},
			map[string]any{"id": "link", "label": "Link as related", "description": "Keep all three open and cross-link them.", "value": "link_related"},
			map[string]any{"id": "leave", "label": "Leave as is", "value": "no_change"},
		},
		"allowNotes": true,
	})
	s.wait(6 * time.Minute)
	s.answer(ask, map[string]any{"selected": []any{map[string]any{"id": "merge", "value": "merge_into_oldest"}}, "notes": "Keep the Acme contact on #4812 informed."})
	s.wait(20 * time.Second)
	s.output(t, demoText("Merged #4819 and #4833 into #4812 and notified the Acme contact."))
	s.wait(5 * time.Second)
	s.complete(t, demoContent("Merged 2 duplicate tickets into #4812."), map[string]any{"merged": []int{4819, 4833}, "primary": 4812})
	// Turn 9 writes a long multi-line report.
	s.wait(22 * time.Minute)
	t = s.enqueue(demoUUID(1009), user, "weekly-report", demoContent("Write the weekly triage report for the support channel."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(90 * time.Second)
	s.output(t, demoText(demoReport()))
	s.wait(5 * time.Second)
	s.complete(t, demoContent("The weekly triage report is ready to share."), map[string]any{"sections": 6, "lines": 64})
	for turn := 10; turn <= 11; turn++ {
		s.triage(turn, next())
	}
	// Turn 12 is interrupted by the user while a Question and a started
	// message are outstanding; both settle with the Turn.
	s.wait(25 * time.Minute)
	t = s.enqueue(demoUUID(1012), user, "sla-recheck", demoContent("Re-run the SLA breach check across every enterprise ticket."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(30 * time.Second)
	s.output(t, demoText("Checking 41 enterprise tickets against their contract SLAs…"))
	s.wait(20 * time.Second)
	s.ask(t, demoUUID(8002), map[string]any{"type": "text", "prompt": []any{demoText("Should tickets that are already escalated count toward the breach total?")}})
	s.wait(40 * time.Second)
	m := s.message(t, demoUUID(8101), "Also include the Globex tickets from last week.")
	s.wait(2 * time.Second)
	s.advanceMessage(t, m, "started")
	s.wait(25 * time.Second)
	s.settle(t, "interrupted")
	s.control(demoUUID(8201), "turn_interrupt", "user", defaultUserID, "", t)
	s.row.generation++
	s.wait(4 * time.Second)
	s.stopProcess("stopped")
	for turn := 13; turn <= 14; turn++ {
		if turn == 13 {
			s.wait(26 * time.Minute)
			t = s.enqueue(demoUUID(1013), user, "sla-recheck-2", demoContent("Run the SLA check again, counting escalated tickets, and include Globex from last week."))
			s.wait(25 * time.Second)
			s.startProcess()
			s.run(t)
			s.wait(50 * time.Second)
			s.output(t, demoText("17 enterprise tickets breached their SLA this week, 3 of them already escalated."))
			s.wait(5 * time.Second)
			s.complete(t, demoContent("17 SLA breaches found; 3 were already escalated."), map[string]any{"breached": 17, "escalated": 3, "customers": []string{"Acme Corp", "Globex", "Umbrella"}})
			continue
		}
		s.triage(turn, next())
	}
	// Turn 15 fails in a tool call.
	s.wait(25 * time.Minute)
	t = s.enqueue(demoUUID(1015), user, "search-similar", demoContent("Find older tickets similar to #4821 so we can reuse the answer."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(12 * time.Second)
	s.output(t, demoText("Searching resolved tickets for “429 under the documented limit”…"))
	s.wait(95 * time.Second)
	s.fail(t, "tool_error", "Ticket search returned HTTP 503 after 3 retries.")
	for turn := 16; turn <= 17; turn++ {
		s.triage(turn, next())
	}
	// Turn 18 receives a message mid-Turn and uses it.
	s.wait(24 * time.Minute)
	tk := next()
	t = s.enqueue(demoUUID(1018), user, "triage-with-note", demoContent(fmt.Sprintf("Triage ticket #%d: %s", tk.number, tk.subject)))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(15 * time.Second)
	m = s.message(t, demoUUID(8102), "Prioritise anything from Initech today; their renewal is this week.")
	s.wait(2 * time.Second)
	s.advanceMessage(t, m, "started")
	s.wait(1 * time.Second)
	s.advanceMessage(t, m, "delivered")
	s.wait(20 * time.Second)
	s.output(t, demoText(fmt.Sprintf("Raised #%d to high priority because Initech renews this week.", tk.number)))
	s.wait(10 * time.Second)
	s.complete(t, demoContent(fmt.Sprintf("Routed #%d to %s with high priority.", tk.number, tk.queue)), map[string]any{"ticket": tk.number, "queue": tk.queue, "priority": "high"})
	s.triage(19, next())
	// Turn 20 spawns a demo-reporter child Session from structured input.
	s.wait(26 * time.Minute)
	t = s.enqueue(demoUUID(1020), user, "status-summary", demoContent("Ask demo-reporter to draft the customer-facing status summary for the CSV export incident."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(20 * time.Second)
	s.output(t, demoText("Starting a demo-reporter Session for the status summary."))
	s.wait(5 * time.Second)
	seedDemoChildSession(h, s, t, demoSeedChildSessionID, demoUUID(1101))
	s.at = latestDemoEvent(h, demoSeedChildSessionID).Add(10 * time.Second)
	s.output(t, demoText("demo-reporter finished the status summary."), demoValue(map[string]any{"session": demoSeedChildSessionID, "status": "closed", "words": 182}))
	s.wait(5 * time.Second)
	s.complete(t, demoContent("The status summary is drafted in the child Session."), map[string]any{"child_session": demoSeedChildSessionID})
	for turn := 21; turn <= 23; turn++ {
		s.triage(turn, next())
	}
	// Turn 24 is interrupted by a Session hold; Turn 25 waits for the resume.
	s.wait(25 * time.Minute)
	tk = next()
	t = s.enqueue(demoUUID(1024), user, "refund-policy", demoContent(fmt.Sprintf("Triage ticket #%d: %s", tk.number, tk.subject)))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(30 * time.Second)
	s.output(t, demoText("Drafting a refund reply under the 30-day policy…"))
	s.wait(20 * time.Second)
	hold := s.hold(demoUUID(8301), "subtree", "Pausing while billing confirms the refund policy", "user", defaultUserID)
	s.settle(t, "interrupted")
	s.control(demoUUID(8202), "interrupt", "user", defaultUserID, hold.id, nil)
	s.wait(5 * time.Second)
	s.stopProcess("stopped")
	s.wait(5 * time.Minute)
	t = s.enqueue(demoUUID(1025), user, "refund-policy-2", demoContent(fmt.Sprintf("Billing confirmed: refunds within 60 days are fine. Finish ticket #%d.", tk.number)))
	s.wait(10 * time.Minute)
	s.release(hold)
	s.control(demoUUID(8203), "resume", "user", defaultUserID, hold.id, nil)
	s.wait(25 * time.Second)
	s.startProcess()
	s.run(t)
	s.wait(35 * time.Second)
	s.output(t, demoText(fmt.Sprintf("Sent the refund confirmation for #%d under the 60-day policy.", tk.number)))
	s.wait(5 * time.Second)
	s.complete(t, demoContent(fmt.Sprintf("Refund approved for #%d.", tk.number)), map[string]any{"ticket": tk.number, "refund": "approved", "policy_days": 60})
	for turn := 26; turn <= 27; turn++ {
		s.triage(turn, next())
	}
	// Turn 28's response could not be mirrored to Slack.
	completed := s.triage(28, next())
	s.at = s.at.Add(time.Second)
	seq := s.row.nextEvent - 1
	s.event(completed, "slack.delivery_unavailable", map[string]any{"reason": "route_unavailable", "source_seq": seq})
	s.triage(29, next())
	// Turn 30 asks a text Question.
	s.wait(25 * time.Minute)
	t = s.enqueue(demoUUID(1030), user, "escalation-channel", demoContent("Set up escalation routing for the urgent tickets."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(20 * time.Second)
	ask = s.ask(t, demoUUID(8003), map[string]any{"type": "text", "title": "Escalation channel", "prompt": []any{demoText("Which Slack channel should urgent escalations go to?")}})
	s.wait(3 * time.Minute)
	s.answer(ask, "#support-escalations")
	s.wait(15 * time.Second)
	s.output(t, demoText("Urgent tickets now escalate to #support-escalations."))
	s.wait(5 * time.Second)
	s.complete(t, demoContent("Escalation routing is set to #support-escalations."), map[string]any{"channel": "#support-escalations", "urgent_tickets": 2})
	// Turn 31 is interrupted when its process fails; the system hold is resumed.
	s.wait(25 * time.Minute)
	tk = next()
	t = s.enqueue(demoUUID(1031), user, "triage-process-loss", demoContent(fmt.Sprintf("Triage ticket #%d: %s", tk.number, tk.subject)))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(25 * time.Second)
	s.output(t, demoText(fmt.Sprintf("Reading ticket #%d from %s (%s plan).", tk.number, tk.customer, tk.plan)))
	s.wait(30 * time.Second)
	s.settle(t, "interrupted")
	s.stopProcess("lost")
	failureHold := s.hold(demoUUID(8302), "local", "Session process failed", "system", "")
	s.event(nil, "session.process_failed", map[string]any{"holdId": failureHold.id, "processEpoch": s.epoch})
	s.wait(12 * time.Minute)
	s.release(failureHold)
	s.control(demoUUID(8204), "resume", "user", defaultUserID, failureHold.id, nil)
	s.wait(20 * time.Second)
	s.startProcess()
	for turn := 32; turn <= 33; turn++ {
		s.triage(turn, next())
	}
	// Turn 34 fails with an application error.
	s.wait(25 * time.Minute)
	t = s.enqueue(demoUUID(1034), user, "auto-assign", demoContent("Auto-assign the remaining billing tickets to the on-call agents."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(20 * time.Second)
	s.output(t, demoValue(map[string]any{"candidates": 6, "on_call": []string{"maria", "dev", "lee"}}))
	s.wait(15 * time.Second)
	s.fail(t, "application_error", "Classifier returned an unknown queue \"billing-urgent\"; expected one of billing, data, identity, integrations, legal, platform, product, security.")
	for turn := 35; turn <= 37; turn++ {
		s.triage(turn, next())
	}
	// Turn 38 starts a child Session and cancels it.
	s.wait(25 * time.Minute)
	t = s.enqueue(demoUUID(1038), user, "refund-notice", demoContent("Have demo-reporter draft the refund notice for ticket #4877."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(15 * time.Second)
	s.output(t, demoText("Starting a demo-reporter Session for the refund notice."))
	s.wait(5 * time.Second)
	seedDemoCancelledChildSession(h, s, t)
	s.at = latestDemoEvent(h, demoSeedCancelledChildSessionID).Add(10 * time.Second)
	s.output(t, demoText("Cancelled the draft: billing will send the refund notice manually."))
	s.wait(5 * time.Second)
	s.complete(t, demoContent("The refund notice draft was cancelled; billing owns it."), map[string]any{"child_session": demoSeedCancelledChildSessionID, "cancelled": true})
	for turn := 39; turn <= 45; turn++ {
		s.triage(turn, next())
	}
	s.wait(12 * time.Minute)
	s.close(nil, demoUUID(8205))
}

// seedDemoChildSession is a demo-reporter Session spawned with JSON input by
// the parent's Turn and closed by the parent when its work is done.
func seedDemoChildSession(h *demoHistory, parent *demoLog, origin *demoTurn, id, turnID string) {
	s := h.session(&demoSessionRow{id: id, agent: demoSeedReporterAgentID, computer: demoSeedHistoryComputerID, root: parent.row.id, parent: parent.row.id, origin: origin.id, depth: 1, status: "open", generation: 1, createdAt: parent.at}, parent.lease)
	caller := demoCaller{kind: "session", id: parent.row.id, origin: origin.id, method: "spawn", target: demoSeedReporterAgentID}
	t := s.enqueue(turnID, caller, "status-summary", demoJSON(map[string]any{
		"audience":  "customers",
		"incident":  map[string]any{"title": "CSV export timeouts", "tickets": []int{4812, 4819, 4833}, "started_at": parent.at.Add(-50 * time.Hour).UTC().Format(time.RFC3339)},
		"tone":      "plain",
		"max_words": 200,
	}))
	s.wait(15 * time.Second)
	s.startProcess()
	s.run(t)
	s.wait(40 * time.Second)
	s.output(t, demoText("Drafting the status summary for customers…"))
	s.wait(50 * time.Second)
	summary := "We have fixed an issue that caused CSV exports to time out for large workspaces. Exports started between Monday and Wednesday may need to be run again. No data was lost."
	s.output(t, demoText(summary))
	s.wait(5 * time.Second)
	s.complete(t, demoContent(summary), map[string]any{"words": 182, "status": "draft"})
	s.wait(10 * time.Second)
	s.control(demoUUID(8206), "close", "session", parent.row.id, "", nil)
	s.row.status = "closed"
	s.row.generation++
	s.event(nil, "session.closed", nil)
	s.wait(4 * time.Second)
	s.stopProcess("stopped")
	s.event(nil, "session.process_stopped", nil)
	s.row.eligibleAt = s.at
}

// seedDemoCancelledChildSession is cancelled by its parent while running.
func seedDemoCancelledChildSession(h *demoHistory, parent *demoLog, origin *demoTurn) {
	s := h.session(&demoSessionRow{id: demoSeedCancelledChildSessionID, agent: demoSeedReporterAgentID, computer: demoSeedHistoryComputerID, root: parent.row.id, parent: parent.row.id, origin: origin.id, depth: 1, status: "open", generation: 1, createdAt: parent.at}, parent.lease)
	caller := demoCaller{kind: "session", id: parent.row.id, origin: origin.id, method: "spawn", target: demoSeedReporterAgentID}
	t := s.enqueue(demoUUID(1131), caller, "refund-notice", demoContent("Draft a refund notice for ticket #4877 (cancelled annual plan, Pied Piper)."))
	s.wait(15 * time.Second)
	s.startProcess()
	s.run(t)
	s.wait(30 * time.Second)
	s.output(t, demoText("Drafting the refund notice for Pied Piper…"))
	s.wait(45 * time.Second)
	s.settle(t, "cancelled")
	s.row.status = "cancelled"
	s.row.generation++
	s.event(nil, "session.cancelled", nil)
	s.control(demoUUID(8207), "cancel", "session", parent.row.id, "", nil)
	s.wait(4 * time.Second)
	s.stopProcess("stopped")
	s.event(nil, "session.process_stopped", nil)
	s.row.eligibleAt = s.at
}

// seedDemoJSONSession starts from structured JSON input rather than text and
// closes after its idle period.
func seedDemoJSONSession(h *demoHistory, now time.Time) {
	s := h.session(&demoSessionRow{id: demoSeedJSONSessionID, agent: demoSeedAgentID, computer: demoSeedHistoryComputerID, status: "open", generation: 1, createdAt: now.Add(-6 * time.Hour), closeAfterIdleMS: 900000}, 2)
	t := s.enqueue(demoUUID(1111), demoUserCaller("create", demoSeedAgentID), "renewal-check", demoJSON(map[string]any{
		"accounts": []any{
			map[string]any{"id": "acct_acme", "renewal": "2026-11-01", "seats": 250},
			map[string]any{"id": "acct_globex", "renewal": "2026-11-15", "seats": 80},
		},
		"checks":  []string{"open_tickets", "sla_breaches", "usage_trend"},
		"dry_run": true,
		"notify":  nil,
	}))
	s.wait(20 * time.Second)
	s.startProcess()
	s.run(t)
	s.wait(30 * time.Second)
	s.output(t, demoValue(map[string]any{"account": "acct_acme", "open_tickets": 9, "sla_breaches": 2, "usage_trend": "up"}))
	s.wait(20 * time.Second)
	s.output(t, demoValue(map[string]any{"account": "acct_globex", "open_tickets": 4, "sla_breaches": 1, "usage_trend": "flat"}))
	s.wait(5 * time.Second)
	s.complete(t, nil, map[string]any{
		"accounts": []any{
			map[string]any{"id": "acct_acme", "risk": "medium", "reasons": []string{"2 SLA breaches", "9 open tickets"}},
			map[string]any{"id": "acct_globex", "risk": "low", "reasons": []string{"1 SLA breach"}},
		},
		"dry_run": true,
	})
	s.wait(15 * time.Minute)
	s.close(map[string]any{"reason": "close_after_idle"}, "")
}

// seedDemoExpiredSession is closed and its history content has expired under
// the Project's retention policy; identities, statuses and digests remain.
func seedDemoExpiredSession(h *demoHistory, now time.Time) {
	s := h.session(&demoSessionRow{id: demoSeedExpiredSessionID, agent: demoSeedTriageAgentID, computer: demoSeedHistoryComputerID, status: "open", generation: 1, createdAt: now.Add(-59 * time.Hour)}, 1)
	t := s.enqueue(demoUUID(1121), demoUserCaller("create", demoSeedTriageAgentID), "legacy-triage", demoContent("Triage the backlog left over from the previous support tool."))
	s.wait(30 * time.Second)
	s.startProcess()
	s.run(t)
	s.wait(40 * time.Second)
	s.output(t, demoText("Found 38 tickets in the legacy backlog."))
	ask := s.ask(t, demoUUID(8004), map[string]any{"type": "text", "prompt": []any{demoText("Should closed legacy tickets be imported too?")}})
	s.wait(4 * time.Minute)
	s.answer(ask, "No, only open ones.")
	s.wait(30 * time.Second)
	s.complete(t, demoContent("Imported 21 open legacy tickets."), map[string]any{"imported": 21})
	s.wait(20 * time.Minute)
	t = s.enqueue(demoUUID(1122), demoUserCaller("enqueue", s.row.id), "legacy-attachments", demoContent("Copy the legacy attachments as well."))
	s.wait(3 * time.Second)
	s.run(t)
	s.wait(60 * time.Second)
	s.fail(t, "tool_error", "The legacy attachment store refused the export token.")
	s.wait(30 * time.Minute)
	s.close(nil, demoUUID(8208))
	h.expire(demoSeedExpiredSessionID, now.Add(-3*time.Hour))
}

func latestDemoEvent(h *demoHistory, session string) time.Time {
	var at time.Time
	for _, e := range h.events {
		if e.session == session && e.at.After(at) {
			at = e.at
		}
	}
	return at
}

// demoSnapshot is a large structured output with nested objects and a long array.
func demoSnapshot(now time.Time) map[string]any {
	queues := []string{"billing", "data", "identity", "integrations", "legal", "platform", "product", "security"}
	priorities := []string{"low", "medium", "high", "urgent"}
	customers := []map[string]any{
		{"name": "Acme Corp", "plan": "enterprise", "region": "us-east"},
		{"name": "Globex", "plan": "team", "region": "eu-west"},
		{"name": "Initech", "plan": "team", "region": "us-west"},
		{"name": "Umbrella", "plan": "enterprise", "region": "eu-central"},
		{"name": "Hooli", "plan": "team", "region": "us-west"},
		{"name": "Pied Piper", "plan": "free", "region": "us-west"},
		{"name": "Stark Industries", "plan": "enterprise", "region": "us-east"},
		{"name": "Wayne Enterprises", "plan": "team", "region": "us-east"},
	}
	subjects := []string{"Export times out", "Login loop", "Wrong invoice currency", "Webhook retries", "Seat count stale", "Missing audit entries", "Search ignores accents", "Import stuck"}
	tickets := make([]any, 0, 120)
	counts := map[string]int{}
	breached := 0
	for i := range 120 {
		queue := queues[(i*5+i/7)%len(queues)]
		priority := priorities[(i*3+i/5)%len(priorities)]
		counts[queue]++
		due := now.Add(-28*time.Hour + time.Duration(i*37)*time.Minute).UTC()
		isBreached := i%9 == 0
		if isBreached {
			breached++
		}
		ticket := map[string]any{
			"id":       4700 + i,
			"subject":  subjects[i%len(subjects)],
			"queue":    queue,
			"priority": priority,
			"customer": customers[i%len(customers)],
			"sla":      map[string]any{"first_response_due": due.Format(time.RFC3339), "breached": isBreached},
			"labels":   []string{queue, priority},
			"assignee": nil,
		}
		if i%11 == 0 {
			ticket["duplicates"] = []int{4700 + i + 3}
		}
		tickets = append(tickets, ticket)
	}
	return map[string]any{
		"generated_at": now.Add(-27 * time.Hour).UTC().Format(time.RFC3339),
		"summary":      map[string]any{"open": 120, "breached": breached, "by_queue": counts, "oldest_ticket": 4700},
		"filters":      map[string]any{"status": []string{"open", "pending"}, "updated_since": now.Add(-7 * 24 * time.Hour).UTC().Format("2006-01-02"), "include_spam": false},
		"tickets":      tickets,
	}
}

func demoReport() string {
	return `Weekly support triage report

Summary
- 120 open tickets at the start of the week; 14 had already breached their first-response SLA.
- Billing is the largest queue (23 tickets), followed by identity (19) and product (18).
- Three tickets about the CSV export timeout were merged into #4812.
- Median time to first response: 3h 12m (target: 4h).

Highlights
1. CSV export timeouts (#4812) affect large enterprise workspaces. Engineering has a fix in review.
2. SAML login loops (#4817) are limited to Globex and caused by a stale IdP certificate on their side.
3. Duplicate Slack notifications (#4829) started after the integrations deploy on Tuesday.
4. The refund backlog is down to 4 tickets after billing confirmed the 60-day policy.

Queues
  billing        23 open   5 breached   oldest: 3d 4h
  data           14 open   2 breached   oldest: 2d 1h
  identity       19 open   3 breached   oldest: 1d 22h
  integrations   12 open   1 breached   oldest: 1d 6h
  legal           6 open   0 breached   oldest: 4d 2h
  platform        9 open   1 breached   oldest: 0d 19h
  product        18 open   1 breached   oldest: 5d 3h
  security       11 open   1 breached   oldest: 1d 2h
  unsorted        8 open   0 breached   oldest: 0d 3h

Customers to watch
- Acme Corp: 9 open tickets, 2 SLA breaches, renewal on November 1.
- Globex: SAML loop is blocking 40 seats; workaround sent.
- Umbrella: IP allowlist issue escalated to security as urgent.

Escalations
- #4817 SAML login loop → identity on-call (urgent)
- #4871 IP allowlist blocks the office VPN → security on-call (urgent)
- #4821 API 429s under the documented limit → platform (high)

Process notes
- Billing tickets now stay in their own queue during triage.
- Escalated tickets count toward the SLA breach total from this week on.
- The legacy backlog import finished; 21 open tickets came across.

Next week
- Re-run the duplicate check on Monday morning.
- Pair with billing on the refund macro.
- Review the 8 unsorted tickets before Tuesday's stand-up.

Generated by demo-triage.`
}
