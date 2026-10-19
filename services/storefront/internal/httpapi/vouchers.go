package httpapi

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/vouchers"
	"io"
	"net/http"
	"strings"
)

func registerVouchers(r *gin.Engine, group, admin *gin.RouterGroup, o orders.Service, delivery *fulfillment.Service, token func(*gin.Context, string) string) {
	s := vouchers.Service{Store: o.Store, Auth: o.Auth, Delivery: delivery}
	buyer := func(c *gin.Context) (string, bool) {
		value := token(c, "buyer")
		if !security.ValidToken(value) {
			fail(c, fault.New(401, "请先建立浏览器订单会话。"))
			return "", false
		}
		return buyerHash(c, value), true
	}
	reply := func(c *gin.Context, v any, e error) {
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, v)
	}
	admin.GET("/vouchers", func(c *gin.Context) {
		v, e := s.List(c.Request.Context(), c.Query("q"), c.Query("state"))
		reply(c, gin.H{"vouchers": v}, e)
	})
	admin.POST("/voucher-batches", func(c *gin.Context) {
		var in vouchers.BatchInput
		if !read(c, &in) {
			return
		}
		v, e := s.Batch(c.Request.Context(), c.MustGet("principal").(auth.Principal).Subject, in)
		reply(c, v, e)
	})
	admin.POST("/voucher-sales", func(c *gin.Context) {
		var in vouchers.SaleInput
		if !read(c, &in) {
			return
		}
		v, e := s.Sell(c.Request.Context(), c.MustGet("principal").(auth.Principal).Subject, in)
		reply(c, v, e)
	})
	admin.POST("/vouchers/:id/actions", func(c *gin.Context) {
		var in vouchers.ActionInput
		if !read(c, &in) {
			return
		}
		v, e := s.Action(c.Request.Context(), c.MustGet("principal").(auth.Principal).Subject, c.Param("id"), in)
		reply(c, v, e)
	})
	admin.GET("/vouchers/:id/events", func(c *gin.Context) {
		v, e := orders.Rows(c.Request.Context(), o.Store.DB, `SELECT actor,action,note,created_at FROM voucher_events WHERE voucher_id=$1 ORDER BY created_at DESC LIMIT 100`, c.Param("id"))
		reply(c, gin.H{"events": v}, e)
	})
	group.POST("/vouchers/preview", func(c *gin.Context) {
		b, ok := buyer(c)
		if !ok {
			return
		}
		if e := o.Auth.Limit(c.Request.Context(), "voucher-preview:"+trafficIP(c.ClientIP()), 20, 900); e != nil {
			fail(c, e)
			return
		}
		var in struct {
			Code string `json:"code"`
		}
		if !read(c, &in) {
			return
		}
		v, e := s.Preview(c.Request.Context(), b, in.Code)
		reply(c, v, e)
	})
	group.POST("/orders/recover-by-secret", func(c *gin.Context) {
		b, ok := buyer(c)
		if !ok {
			return
		}
		if e := o.Auth.Limit(c.Request.Context(), "voucher-recover:"+trafficIP(c.ClientIP()), 15, 900); e != nil {
			fail(c, e)
			return
		}
		var in struct {
			OrderID string `json:"orderId"`
			Secret  string `json:"secret"`
		}
		if !read(c, &in) {
			return
		}
		e := s.Recover(c.Request.Context(), strings.TrimSpace(in.OrderID), b, strings.TrimSpace(in.Secret))
		reply(c, gin.H{"ok": e == nil}, e)
	})
	group.POST("/orders/:id/recovery-secret", func(c *gin.Context) {
		b, ok := buyer(c)
		if !ok {
			return
		}
		var in struct{}
		if !read(c, &in) {
			return
		}
		if e := o.Auth.Limit(c.Request.Context(), "voucher-secret:"+b, 10, 900); e != nil {
			fail(c, e)
			return
		}
		secret, e := s.Recovery(c.Request.Context(), c.Param("id"), b)
		reply(c, gin.H{"secret": secret}, e)
	})
	r.POST("/api/internal/voucher-redemptions/confirm", func(c *gin.Context) {
		if delivery == nil {
			fail(c, fault.New(503, "交付服务未配置。"))
			return
		}
		raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
		if e != nil {
			fail(c, fault.New(413, "请求过大。"))
			return
		}
		if e = delivery.Verify(c.Request.Context(), c.Request.Header, c.FullPath(), raw); e != nil {
			fail(c, e)
			return
		}
		var in vouchers.ConfirmInput
		if json.Unmarshal(raw, &in) != nil {
			fail(c, fault.New(400, "请求无效。"))
			return
		}
		v, e := s.Confirm(c.Request.Context(), in)
		reply(c, v, e)
	})
}
