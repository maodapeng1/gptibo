package httpapi

import (
	"context"
	"encoding/json"
	"gptibo/storefront/internal/domainbilling"
	"gptibo/storefront/internal/domains"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/delivery"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/invoices"
	"gptibo/storefront/internal/mailer"
	"gptibo/storefront/internal/management"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/procurement"
	"gptibo/storefront/internal/refunds"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/securityaudit"
	"gptibo/storefront/internal/store"
	"gptibo/storefront/internal/xgift"
)

func fail(c *gin.Context, e error) {
	if fault.Status(e) == 429 {
		c.Header("Retry-After", "60")
	}
	c.AbortWithStatusJSON(fault.Status(e), gin.H{"error": fault.Message(e)})
}
func read(c *gin.Context, v any) bool {
	return readLimit(c, v, 32768)
}
func readLimit(c *gin.Context, v any, limit int64) bool {
	typ, _, e := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if e != nil || typ != "application/json" {
		fail(c, fault.New(415, "请提交 JSON 数据。"))
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(v); e != nil {
		fail(c, fault.New(400, "请求内容无效或过大。"))
		return false
	}
	if e = decoder.Decode(new(any)); e != io.EOF {
		fail(c, fault.New(400, "只能提交一个 JSON 对象。"))
		return false
	}
	return true
}
func page(c *gin.Context) int {
	p, e := strconv.Atoi(c.DefaultQuery("page", "1"))
	if e != nil {
		return 0
	}
	return p
}

func register(r *gin.Engine, s *store.Store, opts Options) {
	a := opts.Auth
	// Only the public origin or a domain resolved by trusted ingress can select a widget.
	verificationHost := func(ctx context.Context) string {
		if host := domains.Current(ctx).Host; host != "" {
			return host
		}
		origin, _ := url.Parse(opts.Origin)
		if origin == nil {
			return ""
		}
		return origin.Hostname()
	}
	m := management.Service{Store: s, Auth: a, Domains: opts.Domains, Origin: opts.Origin, DomainsEnabled: func() bool { return opts.Domains != nil && opts.Domains.Enabled() }}
	if opts.Recharge != nil {
		m.VerifySupplier = opts.Recharge.Verify
	}
	secure := strings.HasPrefix(opts.Origin, "https://")
	name := func(role string) string {
		if secure {
			return "__Host-sf_" + role
		}
		return "sf_" + role
	}
	token := func(c *gin.Context, role string) string {
		v, _ := c.Cookie(name(role))
		if opts.LegacyBuyer && role == "buyer" && !security.ValidToken(v) {
			legacy := "ymx_buyer_dev"
			if secure {
				legacy = "__Host-ymx_buyer"
			}
			old, _ := c.Cookie(legacy)
			if security.ValidToken(old) {
				v = old
			}
		}
		return v
	}
	cookie := func(c *gin.Context, role, value string, age int) {
		http.SetCookie(c.Writer, &http.Cookie{Name: name(role), Value: value, Path: "/", MaxAge: age, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	}
	guard := func(c *gin.Context) {
		timeout := 12 * time.Second
		if c.FullPath() == "/api/admin/login/email-code" || c.FullPath() == "/api/partner/auth/send" || c.FullPath() == "/api/orders/recovery/send" {
			timeout = 25 * time.Second
		}
		// Order preparation and fulfillment can make several bounded supplier calls.
		// Leave time for the whole chain, not just its first 10-second request.
		if c.Request.Method == http.MethodPost && (c.FullPath() == "/api/orders" || c.FullPath() == "/api/orders/:id/fulfillment" || c.FullPath() == "/api/orders/:id/delivery-link" || c.FullPath() == "/api/vouchers/preview" || c.FullPath() == "/api/admin/vouchers/:id/actions") || strings.Contains(c.FullPath(), "/xgift/") || c.FullPath() == "/api/admin/orders/:id/actions" || c.FullPath() == "/api/admin/recharge/reconcile" {
			timeout = 45 * time.Second
		}
		if c.FullPath() == "/api/orders/:id/session-precheck" || c.FullPath() == "/api/admin/orders/:id/recharge-retry" {
			timeout = 55 * time.Second
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.GetHeader("Origin") != domainOrigin(c, opts.Origin) {
			fail(c, fault.New(403, "请从本站提交操作。"))
			return
		}
		c.Next()
	}
	group := r.Group("/api")
	group.Use(guard)
	send := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var in auth.SendInput
			if !read(c, &in) {
				return
			}
			if role == "admin" {
				c.Set("audit-account", "***")
			} else {
				c.Set("audit-account", securityaudit.Mask(in.Email))
			}
			if opts.AuthTurnstile {
				if e := opts.Turnstile.VerifyAction(c.Request.Context(), verificationHost(c.Request.Context()), in.TurnstileToken, role+"_send"); e != nil {
					fail(c, e)
					return
				}
			}
			sendCtx := c.Request.Context()
			if role != "admin" {
				scope := domains.Current(sendCtx)
				slug := scope.Slug
				if role == "customer" && scope.Host == "" {
					slug = in.Shop
				}
				if slug != "" {
					brands := mailer.Brands{DB: s.DB, Origin: opts.Origin}
					brand, err := brands.Slug(sendCtx, slug)
					if err != nil {
						fail(c, fault.New(404, "商店暂不可用。"))
						return
					}
					// Verification codes must link back to the same session/domain scope.
					origin := domainOrigin(c, opts.Origin)
					brand.WorkspaceURL = origin + "/join"
					path := "/s/" + url.PathEscape(slug)
					if regexp.MustCompile(`^(?:[A-Z0-9]{6}|[A-Z]{8})$`).MatchString(slug) {
						path = "/r/" + url.PathEscape(slug)
					}
					brand.OrdersURL = origin + path + "/orders"
					sendCtx = mailer.WithBrand(sendCtx, brand)
				}
			}
			id, e := a.Send(sendCtx, role, in, c.ClientIP())
			if e != nil {
				fail(c, e)
				return
			}
			c.JSON(200, gin.H{"challenge": id, "retryAfter": 60, "message": "若该账号可用，验证码将发送至对应邮箱。"})
		}
	}
	verify := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var in auth.VerifyInput
			if !read(c, &in) {
				return
			}
			if role == "admin" {
				c.Set("audit-account", "***")
			} else {
				c.Set("audit-account", securityaudit.Mask(in.Email))
			}
			if opts.AuthTurnstile {
				if e := opts.Turnstile.VerifyAction(c.Request.Context(), verificationHost(c.Request.Context()), in.TurnstileToken, role+"_verify"); e != nil {
					fail(c, e)
					return
				}
			}
			value, e := a.Verify(c.Request.Context(), role, in, c.ClientIP())
			if e != nil {
				fail(c, e)
				return
			}
			c.Set("audit-session", a.Fingerprint("session", value))
			c.Set("audit-role", role)
			cookie(c, role, value, 3*24*3600)
			c.JSON(200, gin.H{"ok": true})
		}
	}
	logout := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var in struct{}
			if !read(c, &in) {
				return
			}
			if e := a.Logout(c.Request.Context(), role, token(c, role)); e != nil {
				fail(c, e)
				return
			}
			cookie(c, role, "", -1)
			c.JSON(200, gin.H{"ok": true})
		}
	}
	session := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			p, e := a.Require(c.Request.Context(), s.DB, role, token(c, role))
			if e != nil && fault.Status(e) != 401 {
				fail(c, e)
				return
			}
			if role == "customer" {
				var email any
				if e == nil {
					email = p.Subject
				}
				c.JSON(200, gin.H{"email": email, "mailReady": a.Ready()})
				return
			}
			out := gin.H{"authenticated": e == nil, "mailReady": a.Ready()}
			if role == "admin" {
				out["configured"] = a.AdminConfigured()
				out["passwordReady"] = a.PasswordReady()
				if e == nil {
					out["username"] = a.Username()
				}
			} else if e == nil {
				partner, err := m.Partner(c.Request.Context(), p.Subject)
				if err != nil {
					fail(c, err)
					return
				}
				out["name"] = partner["name"]
			}
			c.JSON(200, out)
		}
	}
	group.GET("/admin/session", session("admin"))
	group.POST("/admin/login/email-code", send("admin"))
	group.POST("/admin/login", verify("admin"))
	group.POST("/admin/logout", logout("admin"))
	group.POST("/admin/password-login", func(c *gin.Context) {
		var in struct {
			Username       string `json:"username"`
			Password       string `json:"password"`
			Code           string `json:"code"`
			TurnstileToken string `json:"turnstileToken"`
		}
		if !read(c, &in) {
			return
		}
		if opts.Rehearsal {
			fail(c, fault.New(503, "维护模式暂不开放管理员登录。"))
			return
		}
		if opts.AuthTurnstile {
			if e := opts.Turnstile.VerifyAction(c.Request.Context(), verificationHost(c.Request.Context()), in.TurnstileToken, "admin_verify"); e != nil {
				fail(c, e)
				return
			}
		}
		value, e := a.PasswordLogin(c.Request.Context(), in.Username, in.Password, in.Code, c.ClientIP())
		if e != nil {
			fail(c, e)
			return
		}
		c.Set("audit-role", "admin")
		c.Set("audit-session", a.Fingerprint("session", value))
		cookie(c, "admin", value, 3*24*3600)
		c.JSON(200, gin.H{"ok": true})
	})

	group.GET("/partner/auth/session", session("partner"))
	group.POST("/partner/auth/send", send("partner"))
	group.POST("/partner/auth/verify", verify("partner"))
	group.POST("/partner/auth/logout", logout("partner"))
	group.GET("/orders/recovery/session", session("customer"))
	group.POST("/orders/recovery/send", send("customer"))
	group.POST("/orders/recovery/verify", verify("customer"))
	group.POST("/orders/recovery/logout", logout("customer"))
	group.POST("/orders/client", func(c *gin.Context) {
		var in struct{}
		if !read(c, &in) {
			return
		}
		value := token(c, "buyer")
		if !security.ValidToken(value) {
			value = security.Token()
		}
		cookie(c, "buyer", value, 365*24*3600)
		c.JSON(200, gin.H{"ok": true})
	})
	require := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			p, e := a.Require(c.Request.Context(), s.DB, role, token(c, role))
			if e != nil {
				fail(c, e)
				return
			}
			c.Set("principal", p)
			c.Next()
		}
	}
	admin := group.Group("/admin")
	admin.Use(require("admin"))
	admin.GET("/security", func(c *gin.Context) {
		if opts.SecurityAudit == nil {
			c.JSON(200, gin.H{"enabled": false, "events": []any{}})
			return
		}
		events, e := opts.SecurityAudit.Recent(c.Request.Context())
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"enabled": true, "events": events})
	})
	partner := group.Group("/partner")
	partner.Use(require("partner"))
	registerDomains(admin, partner, opts)
	own := func(c *gin.Context) string { return c.MustGet("principal").(auth.Principal).Subject }
	admin.GET("/plans", func(c *gin.Context) {
		out, e := m.Plans(c.Request.Context())
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"plans": out})
	})
	savePlan := func(create bool) gin.HandlerFunc {
		return func(c *gin.Context) {
			var in management.PlanInput
			if !read(c, &in) {
				return
			}
			if e := m.SavePlan(c.Request.Context(), token(c, "admin"), c.Param("id"), in, create); e != nil {
				fail(c, e)
				return
			}
			c.JSON(200, gin.H{"ok": true})
		}
	}
	admin.POST("/plans", savePlan(true))
	admin.POST("/plans/:id", savePlan(false))
	admin.GET("/partners", func(c *gin.Context) {
		out, total, e := m.Partners(c.Request.Context(), page(c), c.Query("q"))
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"rows": out, "total": total, "page": page(c), "mailReady": a.Ready()})
	})
	admin.POST("/partners", func(c *gin.Context) {
		var in struct {
			Email string `json:"email"`
		}
		if !read(c, &in) {
			return
		}
		id, slug, e := m.CreatePartner(c.Request.Context(), token(c, "admin"), in.Email)
		if e != nil {
			fail(c, e)
			return
		}
		sent := m.Invite(c.Request.Context(), token(c, "admin"), id, opts.Origin) == nil
		message := "代理已开通，但邀请邮件发送未确认。可在详情中重发。"
		if sent {
			message = "代理已开通，邀请邮件已提交发送。请通过工作台获取登录验证码。"
		}
		c.JSON(200, gin.H{"ok": true, "id": id, "slug": slug, "invitationSent": sent, "message": message})
	})
	admin.POST("/partners/:id/invitation", func(c *gin.Context) {
		var in struct{}
		if !read(c, &in) {
			return
		}
		if e := m.Invite(c.Request.Context(), token(c, "admin"), c.Param("id"), opts.Origin); e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"ok": true, "message": "邀请邮件已提交发送。"})
	})
	admin.GET("/partners/:id", func(c *gin.Context) {
		out, e := m.Dashboard(c.Request.Context(), c.Param("id"), true)
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, out)
	})
	admin.POST("/partners/:id", func(c *gin.Context) {
		var in management.PartnerInput
		if !read(c, &in) {
			return
		}
		if e := m.SavePartner(c.Request.Context(), token(c, "admin"), c.Param("id"), in); e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	partner.GET("/plans", func(c *gin.Context) {
		plans, e := m.PartnerPlans(c.Request.Context(), own(c))
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"plans": plans})
	})
	price := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var in management.PriceInput
			if !read(c, &in) {
				return
			}
			if e := m.Price(c.Request.Context(), role, token(c, role), c.Param("id"), c.Param("plan"), in); e != nil {
				fail(c, e)
				return
			}
			c.JSON(200, gin.H{"ok": true})
		}
	}
	admin.POST("/partners/:id/prices/:plan", price("admin"))
	partner.POST("/prices/:plan", price("partner"))
	contacts := func(role string) gin.HandlerFunc {
		return func(c *gin.Context) {
			owner := "platform"
			if role == "partner" {
				owner = own(c)
			}
			if c.Request.Method == "POST" {
				var in management.ContactInput
				if !read(c, &in) {
					return
				}
				if e := m.SaveContacts(c.Request.Context(), role, token(c, role), in); e != nil {
					fail(c, e)
					return
				}
			}
			out, e := m.Contacts(c.Request.Context(), owner)
			if e != nil {
				fail(c, e)
				return
			}
			c.JSON(200, out)
		}
	}
	admin.GET("/support-contacts", contacts("admin"))
	admin.POST("/support-contacts", contacts("admin"))
	partner.GET("/support-contacts", contacts("partner"))
	partner.POST("/support-contacts", contacts("partner"))
	announcement := func(c *gin.Context) {
		if c.Request.Method == "POST" {
			var in management.Announcement
			if !read(c, &in) {
				return
			}
			if e := m.SaveAnnouncement(c.Request.Context(), token(c, "partner"), in); e != nil {
				fail(c, e)
				return
			}
		}
		out, e := m.Announcement(c.Request.Context(), own(c))
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, out)
	}
	partner.GET("/announcement", announcement)
	partner.POST("/announcement", announcement)
	admin.GET("/audit", func(c *gin.Context) {
		out, e := m.Audit(c.Request.Context(), page(c))
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"rows": out, "page": page(c)})
	})
	registerWorkspace(admin, partner, m, token)
	registerHierarchy(admin, partner, m, token, opts.Origin)
	registerPayouts(admin, partner, m, token)
	registerAccounting(admin, m, token)
	registerDelivery(admin, delivery.Service{Store: s, Auth: a, Sender: opts.MailSender, Origin: opts.Origin, AdminEmail: opts.AdminEmail, DomainsEnabled: func() bool { return opts.Domains != nil && opts.Domains.Enabled() }}, token)
	registerCustomers(admin, partner, m, token)
	paymentService := payments.Service{Store: s}
	if opts.Payments != nil {
		paymentService = *opts.Payments
		paymentService.Store = s
	}
	registerPayments(r, group, admin, paymentService, a, token)
	var domainFee int64
	if opts.Domains != nil {
		domainFee = opts.Domains.MonthlyFeeCents
	}
	registerDomainBilling(r, admin, partner, domainbilling.Service{MonthlyFeeCents: domainFee, Store: s, Payments: paymentService, VerifySupplier: m.VerifySupplier, RenewEnabled: m.DomainsEnabled}, m, token)
	registerInvoices(r, admin, partner, invoices.Service{Store: s, Auth: a, Payments: &paymentService, Provider: paymentService.Provider, NotifyOrigin: paymentService.NotifyOrigin}, token)
	refundService := refunds.Service{Store: s, Auth: a, Payments: &paymentService, Testing: opts.OrderTesting}
	if opts.Recharge != nil {
		refundService.Supplier = opts.Recharge
	}
	registerRefunds(group, admin, refundService, token)
	registerPaymentTests(admin, paymentService, a, refundService, opts.PaymentTesting, token)
	registerAnalytics(group, admin, partner, m, a)
	orderService := orders.Service{Store: s, Auth: a, ProtectCreation: opts.OrderProtection, Testing: opts.OrderTesting, ReadOnly: opts.Rehearsal, PaymentReady: paymentService.Provider != nil}
	group.GET("/orders/verification", func(c *gin.Context) {
		if opts.Turnstile == nil || (c.Query("purpose") == "auth" && !opts.AuthTurnstile) {
			c.JSON(200, gin.H{"enabled": false})
			return
		}
		key, err := opts.Turnstile.SiteKey(verificationHost(c.Request.Context()))
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"enabled": true, "siteKey": key, "purpose": c.Query("purpose")})
	})
	if opts.Turnstile != nil {
		orderService.VerifyCreation = func(ctx context.Context, in orders.CreateInput) error {
			return opts.Turnstile.Verify(ctx, verificationHost(ctx), in.TurnstileToken)
		}
	}
	orderService.PaymentSnapshot = paymentService.Prepare
	if opts.Recharge != nil {
		orderService.LiveSnapshot = opts.Recharge.Snapshot
		orderService.LivePlan = func(plan string) bool { return opts.Recharge.Checkout && opts.Recharge.Products[plan] != "" }
		orderService.SupplierReady = opts.Recharge.Client != nil
		orderService.SessionPrecheckRequired = opts.Recharge.Checker != nil
		orderService.SupplierCheckout = opts.Recharge.Client != nil && opts.Recharge.Checkout && !opts.OrderTesting
	}
	if opts.Procurement != nil {
		previousPlan, previousSnapshot := orderService.LivePlan, orderService.LiveSnapshot
		orderService.LivePlan = func(plan string) bool {
			if procurement.Kind(plan) != "" {
				return opts.Procurement.Enabled(plan)
			}
			return previousPlan != nil && previousPlan(plan)
		}
		orderService.LiveSnapshot = func(ctx context.Context, tx pgx.Tx, o orders.Order, in orders.CreateInput) error {
			if procurement.Kind(o.Plan) != "" {
				return opts.Procurement.Snapshot(ctx, tx, o, in)
			}
			if previousSnapshot == nil {
				return orders.Conflict()
			}
			return previousSnapshot(ctx, tx, o, in)
		}
		registerProcurement(admin, opts.Procurement, token)
	}
	if opts.XGift != nil {
		previousPlan, previousSnapshot := orderService.LivePlan, orderService.LiveSnapshot
		orderService.LivePlan = func(plan string) bool {
			if xgift.IsPlan(plan) {
				return opts.XGift.Enabled(plan)
			}
			return previousPlan != nil && previousPlan(plan)
		}
		orderService.LiveSnapshot = func(ctx context.Context, tx pgx.Tx, o orders.Order, in orders.CreateInput) error {
			if xgift.IsPlan(o.Plan) {
				return opts.XGift.Snapshot(ctx, tx, o, in)
			}
			if previousSnapshot == nil {
				return orders.Conflict()
			}
			return previousSnapshot(ctx, tx, o, in)
		}
		orderService.Replay = opts.XGift.Replay
		orderService.RecoverGift = opts.XGift.Recover
		registerXGift(group, admin, opts.XGift, token)
	}
	if opts.Fulfillment != nil {
		previousPlan, previousSnapshot := orderService.LivePlan, orderService.LiveSnapshot
		orderService.LivePlan = func(plan string) bool {
			return opts.Fulfillment.Enabled(plan) || (previousPlan != nil && previousPlan(plan))
		}
		orderService.LiveSnapshot = func(ctx context.Context, tx pgx.Tx, o orders.Order, in orders.CreateInput) error {
			if opts.Fulfillment.Enabled(o.Plan) {
				return opts.Fulfillment.Snapshot(ctx, tx, o, in)
			}
			if previousSnapshot == nil {
				return orders.Conflict()
			}
			return previousSnapshot(ctx, tx, o, in)
		}
	}
	registerVouchers(r, group, admin, orderService, opts.Fulfillment, token)
	registerFulfillment(r, group, orderService, opts.Fulfillment, token)
	registerOrders(group, admin, orderService, token)
	registerRecharge(r, group, admin, orderService, opts.Recharge, token)
}
