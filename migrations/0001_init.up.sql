CREATE TABLE batches (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key text UNIQUE,
    request_hash    text,
    total           int NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notifications (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id            uuid REFERENCES batches (id),
    idempotency_key     text UNIQUE,
    request_hash        text,
    channel             text NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    recipient           text NOT NULL,
    content             text NOT NULL,
    template_id         uuid,
    template_vars       jsonb,
    priority            smallint NOT NULL DEFAULT 1 CHECK (priority BETWEEN 0 AND 2),
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'processing', 'sent', 'failed', 'cancelled')),
    attempts            int NOT NULL DEFAULT 0,
    max_attempts        int NOT NULL DEFAULT 8,
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    scheduled_at        timestamptz,
    locked_until        timestamptz,
    lease_token         uuid,
    batch_position      int,
    provider_message_id text,
    last_error          text,
    correlation_id      text NOT NULL,
    trace_parent        text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    sent_at             timestamptz
);

CREATE TABLE notification_attempts (
    id              bigserial PRIMARY KEY,
    notification_id uuid NOT NULL REFERENCES notifications (id),
    attempt_no      int NOT NULL,
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz,
    http_status     int,
    error           text,
    duration_ms     int
);

CREATE INDEX notification_attempts_notification_idx ON notification_attempts (notification_id);

CREATE INDEX notifications_claim_idx ON notifications (channel, priority, next_attempt_at) WHERE status = 'pending';
CREATE INDEX notifications_reaper_idx ON notifications (locked_until) WHERE status = 'processing';
CREATE INDEX notifications_created_idx ON notifications (created_at, id);
CREATE INDEX notifications_status_created_idx ON notifications (status, created_at, id);
CREATE INDEX notifications_channel_created_idx ON notifications (channel, created_at, id);
CREATE UNIQUE INDEX notifications_batch_position_uq ON notifications (batch_id, batch_position);
