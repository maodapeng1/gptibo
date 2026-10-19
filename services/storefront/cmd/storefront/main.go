package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/config"
	"gptibo/storefront/internal/delivery"
	"gptibo/storefront/internal/domainbilling"
	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/internal/importer"
	"gptibo/storefront/internal/invoices"
	"gptibo/storefront/internal/mailer"
	"gptibo/storefront/internal/notifications"
	"gptibo/storefront/internal/refunds"
	"gptibo/storefront/internal/securityaudit"
	"gptibo/storefront/internal/store"
	"gptibo/storefront/internal/telegram"
	"gptibo/storefront/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		// Database errors can contain connection strings or SQL data: never print them.
		logger.Error("storefront command failed; check configuration, database and migration version")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	if len(os.Args) != 2 {
		return errors.New("usage: storefront api|worker|check-runtime|check-import-bindings|migrate|grant-runtime|health-api|health-worker|import-d1-check|import-d1-apply|import-d1-reconcile|telegram-configure")
	}
	mode := os.Args[1]
	switch mode {
	case "api", "worker", "check-runtime", "check-import-bindings", "migrate", "grant-runtime", "health-api", "health-worker", "import-d1-check", "import-d1-apply", "import-d1-reconcile", "telegram-configure":
	default:
		return errors.New("unknown command")
	}
	if mode == "health-api" {
		address := os.Getenv("HEALTH_URL")
		if address == "" {
			address = "http://127.0.0.1:8093/healthz"
		}
		client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.Get(address)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return errors.New("API not ready")
		}
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if mode == "telegram-configure" {
		client, err := config.LoadTelegram()
		if err != nil {
			return err
		}
		if client == nil {
			return errors.New("Telegram is not enabled")
		}
		origin := strings.TrimRight(os.Getenv("PUBLIC_ORIGIN"), "/")
		if origin != "https://shop.example.invalid" {
			return errors.New("production Telegram origin required")
		}
		configureCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		return client.ConfigureModeration(configureCtx, origin+"/api/telegram/webhook")
	}
	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.DB.Close()
	maintenance, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if mode == "migrate" {
		return s.Migrate(maintenance)
	}
	if mode == "import-d1-check" || mode == "import-d1-apply" || mode == "import-d1-reconcile" {
		importCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		var report importer.Report
		if mode == "import-d1-reconcile" {
			report, err = importer.Reconcile(importCtx, s, os.Getenv("IMPORT_SHA256"))
		} else {
			report, err = importer.Run(importCtx, s, os.Getenv("IMPORT_SNAPSHOT_FILE"), os.Getenv("IMPORT_SHA256"), mode == "import-d1-apply")
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	if mode == "grant-runtime" {
		password, err := config.Secret("APP_DB_PASSWORD")
		if err != nil {
			return err
		}
		role := os.Getenv("RUNTIME_DB_USER")
		if role == "" {
			role = "storefront_app"
		}
		return s.GrantRuntime(maintenance, role, password)
	}
	if err = s.VerifyRuntime(maintenance); err != nil {
		return err
	}
	if mode == "health-worker" {
		if !s.WorkerReady(maintenance, cfg.WorkerID) {
			return errors.New("worker not ready")
		}
		return nil
	}
	paymentService, err := config.LoadPayments()
	if err != nil {
		return err
	}
	paymentService.Store = s
	httpConfig, err := config.LoadHTTP()
	if err != nil {
		return err
	}
	mailSender, _ := httpConfig.Auth.Sender.(mailer.Sender)
	fulfillmentService, err := config.LoadFulfillment()
	if err != nil {
		return err
	}
	if fulfillmentService != nil {
		fulfillmentService.Store = s
	}
	rechargeService, err := config.LoadRecharge()
	if err != nil {
		return err
	}
	if rechargeService != nil {
		rechargeService.Store = s
		rechargeService.CredentialSecret = httpConfig.Auth.Secret
	}
	procurementService, err := config.LoadProcurement()
	if err != nil {
		return err
	}
	procurementService.Store = s
	procurementService.Delivery = delivery.Service{Store: s, Sender: mailSender, Origin: httpConfig.Origin}
	procurementService.PaymentReady = paymentService.Provider != nil && paymentService.Checkout && paymentService.NotifyOrigin != ""
	xgiftService, err := config.LoadXGift()
	if err != nil {
		return err
	}
	xgiftService.Store = s
	xgiftService.PaymentReady = procurementService.PaymentReady
	if mode == "check-import-bindings" {
		report, e := importer.CheckBindings(maintenance, s, paymentService, rechargeService, procurementService, xgiftService, os.Getenv("MAIL_FROM"))
		if e != nil {
			return e
		}
		if e = json.NewEncoder(os.Stdout).Encode(report); e != nil {
			return e
		}
		for name, count := range report {
			if strings.HasSuffix(name, "_mismatched") && count > 0 {
				return errors.New("imported channel bindings incompatible")
			}
		}
		return nil
	}
	telegramClient, err := config.LoadTelegram()
	if err != nil {
		return err
	}
	identity, err := auth.New(s, httpConfig.Auth)
	if err != nil {
		return err
	}
	domainService, err := config.LoadDomains()
	if err != nil {
		return err
	}
	if domainService != nil {
		domainService.Store = s
		if httpConfig.Turnstile != nil {
			domainService.VerificationReady = func(host string) error {
				_, err := httpConfig.Turnstile.SiteKey(host)
				return err
			}
		}
	}
	mailDomainsEnabled := func() bool { return domainService != nil && domainService.Enabled() }
	procurementService.Delivery.DomainsEnabled = mailDomainsEnabled
	if mode == "check-runtime" {
		return nil
	}
	var auditService *securityaudit.Service
	if httpConfig.SecurityAudit && !httpConfig.Rehearsal {
		auditService = &securityaudit.Service{Store: s, Auth: identity, Sender: mailSender, Recipient: httpConfig.SecurityAlertEmail}
	}
	if mode == "worker" {
		jobs := []worker.Job{}
		if auditService != nil {
			jobs = append(jobs, worker.Job{Name: "security-alerts", Interval: time.Minute, Timeout: 2 * time.Minute, Run: auditService.Alerts})
		}
		billing := domainbilling.Service{Store: s, Payments: paymentService, RenewEnabled: func() bool { return domainService != nil && domainService.Enabled() }}
		if domainService != nil {
			billing.MonthlyFeeCents = domainService.MonthlyFeeCents
		}
		if rechargeService != nil {
			billing.VerifySupplier = rechargeService.Verify
		}
		jobs = append(jobs, worker.Job{Name: "domain-billing", Interval: time.Minute, Timeout: 4 * time.Minute, Run: billing.Reconcile})
		if domainService != nil {
			jobs = append(jobs, worker.Job{Name: "custom-domains", Interval: time.Minute, Timeout: 4 * time.Minute, Run: domainService.Reconcile})
		}
		if telegramClient != nil {
			tg := telegram.Service{Store: s, Auth: identity, Client: telegramClient, Sender: mailSender, Origin: httpConfig.Origin}
			jobs = append(jobs, worker.Job{Name: "telegram-invitations", Interval: time.Minute, Timeout: 4 * time.Minute, Run: tg.Invitations})
		}
		notificationSender, e := config.LoadNotifications(httpConfig.Origin)
		if e != nil {
			return e
		}
		if notificationSender != nil {
			n := notifications.Service{Store: s, Sender: notificationSender}
			jobs = append(jobs, worker.Job{Name: "payout-notifications", Interval: 5 * time.Minute, Timeout: 2 * time.Minute, Run: n.Reconcile})
		}
		if len(xgiftService.Clients) > 0 {
			jobs = append(jobs, worker.Job{Name: "xgift", Wake: true, Interval: 5 * time.Second, Timeout: 4 * time.Minute, Run: xgiftService.Reconcile})
		}
		if procurementService.AccountSupplier != nil || procurementService.DeliverySupplier != nil {
			jobs = append(jobs, worker.Job{Name: "procurement", Wake: true, Interval: 5 * time.Second, Timeout: 4 * time.Minute, Run: procurementService.Reconcile})
		}
		if fulfillmentService != nil {
			jobs = append(jobs, worker.Job{Name: "fulfillment-sync", Interval: 15 * time.Second, Timeout: 45 * time.Second, Run: fulfillmentService.Reconcile})
		}
		if rechargeService != nil {
			jobs = append(jobs, worker.Job{Name: "recharge-active", Wake: true, Interval: 5 * time.Second, Timeout: 45 * time.Second, Run: rechargeService.ReconcileActive})
			jobs = append(jobs, worker.Job{Name: "recharge-orders", Interval: 5 * time.Minute, Timeout: 4 * time.Minute, Run: rechargeService.Reconcile})
			rf := refunds.Service{Store: s, Supplier: rechargeService}
			jobs = append(jobs, worker.Job{Name: "recharge-refunds", Wake: true, Interval: 15 * time.Second, Timeout: 4 * time.Minute, Run: rf.Reconcile})
			jobs = append(jobs, worker.Job{Name: "recharge-external-refunds", Interval: 30 * time.Second, Timeout: 4 * time.Minute, Run: rf.ReconcileExternal})
		}
		if paymentService.Provider != nil {
			jobs = append(jobs, worker.Job{Name: "payments", Interval: 5 * time.Minute, Timeout: 2 * time.Minute, Run: func(c context.Context) error { _, e := paymentService.Reconcile(c); return e }})
		}
		if paymentService.Provider != nil {
			invoiceService := invoices.Service{Store: s, Payments: &paymentService, Provider: paymentService.Provider}
			jobs = append(jobs, worker.Job{Name: "invoice-payments", Interval: 5 * time.Minute, Timeout: 2 * time.Minute, Run: func(c context.Context) error { _, e := invoiceService.Reconcile(c); return e }})
		}
		if mailSender != nil {
			deliveryService := delivery.Service{Store: s, Sender: mailSender, Origin: httpConfig.Origin, DomainsEnabled: mailDomainsEnabled}
			jobs = append(jobs, worker.Job{Name: "partner-order-emails", Wake: true, Interval: 15 * time.Second, Timeout: 4 * time.Minute, Run: func(c context.Context) error { _, e := deliveryService.PartnerOrderNotifications(c, 20); return e }})
			jobs = append(jobs, worker.Job{Name: "recharge-reminders", Interval: time.Minute, Timeout: 4 * time.Minute, Run: func(c context.Context) error { _, e := deliveryService.Reminders(c, 20); return e }})
		}
		return worker.Run(ctx, s, cfg.WorkerID, logger, jobs...)
	}
	gin.SetMode(gin.ReleaseMode)
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpapi.Router(s, cfg.WorkerID, logger, httpapi.Options{Fulfillment: fulfillmentService, AdminAccess: httpConfig.AdminAccess, SecurityAudit: auditService, Turnstile: httpConfig.Turnstile, AuthTurnstile: httpConfig.AuthTurnstile, OrderProtection: true, TrafficProtection: httpConfig.TrafficProtection, Domains: domainService, Rehearsal: httpConfig.Rehearsal, Origin: httpConfig.Origin, Auth: identity, TrustedProxies: httpConfig.TrustedProxies, OrderTesting: httpConfig.OrderTesting, Payments: &paymentService, PaymentTesting: httpConfig.PaymentTesting, MailSender: mailSender, AdminEmail: httpConfig.Auth.AdminEmail, Recharge: rechargeService, Procurement: procurementService, XGift: xgiftService, Telegram: telegramClient, LegacyBuyer: httpConfig.LegacyBuyer}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() {
		logger.Info("storefront API started", "address", cfg.ListenAddr)
		done <- server.ListenAndServe()
	}()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
	return nil
}
