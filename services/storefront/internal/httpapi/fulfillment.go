package httpapi

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"io"
	"net/http"
)

func registerFulfillment(r *gin.Engine, group *gin.RouterGroup, o orders.Service, s *fulfillment.Service, token func(*gin.Context, string) string) {
	group.POST("/orders/:id/delivery-link", func(c *gin.Context) {
		if s == nil {
			fail(c, fault.New(503, "充值交付服务尚未配置。"))
			return
		}
		value := token(c, "buyer")
		if !security.ValidToken(value) {
			fail(c, fault.New(401, "请先建立浏览器订单会话。"))
			return
		}
		a := &orders.Access{OwnerHash: buyerHash(c, value), ClientIP: trafficIP(c.ClientIP())}
		p, e := o.Auth.Require(c.Request.Context(), o.Store.DB, "customer", token(c, "customer"))
		if e == nil {
			a.Email = p.Subject
		} else if fault.Status(e) != 401 {
			fail(c, e)
			return
		}
		if e = s.Store.DB.Ping(c.Request.Context()); e != nil {
			fail(c, e)
			return
		}
		if e = o.Auth.Limit(c.Request.Context(), "delivery-link:"+a.OwnerHash, 30, 60); e != nil {
			fail(c, e)
			return
		}
		link, e := s.Ticket(c.Request.Context(), c.Param("id"), a)
		if e != nil {
			fail(c, e)
			return
		}
		c.JSON(200, gin.H{"url": link})
	})
	// Separate HMAC-only route: not part of browser-cookie authentication or CSRF group.
	r.POST("/api/internal/fulfillment/authorize", func(c *gin.Context) {
		if s == nil {
			fail(c, fault.New(503, "充值交付服务尚未配置。"))
			return
		}
		raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
		if e != nil {
			fail(c, fault.New(413, "请求过大。"))
			return
		}
		if e = s.Verify(c.Request.Context(), c.Request.Header, c.FullPath(), raw); e != nil {
			fail(c, e)
			return
		}
		var body struct {
			SalesOrderID string `json:"salesOrderId"`
			Phase        string `json:"phase"`
			ContextID    string `json:"contextId"`
		}
		if json.Unmarshal(raw, &body) != nil || body.SalesOrderID == "" || (body.Phase != "inspect" && body.Phase != "execute" && body.Phase != "preview") {
			fail(c, fault.New(400, "请求无效。"))
			return
		}
		ok, e := s.AuthorizeContext(c.Request.Context(), body.SalesOrderID, body.Phase == "execute", body.ContextID, body.Phase == "preview")
		if e != nil {
			fail(c, e)
			return
		}
		out := gin.H{"authorized": ok}
		if ok {
			var contextID, operationID, accountHash string
			e = s.Store.DB.QueryRow(c.Request.Context(), `SELECT context_id,operation_id,account_hash FROM voucher_redemptions WHERE order_id=$1`, body.SalesOrderID).Scan(&contextID, &operationID, &accountHash)
			if e == nil {
				out["contextId"] = contextID
				out["operationId"] = operationID
				out["accountHash"] = accountHash
				out["voucher"] = true
			}
		}
		c.JSON(200, out)
	})
}
