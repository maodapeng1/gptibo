package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gptibo/storefront/migrations"
)

type Store struct{ DB *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	c, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	c.MaxConns = 10
	c.ConnConfig.ConnectTimeout = 5 * time.Second
	db, err := pgxpool.NewWithConfig(ctx, c)
	if err != nil {
		return nil, errors.New("database initialization failed")
	}
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = db.Ping(ping); err != nil {
		db.Close()
		return nil, errors.New("database unavailable")
	}
	return &Store{DB: db}, nil
}

func checksum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Do not let a queued schema lock stall live authentication traffic for the
	// full maintenance timeout. A busy database leaves the migration unapplied.
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '3s'"); err != nil {
		return err
	}
	// Transaction-scoped lock prevents parallel migration races and releases on failure.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(903493)"); err != nil {
		return err
	}
	var existing bool
	if err = tx.QueryRow(ctx, "SELECT to_regclass('storefront_migrations') IS NOT NULL").Scan(&existing); err != nil {
		return err
	}
	if !existing {
		var count int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind IN ('r','p','v','m','S','f')").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("first migration requires an empty, dedicated storefront schema")
		}
		if _, err = tx.Exec(ctx, "CREATE TABLE storefront_migrations(name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
			return err
		}
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	var applied int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM storefront_migrations").Scan(&applied); err != nil {
		return err
	}
	if applied > len(entries) {
		return errors.New("database is newer than this release")
	}
	for _, entry := range entries {
		b, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			return err
		}
		var hash string
		err = tx.QueryRow(ctx, "SELECT checksum FROM storefront_migrations WHERE name=$1", entry.Name()).Scan(&hash)
		if err == nil {
			if !migrationMatches(entry.Name(), b, hash) {
				return fmt.Errorf("migration changed: %s", entry.Name())
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, string(b)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO storefront_migrations(name,checksum) VALUES($1,$2)", entry.Name(), checksum(b)); err != nil {
			return err
		}
	}
	// Also reject foreign migration names rather than silently accepting a mixed history.
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM storefront_migrations").Scan(&applied); err != nil {
		return err
	}
	if applied != len(entries) {
		return errors.New("unexpected migration history")
	}
	return tx.Commit(ctx)
}

func (s *Store) Verify(ctx context.Context) error {
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	var count int
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM storefront_migrations").Scan(&count); err != nil {
		return errors.New("storefront migrations unavailable")
	}
	if count != len(entries) {
		return errors.New("storefront migration version mismatch")
	}
	for _, entry := range entries {
		b, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			return err
		}
		var hash string
		if err = s.DB.QueryRow(ctx, "SELECT checksum FROM storefront_migrations WHERE name=$1", entry.Name()).Scan(&hash); err != nil || !migrationMatches(entry.Name(), b, hash) {
			return fmt.Errorf("migration missing or changed: %s", entry.Name())
		}
	}
	return nil
}

// Runtime connections must not be migration owners or privileged cluster roles.
func (s *Store) VerifyRuntime(ctx context.Context) error {
	var unsafe bool
	err := s.DB.QueryRow(ctx, `SELECT r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls
 OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
 OR EXISTS(SELECT 1 FROM pg_database WHERE datdba=r.oid)
 OR EXISTS(SELECT 1 FROM pg_namespace WHERE nspowner=r.oid)
 OR EXISTS(SELECT 1 FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relowner=r.oid)
 OR has_schema_privilege(current_schema(),'CREATE')
 FROM pg_roles r WHERE rolname=current_user`).Scan(&unsafe)
	if err != nil {
		return errors.New("cannot verify runtime database privileges")
	}
	if unsafe {
		return errors.New("API and worker require a restricted runtime database role")
	}
	return s.Verify(ctx)
}

