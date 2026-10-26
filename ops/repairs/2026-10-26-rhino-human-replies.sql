-- Exact, reviewed correction for two Zendesk agent replies in campaign 226.
-- Default: validate and preview inside a rolled-back transaction.
-- Apply with psql -X -v ON_ERROR_STOP=1 -v apply=true -f <this file>.
-- Original bodies, headers, timestamps, event IDs and all send rows are preserved.
\set ON_ERROR_STOP on
\if :{?apply}
\else
  \set apply false
\endif

BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '15s';

CREATE TEMP TABLE rhino_reply_targets (event_id bigint, snapshot_id bigint, message_id text, occurred_at timestamptz) ON COMMIT DROP;
INSERT INTO rhino_reply_targets VALUES
  (2416, 2066, '1a13a97bbc8e7a0b', '2026-10-14T13:26:23Z'),
  (2707, 2357, '1a178a007db2e99b', '2026-10-26T14:31:53Z');

DO $repair$
DECLARE
  valid_count integer;
  corrected_count integer;
BEGIN
  -- Same global lock as the production tick. Never race scheduled delivery.
  IF NOT pg_try_advisory_xact_lock(7165064710523742563) THEN
    RAISE EXCEPTION 'cold-cli tick lock is busy; retry after the tick finishes';
  END IF;

  PERFORM 1 FROM campaign_leads WHERE campaign_id=226 AND lead_id=5441 FOR UPDATE;
  PERFORM 1 FROM events WHERE id IN (2416,2707) FOR UPDATE;
  PERFORM 1 FROM email_messages WHERE id IN (2066,2357) FOR UPDATE;

  SELECT count(*) INTO valid_count
  FROM rhino_reply_targets t
  JOIN events e ON e.id=t.event_id
  JOIN email_messages em ON em.id=t.snapshot_id
  JOIN campaigns c ON c.id=e.campaign_id
  JOIN campaign_leads cl ON cl.campaign_id=c.id AND cl.lead_id=e.lead_id
  JOIN leads l ON l.id=e.lead_id
  WHERE c.id=226 AND c.workspace_id='productlair'
    AND c.name='publisher-partnerships-2026-10-12' AND c.status='completed'
    AND l.id=5441 AND lower(l.email)='hello@rhinorank.io'
    AND e.account_id=3 AND em.account_id=3
    AND em.campaign_id=226 AND em.lead_id=5441 AND em.direction='inbound'
    AND e.message_id=t.message_id AND em.message_id=t.message_id
    AND e.thread_id='1a12f2e2dcf5d5d0' AND em.thread_id=e.thread_id
    AND e.timestamp=t.occurred_at AND em.occurred_at=t.occurred_at
    AND e.type=em.type AND e.type IN ('auto_reply','reply')
    AND cl.status IN ('active','replied')
    AND em.raw_headers::jsonb->>'X-Mailer'='Zendesk Mailer'
    AND em.raw_headers::jsonb->>'Auto-Submitted'='auto-generated';
  IF valid_count<>2 THEN
    RAISE EXCEPTION 'reviewed Rhino rows no longer match (matched %/2); no correction applied', valid_count;
  END IF;
  IF EXISTS (SELECT 1 FROM scheduled_sends WHERE campaign_id=226 AND status<>'sent') OR
     (SELECT count(*) FROM scheduled_sends WHERE campaign_id=226)<>30 THEN
    RAISE EXCEPTION 'campaign send history changed; review again before correcting';
  END IF;

  SELECT count(*) INTO corrected_count FROM events e JOIN rhino_reply_targets t ON t.event_id=e.id
  WHERE e.type='reply' AND e.metadata::jsonb->'classification_corrections' @>
    '[{"repair_id":"2026-10-26-rhino-human-replies"}]'::jsonb;
  IF corrected_count=2 AND EXISTS (SELECT 1 FROM campaign_leads WHERE campaign_id=226 AND lead_id=5441 AND status='replied') THEN
    RAISE NOTICE 'Rhino correction already applied; no changes';
    RETURN;
  END IF;
  IF EXISTS (SELECT 1 FROM events e JOIN rhino_reply_targets t ON t.event_id=e.id WHERE e.type<>'auto_reply') THEN
    RAISE EXCEPTION 'unexpected partial or external reclassification; review before proceeding';
  END IF;

  UPDATE events e SET type='reply', metadata=jsonb_set(e.metadata::jsonb,
    '{classification_corrections}', coalesce(e.metadata::jsonb->'classification_corrections','[]'::jsonb) ||
    jsonb_build_array(jsonb_build_object(
      'repair_id','2026-10-26-rhino-human-replies',
      'reason','Human Zendesk agent comment incorrectly treated as an automatic reply',
      'corrected_at',CURRENT_TIMESTAMP,
      'previous_type',e.type,
      'snapshot_id',t.snapshot_id,
      'previous_snapshot_type',(SELECT type FROM email_messages WHERE id=t.snapshot_id),
      'previous_campaign_lead_status',(SELECT status FROM campaign_leads WHERE campaign_id=226 AND lead_id=5441)
    )),true)::text
  FROM rhino_reply_targets t WHERE e.id=t.event_id;

  UPDATE email_messages SET type='reply' WHERE id IN (SELECT snapshot_id FROM rhino_reply_targets);
  UPDATE campaign_leads SET status='replied' WHERE campaign_id=226 AND lead_id=5441 AND status='active';
END
$repair$;

SELECT e.id,e.type,e.message_id,e.timestamp,em.type AS snapshot_type,cl.status AS lead_status
FROM rhino_reply_targets t JOIN events e ON e.id=t.event_id
JOIN email_messages em ON em.id=t.snapshot_id
JOIN campaign_leads cl ON cl.campaign_id=e.campaign_id AND cl.lead_id=e.lead_id
ORDER BY e.id;

\if :apply
  COMMIT;
\else
  ROLLBACK;
\endif
