-- A failed supplier query must retain the last confirmed status and not starve other orders.
ALTER TABLE fulfillment_orders ADD COLUMN sync_failures integer NOT NULL DEFAULT 0;
ALTER TABLE fulfillment_orders ADD COLUMN sync_error text NOT NULL DEFAULT '';
ALTER TABLE fulfillment_orders ADD COLUMN next_sync_at bigint NOT NULL DEFAULT 0;
CREATE INDEX fulfillment_sync_due ON fulfillment_orders(next_sync_at,checked_at) WHERE status NOT IN ('succeeded','failed_safe');
