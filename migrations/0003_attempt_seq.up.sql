-- seq numbers the history rows of one notification 1, 2, 3 in write order.
-- attempt_no is the notification's attempts counter at write time: a 429
-- refund gives the attempt back, so later rows can repeat it. seq never repeats.
ALTER TABLE notification_attempts ADD COLUMN seq int;

UPDATE notification_attempts a SET seq = r.n
FROM (SELECT id, row_number() OVER (PARTITION BY notification_id ORDER BY id) AS n
      FROM notification_attempts) r
WHERE a.id = r.id;

ALTER TABLE notification_attempts ALTER COLUMN seq SET NOT NULL;

CREATE OR REPLACE FUNCTION notification_attempts_set_seq() RETURNS trigger AS $$
BEGIN
    IF NEW.seq IS NULL THEN
        SELECT COALESCE(max(seq), 0) + 1 INTO NEW.seq
        FROM notification_attempts WHERE notification_id = NEW.notification_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER notification_attempts_seq BEFORE INSERT ON notification_attempts
    FOR EACH ROW EXECUTE FUNCTION notification_attempts_set_seq();

CREATE UNIQUE INDEX notification_attempts_seq_uq ON notification_attempts (notification_id, seq);
