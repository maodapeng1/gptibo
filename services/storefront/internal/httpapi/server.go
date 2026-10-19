package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"gptibo/storefront/internal/turnstile"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gptibo/storefront/internal/access"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/catalog"
	"gptibo/storefront/internal/delivery"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/mailer"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/procurement"
	"gptibo/storefront/internal/recharge"
	"gptibo/storefront/internal/securityaudit"
	"gptibo/storefront/internal/store"
	"gptibo/storefront/internal/telegram"
	"gptibo/storefront/internal/xgift"
)

type Options struct {
	Fulfillment       *fulfillment.Service
	AdminAccess       *access.Service
	SecurityAudit     *securityaudit.Service
	Turnstile         *turnstile.Service
	AuthTurnstile     bool
	OrderProtection   bool
	TrafficProtection bool
	Domains           *domains.Service
	Rehearsal         bool
	LegacyBuyer       bool
	Telegram          *telegram.Client
	XGift             *xgift.Service
	Procurement       *procurement.Service
	Recharge          *recharge.Service
	MailSender        mailer.Sender
	AdminEmail        string
	PaymentTesting    bool
	Payments          *payments.Service
	OrderTesting      bool
	Origin            string
	Auth              *auth.Service
	TrustedProxies    []string
}

// RouteInventory exposes only method/path metadata for offline frontend contract checks.
func RouteInventory() []map[string]string {
	gin.SetMode(gin.ReleaseMode)
	r := Router(&store.Store{}, "inventory", slog.New(slog.NewTextHandler(io.Discard, nil))).(*gin.Engine)
	out := []map[string]string{}
	for _, route := range r.Routes() {
		out = append(out, map[string]string{"method": route.Method, "path": route.Path})
	}
	return out
}

