CREATE TABLE templates (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    channel    text NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE notifications
    ADD CONSTRAINT notifications_template_fk FOREIGN KEY (template_id) REFERENCES templates (id);
