, target AS (
 SELECT p.* FROM slack_thread_sources p,input WHERE p.session_id=(args->>'session_id')::uuid
), posts AS (
 SELECT p.id,p.turn_id,p.role,p.seq,p.status,p.presentation_path,p.stream_state,
 p.desired_revision,p.confirmed_revision,p.suppressed_revision,p.payload_expired_at IS NOT NULL AS payload_expired,
 p.created_at,p.posted_at,p.closed_at,p.inflight_method,p.attempt_count,
 p.next_attempt_at,p.claimed_until,p.message_ts,p.error,p.delivery_disposed_at,p.reconciliation_paused_at
 FROM slack_posts p JOIN target s ON s.id=p.thread_source_id ORDER BY p.seq DESC LIMIT 100
)
SELECT jsonb_build_object(
 'participant', (SELECT jsonb_build_object('id',p.id,'projected_event_seq',p.projected_event_seq,'created_at',p.created_at,'next_event_seq',s.next_event_seq)
 FROM target p JOIN sessions s ON s.environment_id=p.environment_id AND s.id=p.session_id),
 'thread', (SELECT jsonb_build_object('id',t.id,'desired_status',t.desired_status,'desired_revision',t.desired_revision,
 'confirmed_revision',t.confirmed_revision,'status_confirmation',t.status_confirmation,'stream_status_repair',t.stream_status_repair,
 'inflight_method',t.inflight_method,'claimed_until',t.claimed_until,'next_attempt_at',t.next_attempt_at,'refresh_at',t.refresh_at,
 'delivery_error',t.delivery_error,'deleted_at',t.deleted_at) FROM slack_threads t JOIN target s ON s.thread_id=t.id),
 'installation', (SELECT jsonb_build_object('id',i.id,'credential_revision',i.credential_revision,'credential_expires_at',i.credential_expires_at,
 'authorized_at',i.authorized_at,'refresh_next_at',i.refresh_next_at,'refresh_error',i.refresh_error,
 'authorization_lost_at',i.authorization_lost_at,'disconnected_at',i.disconnected_at,'delivery_next_at',i.delivery_next_at)
 FROM slack_installations i JOIN slack_channels c ON c.installation_id=i.id JOIN slack_threads t ON t.channel_id=c.id JOIN target s ON s.thread_id=t.id),
 'posts', COALESCE((SELECT jsonb_agg(to_jsonb(p) ORDER BY p.seq) FROM posts p),'[]'::jsonb));