func Router(s *store.Store, workerID string, logger *slog.Logger, options ...Options) http.Handler {
	opts := Options{Origin: "http://127.0.0.1:8093"}
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.Auth == nil {
		opts.Auth, _ = auth.New(s, auth.Config{})
	}
	pc := procurement.Service{}
	if opts.Procurement != nil {
		pc = *opts.Procurement
	}
	pc.Store = s
	pc.Auth = opts.Auth
	pc.Delivery = delivery.Service{Store: s, Auth: opts.Auth, Sender: opts.MailSender, Origin: opts.Origin}
	pc.PaymentReady = opts.Payments != nil && opts.Payments.Provider != nil && opts.Payments.Checkout && opts.Payments.NotifyOrigin != ""
	if opts.OrderTesting {
		pc.EmailEnabled = false
	}
	opts.Procurement = &pc
	xg := xgift.Service{Backend: "legacy", Clients: map[string]*xgift.Client{}}
	if opts.XGift != nil {
		xg = *opts.XGift
	}
	xg.Store = s
	xg.Auth = opts.Auth
	xg.PaymentReady = pc.PaymentReady
	if opts.OrderTesting {
		xg.Checkout = false
	}
	opts.XGift = &xg
	r := gin.New()
	// Proxy trust is configured only when the ingress is chosen; default trusts nobody.
	if err := r.SetTrustedProxies(opts.TrustedProxies); err != nil {
		panic("invalid trusted proxy configuration")
	}
	r.HandleMethodNotAllowed = true
	r.Use(func(c *gin.Context) {
		start := time.Now()
		c.Request = c.Request.WithContext(payments.WithClientIP(c.Request.Context(), c.ClientIP()))
		id := rand.Text()
		c.Header("X-Request-ID", id)
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Header("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		}
		defer func() {
			if recover() != nil {
				c.AbortWithStatusJSON(500, gin.H{"error": "服务暂时不可用。"})
			}
			// Never log request bodies, cookies, raw paths or query strings.
			if c.Writer.Header().Get("X-Traffic-Protection") != "origin" {
				logger.Info("http request", "request_id", id, "method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		c.Next()
	})
	if opts.TrafficProtection {
		r.Use(trafficProtection(opts, logger))
	}
	r.Use(domainIngress(opts))
	if opts.SecurityAudit != nil {
		r.Use(securityAudit(opts.SecurityAudit, logger))
	}
	if opts.AdminAccess != nil {
		r.Use(func(c *gin.Context) {
			if c.Request.URL.Path == "/api/admin" || strings.HasPrefix(c.Request.URL.Path, "/api/admin/") {
				if e := opts.AdminAccess.Verify(c.Request.Context(), c.GetHeader("Cf-Access-Jwt-Assertion")); e != nil {
					fail(c, e)
					return
				}
			}
			c.Next()
		})
	}
	r.Use(apiLimit(opts.Auth))
	if opts.Rehearsal {
		r.Use(rehearsalGuard(opts))
	}
	r.GET("/livez", func(c *gin.Context) { c.JSON(200, gin.H{"alive": true}) })
	r.GET("/healthz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		ready := s.Verify(ctx) == nil
		workerReady := ready && s.WorkerReady(ctx, workerID)
		status := 200
		if !ready || !workerReady {
			status = 503
		}
		// Liveness cannot certify external payment or supplier availability.
		c.JSON(status, gin.H{"ready": ready && workerReady, "database_ready": ready, "worker_ready": workerReady})
	})
	r.GET("/api/internal/domain-route", func(c *gin.Context) {
		if storePartner(c, c.Query("partner")) == "!" {
			c.Status(404)
			return
		}
		c.JSON(200, gin.H{"ready": true})
	})
	completedOrders := catalog.NewCompletedOrderCache(s)
	r.GET("/api/catalog", func(c *gin.Context) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		cs := catalog.Service{Store: s, Testing: opts.OrderTesting, CompletedOrders: completedOrders}
		if opts.Recharge != nil {
			cs.Availability = opts.Recharge.Availability
		}
		if opts.Procurement != nil {
			cs.Availability = func(ctx context.Context, plans []catalog.Plan) []catalog.Plan {
				original := append([]catalog.Plan(nil), plans...)
				if opts.Recharge != nil {
					plans = opts.Recharge.Availability(ctx, plans)
				} else {
					for i := range plans {
						plans[i].Enabled = 0
					}
				}
				p := opts.Procurement.Availability(ctx, original)
				for i := range plans {
					if procurement.Kind(plans[i].ID) != "" {
						plans[i].Enabled = p[i].Enabled
					}
				}
				return plans
			}
		}
		previous := cs.Availability
		cs.Availability = func(ctx context.Context, plans []catalog.Plan) []catalog.Plan {
			original := append([]catalog.Plan(nil), plans...)
			if previous != nil {
				plans = previous(ctx, plans)
			}
			gift := opts.XGift.Availability(ctx, original)
			for i := range plans {
				if xgift.IsPlan(plans[i].ID) {
					plans[i].Enabled = gift[i].Enabled
				}
			}
			return plans
		}
		if opts.Fulfillment != nil {
			previousAvailability := cs.Availability
			cs.Availability = func(ctx context.Context, plans []catalog.Plan) []catalog.Plan {
				original := append([]catalog.Plan(nil), plans...)
				if previousAvailability != nil {
					plans = previousAvailability(ctx, plans)
				}
				for i := range plans {
					if opts.Fulfillment.Enabled(plans[i].ID) {
						plans[i].Enabled = original[i].Enabled
					}
				}
				return plans
			}
		}
		out, err := cs.Read(ctx, storePartner(c, c.Query("partner")))
		if errors.Is(err, catalog.ErrShopUnavailable) {
			c.JSON(404, gin.H{"error": "该代理商店暂不可用，请联系平台支持。"})
			return
		}
		if err != nil {
			c.JSON(503, gin.H{"error": "商品信息暂不可用，请稍后重试。"})
			return
		}
		out.PaymentMethods = []catalog.PaymentMethod{}
		if opts.Payments != nil && opts.Payments.Checkout && !opts.OrderTesting {
			ps := *opts.Payments
			ps.Store = s
			methods, err := ps.Methods(ctx, s.DB)
			if err != nil {
				fail(c, err)
				return
			}
			for _, method := range methods {
				out.PaymentMethods = append(out.PaymentMethods, catalog.PaymentMethod{Method: method.Method, Name: method.Name, FeeBPS: method.FeeBPS})
			}
		}
		c.Header("Server-Timing", fmt.Sprintf("catalog;dur=%.1f", float64(time.Since(start).Microseconds())/1000))
		c.JSON(200, out)
	})
	register(r, s, opts)
	registerTelegram(r, telegram.Service{Store: s, Auth: opts.Auth, Client: opts.Telegram, Sender: opts.MailSender, Origin: opts.Origin})
	r.NoRoute(func(c *gin.Context) { c.JSON(404, gin.H{"error": "未找到接口。"}) })
	r.NoMethod(func(c *gin.Context) { c.JSON(405, gin.H{"error": "请求方法不支持。"}) })
	return r
}
