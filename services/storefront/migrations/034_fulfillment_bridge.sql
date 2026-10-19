CREATE TABLE fulfillment_orders (
 order_id text PRIMARY KEY REFERENCES orders(id),
 plan text NOT NULL,
 region text NOT NULL,
 execution_started boolean NOT NULL DEFAULT false,
 status text NOT NULL DEFAULT 'awaiting_session',
 message text NOT NULL DEFAULT '',
 remote_updated_at bigint NOT NULL DEFAULT 0,
 checked_at bigint NOT NULL DEFAULT 0
);
CREATE TABLE fulfillment_nonces (nonce text PRIMARY KEY, expires_at bigint NOT NULL);
