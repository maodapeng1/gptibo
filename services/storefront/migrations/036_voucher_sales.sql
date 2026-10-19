CREATE TABLE voucher_batches (
 id text PRIMARY KEY, title text NOT NULL, plan text NOT NULL, region text NOT NULL,
 valid_days integer NOT NULL CHECK(valid_days BETWEEN 1 AND 3650), created_by text NOT NULL, created_at bigint NOT NULL
);
CREATE TABLE vouchers (
 id text PRIMARY KEY, batch_id text NOT NULL REFERENCES voucher_batches(id), code_hash text NOT NULL UNIQUE,
 mask text NOT NULL, state text NOT NULL DEFAULT 'inactive' CHECK(state IN ('inactive','active','locked','review_required','redeemed','revoked')),
 version bigint NOT NULL DEFAULT 0, sales_order_id text REFERENCES orders(id), expires_at bigint, created_at bigint NOT NULL
);
CREATE TABLE offline_receipts (
 id text PRIMARY KEY, order_id text NOT NULL UNIQUE REFERENCES orders(id), channel text NOT NULL,
 account_label text NOT NULL, transaction_reference text NOT NULL, amount_cents bigint NOT NULL CHECK(amount_cents>0),
 note text NOT NULL, actor text NOT NULL, created_at bigint NOT NULL,
 UNIQUE(channel,account_label,transaction_reference)
);
CREATE TABLE voucher_contexts (
 id text PRIMARY KEY, voucher_id text NOT NULL REFERENCES vouchers(id), buyer_hash text NOT NULL,
 expires_at bigint NOT NULL, created_at bigint NOT NULL
);
CREATE TABLE voucher_redemptions (
 voucher_id text PRIMARY KEY REFERENCES vouchers(id), order_id text NOT NULL UNIQUE REFERENCES orders(id),
 context_id text NOT NULL UNIQUE REFERENCES voucher_contexts(id), operation_id text NOT NULL UNIQUE,
 account_hash text NOT NULL, state text NOT NULL DEFAULT 'processing', created_at bigint NOT NULL
);
CREATE TABLE order_guest_access (
 order_id text NOT NULL REFERENCES orders(id), buyer_hash text NOT NULL,
 created_at bigint NOT NULL, PRIMARY KEY(order_id,buyer_hash)
);
CREATE TABLE order_recovery_secrets (
 order_id text PRIMARY KEY REFERENCES orders(id), token_hash text NOT NULL UNIQUE,
 version bigint NOT NULL DEFAULT 0, created_at bigint NOT NULL
);
CREATE TABLE voucher_events (
 id text PRIMARY KEY, voucher_id text NOT NULL REFERENCES vouchers(id), actor text NOT NULL,
 action text NOT NULL, note text NOT NULL, created_at bigint NOT NULL
);
CREATE TABLE voucher_dispatch (
 order_id text PRIMARY KEY REFERENCES orders(id), operation_id text NOT NULL UNIQUE,
 context_id text NOT NULL, state text NOT NULL DEFAULT 'pending', created_at bigint NOT NULL
);

CREATE UNIQUE INDEX voucher_live_sale ON vouchers(sales_order_id) WHERE state<>'revoked';
CREATE TABLE offline_refunds (
 id text PRIMARY KEY, order_id text NOT NULL UNIQUE REFERENCES orders(id),
 transaction_reference text NOT NULL UNIQUE, amount_cents bigint NOT NULL, note text NOT NULL,
 actor text NOT NULL, created_at bigint NOT NULL
);
ALTER TABLE fulfillment_orders ADD COLUMN remote_order_id text;
ALTER TABLE fulfillment_orders ADD COLUMN card_order_id text;
ALTER TABLE fulfillment_orders ADD COLUMN provider_operation_id text;
ALTER TABLE fulfillment_orders ADD COLUMN error_code text;
ALTER TABLE fulfillment_orders ADD COLUMN error_request_id text;
ALTER TABLE fulfillment_orders ADD COLUMN account_email_masked text;
