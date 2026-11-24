DROP INDEX notification_attempts_seq_uq;

DROP TRIGGER notification_attempts_seq ON notification_attempts;

DROP FUNCTION notification_attempts_set_seq();

ALTER TABLE notification_attempts DROP COLUMN seq;