func (s *Store) GrantRuntime(ctx context.Context, role, password string) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{2,48}$`).MatchString(role) || len(password) < 32 || len(password) > 128 {
		return errors.New("invalid runtime role or password length (32-128 bytes required)")
	}
	if err := s.Verify(ctx); err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var schema, owner string
	if err = tx.QueryRow(ctx, "SELECT current_schema(),current_user").Scan(&schema, &owner); err != nil {
		return err
	}
	if role == owner {
		return errors.New("runtime role must differ from owner")
	}
	var unsafe bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles r WHERE rolname=$1 AND
 (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
 OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
 OR EXISTS(SELECT 1 FROM pg_database WHERE datdba=r.oid)
 OR EXISTS(SELECT 1 FROM pg_namespace WHERE nspowner=r.oid)
 OR EXISTS(SELECT 1 FROM pg_class WHERE relowner=r.oid)))`, role).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe {
		return errors.New("refusing to repurpose a privileged or object-owning role")
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('storefront.role',$1,true),set_config('storefront.password',$2,true)", role, password); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DO $$ DECLARE r text:=current_setting('storefront.role'); BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('CREATE ROLE %I',r); END IF;
 EXECUTE format('ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD %L',r,current_setting('storefront.password'));
 END $$`); err != nil {
		return err
	}
	sc, r := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()
	for _, sql := range []string{
		"REVOKE CREATE ON SCHEMA " + sc + " FROM PUBLIC",
		"GRANT USAGE ON SCHEMA " + sc + " TO " + r,
		"REVOKE ALL ON ALL TABLES IN SCHEMA " + sc + " FROM " + r,
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + sc + " TO " + r,
		"REVOKE ALL ON " + sc + ".legacy_imports," + sc + ".legacy_import_rows FROM " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".worker_heartbeats TO " + r,
		"GRANT INSERT,UPDATE,DELETE ON " + sc + ".auth_challenges," + sc + ".auth_sessions," + sc + ".auth_limits TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".admin_totp_usage," + sc + ".catalog_plans," + sc + ".partners," + sc + ".partner_prices," + sc + ".support_contacts," + sc + ".partner_announcements TO " + r,
		"GRANT INSERT ON " + sc + ".audit_events TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_order_versions," + sc + ".default_price_settings TO " + r,
		"GRANT INSERT,UPDATE,DELETE ON " + sc + ".partner_product_order TO " + r,
		"GRANT INSERT,UPDATE,DELETE ON " + sc + ".partner_domains TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_stores," + sc + ".store_prices," + sc + ".partner_coupons," + sc + ".domain_charges TO " + r,
		"GRANT INSERT,DELETE ON " + sc + ".coupon_targets TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_branding TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".crm_customers," + sc + ".crm_accounts TO " + r,
		"GRANT INSERT ON " + sc + ".crm_subscriptions TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".visitor_days TO " + r,
		"GRANT INSERT,DELETE ON " + sc + ".launch_interest TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".fulfillment_orders," + sc + ".vouchers," + sc + ".order_recovery_secrets," + sc + ".voucher_redemptions," + sc + ".voucher_dispatch TO " + r,
		"GRANT INSERT ON " + sc + ".voucher_batches," + sc + ".offline_receipts," + sc + ".offline_refunds," + sc + ".voucher_contexts," + sc + ".order_guest_access," + sc + ".voucher_events TO " + r,
		"GRANT INSERT,DELETE ON " + sc + ".fulfillment_nonces TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".orders," + sc + ".recharge_reminders TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_notification_settings," + sc + ".partner_order_notifications TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_payout_accounts TO " + r,
		"GRANT INSERT ON " + sc + ".order_events TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_tickets TO " + r,
		"GRANT INSERT ON " + sc + ".partner_ticket_events TO " + r,
		"GRANT UPDATE ON " + sc + ".partner_ticket_notifications TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".payments," + sc + ".payment_refund_attempts," + sc + ".payment_settings TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".refund_cases," + sc + ".partner_debits," + sc + ".partner_payouts TO " + r,
		"GRANT INSERT ON " + sc + ".refund_events," + sc + ".operation_receipts," + sc + ".partner_payout_items," + sc + ".partner_payout_debit_items TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".payout_notifications TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".accounting_costs," + sc + ".accounting_state TO " + r,
		"GRANT INSERT ON " + sc + ".accounting_settlements," + sc + ".accounting_items," + sc + ".invoice_events TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".partner_invoices," + sc + ".invoice_payments," + sc + ".email_deliveries TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".recharge_orders," + sc + ".recharge_redemptions," + sc + ".recharge_refunds TO " + r,
		"GRANT INSERT ON " + sc + ".recharge_webhooks," + sc + ".recharge_credentials TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".account_supplier_orders," + sc + ".delivery_supplier_orders TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".xgift_purchases TO " + r,
		"GRANT INSERT,DELETE ON " + sc + ".xgift_preflights," + sc + ".xgift_preflight_backends TO " + r,
		"GRANT INSERT ON " + sc + ".xgift_recovery_attempts TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".telegram_partner_applications," + sc + ".telegram_partner_sessions TO " + r,
		"GRANT INSERT,UPDATE,DELETE ON " + sc + ".telegram_partner_updates TO " + r,
		"GRANT INSERT,UPDATE ON " + sc + ".telegram_group_moderation TO " + r,
	} {
		if _, err = tx.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Heartbeat(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, "INSERT INTO worker_heartbeats(id) VALUES($1) ON CONFLICT(id) DO UPDATE SET updated_at=now()", id)
	return err
}

func (s *Store) WorkerReady(ctx context.Context, id string) bool {
	var ready bool
	err := s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM worker_heartbeats WHERE id=$1 AND updated_at>now()-interval '45 seconds')", id).Scan(&ready)
	return err == nil && ready
}
