-- requeued_at is set by a manual retry (POST /v1/notifications/{id}/retry).
-- MAX_DELIVERY_AGE is measured from the latest of scheduled_at or created_at
-- and requeued_at, so an operator requeue gets a fresh delivery window.
ALTER TABLE notifications ADD COLUMN requeued_at timestamptz;
