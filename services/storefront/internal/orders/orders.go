package orders

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/store"
)

type Service struct {
	VerifyCreation          func(context.Context, CreateInput) error
	ProtectCreation         bool
	Store                   *store.Store
	Auth                    *auth.Service
	Testing                 bool
	ReadOnly                bool
	PaymentReady            bool
	LiveSnapshot            func(context.Context, pgx.Tx, Order, CreateInput) error
	PaymentSnapshot         func(context.Context, pgx.Tx, Order, CreateInput) error
	LivePlan                func(string) bool
	Sync                    func(context.Context, string, bool) error
	SupplierReady           bool
	SessionPrecheckRequired bool
	SupplierCheckout        bool
	Replay                  func(context.Context, Query, Order, CreateInput) error
	RecoverGift             func(context.Context, string, string, string, int64) error
}
type Access struct{ OwnerHash, Email, ClientIP string }
type Order struct {
	StoreID     *string `json:"store_id"`
	StoreName   *string `json:"store_name"`
	CouponCode  *string `json:"coupon_code"`
	ListPrice   *int64  `json:"list_price_cents"`
	Discount    int64   `json:"discount_cents"`
	ID          string  `json:"id"`
	OwnerHash   string  `json:"owner_hash"`
	Key         string  `json:"idempotency_key"`
	Plan        string  `json:"plan"`
	PlanName    string  `json:"plan_name"`
	Price       int64   `json:"price_cents"`
	Mode        string  `json:"mode"`
	Payment     string  `json:"payment_status"`
	Status      string  `json:"status"`
	Created     int64   `json:"created_at"`
	Updated     int64   `json:"updated_at"`
	Started     *int64  `json:"started_at"`
	Outcome     *string `json:"outcome"`
	Phone       *string `json:"sms_phone"`
	Version     int64   `json:"version"`
	PartnerID   *string `json:"partner_id"`
	PartnerName *string `json:"partner_name"`
	Cost        *int64  `json:"cost_cents"`
	Email       *string `json:"customer_email"`
	Refunded    int64   `json:"refunded_cents"`
	Hold        int     `json:"refund_hold"`
	Test        int     `json:"is_test"`
	Blocked     int     `json:"redemption_blocked"`
	Note        string  `json:"purchase_note"`
}
type Query interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func Get(ctx context.Context, q Query, id string, access *Access, lock bool) (Order, error) {
	var o Order
	var raw []byte
	sql := `SELECT to_jsonb(o) FROM orders o WHERE id=$1`
	args := []any{id}
	if access != nil {
		sql += ` AND is_test=0 AND (owner_hash=$2 OR customer_email=NULLIF($3,'') OR EXISTS(SELECT 1 FROM order_guest_access ga WHERE ga.order_id=o.id AND ga.buyer_hash=$2))`
		args = append(args, access.OwnerHash, access.Email)
		if scope := domains.Current(ctx); scope.PartnerID != "" {
			sql += ` AND partner_id=$4`
			args = append(args, scope.PartnerID)
		}
	}
	if lock {
		sql += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, sql, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, fault.New(404, "未找到订单或没有访问权限。")
	}
	if err != nil {
		return o, err
	}
	err = json.Unmarshal(raw, &o)
	return o, err
}
func List(ctx context.Context, q Query, sql string, args ...any) ([]Order, error) {
	rows, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		var raw []byte
		var o Order
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(raw, &o); e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func Conflict() error {
	return fault.New(409, "订单已更新或当前状态不支持该操作，请刷新后重试。")
}
func Change(ctx context.Context, tx pgx.Tx, o Order, next Order, actor, action, note string) (Order, error) {
	now := time.Now().UnixMilli()
	event := security.ID()
	tag, e := tx.Exec(ctx, `UPDATE orders SET status=$1,payment_status=$2,started_at=$3,outcome=$4,sms_phone=$5,redemption_blocked=$6,updated_at=$7,version=version+1,last_event_id=$8 WHERE id=$9 AND version=$10`, next.Status, next.Payment, next.Started, next.Outcome, next.Phone, next.Blocked, now, event, o.ID, o.Version)
	if e != nil {
		return o, e
	}
	if tag.RowsAffected() != 1 {
		return o, Conflict()
	}
	_, e = tx.Exec(ctx, `INSERT INTO order_events(id,order_id,actor,action,from_status,to_status,note,order_version,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, event, o.ID, actor, action, o.Status, next.Status, note, o.Version+1, now)
	if e != nil {
		return o, e
	}
	// NOTIFY is delivered only after commit; durable order state remains the queue.
	if next.Mode == "live" && next.Test == 0 && next.Payment == "paid" && next.Status == "session" && (o.Payment != "paid" || o.Status != "session") {
		if _, e = tx.Exec(ctx, `SELECT pg_notify('storefront_work',current_schema())`); e != nil {
			return o, e
		}
	}
	next.Version = o.Version + 1
	next.Updated = now
	return next, nil
}
func (s Service) Settle(ctx context.Context, id string, access *Access) (Order, error) {
	current, err := Get(ctx, s.Store.DB, id, access, false)
	if err != nil {
		return current, err
	}
	if s.ReadOnly || current.Mode != "demo" || current.Status != "processing" {
		return current, nil
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return Order{}, e
	}
	defer tx.Rollback(ctx)
	o, e := Get(ctx, tx, id, access, true)
	if e != nil {
		return o, e
	}
	if o.Mode == "demo" && o.Status == "processing" && o.Blocked == 0 && o.Hold == 0 && o.Refunded < o.Price && o.Started != nil && *o.Started <= time.Now().UnixMilli()-4500 {
		next := o
		next.Status = "success"
		if o.Outcome != nil && *o.Outcome == "failed" {
			next.Status = "failed"
		}
		o, e = Change(ctx, tx, o, next, "system", "demo-settle", "历史测试记录已结束，无支付或履约")
		if e != nil {
			return o, e
		}
	}
	return o, tx.Commit(ctx)
}
func (s Service) SettleDue(ctx context.Context) error {
	if s.ReadOnly {
		return nil
	}
	rows, e := List(ctx, s.Store.DB, `SELECT to_jsonb(o) FROM orders o WHERE mode='demo' AND status='processing' AND redemption_blocked=0 AND refund_hold=0 AND refunded_cents<price_cents AND started_at<=$1 ORDER BY started_at LIMIT 100`, time.Now().UnixMilli()-4500)
	if e != nil {
		return e
	}
	for _, o := range rows {
		if _, e = s.Settle(ctx, o.ID, nil); e != nil {
			return e
		}
	}
	return nil
}

type CreateInput struct {
	CouponCode               string `json:"couponCode"`
	ExpectedSupportsWarranty *bool  `json:"expectedSupportsWarranty"`
	TurnstileToken           string `json:"turnstileToken"`
	ExpectedFeeBPS           *int64 `json:"expectedFeeBps"`
	Plan                     string `json:"plan"`
	Key                      string `json:"key"`
	Email                    string `json:"email"`
	Partner                  string `json:"partner"`
	ExpectedPrice            int64  `json:"expectedPrice"`
	Username                 string `json:"username"`
	PreflightToken           string `json:"preflightToken"`
	PaymentMethod            string `json:"paymentMethod"`
}

func (s Service) Create(ctx context.Context, access Access, in CreateInput) (Order, bool, error) {
	var out Order
	if !security.ValidID(in.Key) || len(in.Plan) > 64 || in.Plan == "" {
		return out, false, fault.New(400, "商品或请求编号无效。")
	}
	email, e := auth.Email(in.Email)
	if e != nil {
		return out, false, e
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return out, false, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,9))`, access.OwnerHash+":"+in.Key); e != nil {
		return out, false, e
	}
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(903495)`); e != nil {
		return out, false, e
	}
	var partnerID, partnerName *string
	var storeID, storeName *string
	var upstreamID *string
	var upstreamCost *int64
	if in.Partner != "" {
		e = tx.QueryRow(ctx, `SELECT p.id,p.name,p.parent_id,s.id,s.name FROM partner_stores s JOIN partners p ON p.id=s.partner_id WHERE s.slug=$1 AND s.active AND p.active FOR SHARE OF p,s`, in.Partner).Scan(&partnerID, &partnerName, &upstreamID, &storeID, &storeName)
		if e == pgx.ErrNoRows {
			return out, false, fault.New(404, "代理商店不可用。")
		}
		if e != nil {
			return out, false, e
		}
	}
	var existing string
	e = tx.QueryRow(ctx, `SELECT id FROM orders WHERE owner_hash=$1 AND idempotency_key=$2`, access.OwnerHash, in.Key).Scan(&existing)
	if e == nil {
		out, e = Get(ctx, tx, existing, &access, false)
		if e != nil {
			return out, false, e
		}
		if out.Email == nil || *out.Email != email || out.Plan != in.Plan || !sameString(out.PartnerID, partnerID) || !(sameString(out.StoreID, storeID) || (out.StoreID == nil && out.PartnerID != nil && sameString(out.PartnerID, storeID))) || strings.ToUpper(strings.TrimSpace(in.CouponCode)) != couponCodeValue(out.CouponCode) {
			return out, false, Conflict()
		}
		if s.Replay != nil {
			if e = s.Replay(ctx, tx, out, in); e != nil {
				return out, false, e
			}
		}
		if s.PaymentSnapshot != nil {
			if e = s.PaymentSnapshot(ctx, tx, out, in); e != nil {
				return out, false, e
			}
		}
		return out, false, tx.Commit(ctx)
	}
	if e != pgx.ErrNoRows {
		return out, false, e
	}
	if upstreamID != nil {
		var active bool
		if e = tx.QueryRow(ctx, `SELECT active FROM partners WHERE id=$1 FOR SHARE`, upstreamID).Scan(&active); e != nil {
			return out, false, e
		}
		if !active {
			return out, false, fault.New(409, "上级代理已停用，商店暂不接单。")
		}
	}
	// The demo executor is deliberately limited to the legacy internal test products.
	if (s.Testing && !DemoPlan(in.Plan)) || (!s.Testing && (s.LiveSnapshot == nil || s.LivePlan == nil || !s.LivePlan(in.Plan))) {
		return out, false, fault.New(409, "该服务暂未开放购买，请稍后再来。")
	}
	var name, note, productID string
	var price int64
	var cost *int64
	var enabled, partnerEnabled, supportsWarranty bool
	e = tx.QueryRow(ctx, `SELECT name,price_cents,purchase_note,enabled,partner_enabled,supports_warranty,product_id FROM catalog_plans WHERE id=$1 FOR SHARE`, in.Plan).Scan(&name, &price, &note, &enabled, &partnerEnabled, &supportsWarranty, &productID)
	if e == pgx.ErrNoRows {
		return out, false, Conflict()
	}
	if e != nil {
		return out, false, e
	}
	if !enabled || (partnerID != nil && !partnerEnabled) {
		return out, false, Conflict()
	}
	if partnerID != nil {
		// Serialize visibility changes with new orders, including stores without settings yet.
		if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,4))`, *partnerID); e != nil {
			return out, false, e
		}
		var hidden bool
		if e = tx.QueryRow(ctx, `SELECT COALESCE((SELECT hidden_product_ids ? $2 FROM partner_order_versions WHERE partner_id=$1),false)`, *partnerID, productID).Scan(&hidden); e != nil {
			return out, false, e
		}
		if hidden && storeID != nil && *storeID == *partnerID {
			return out, false, fault.New(409, "该商品已被商店下架，请选择其他商品。")
		}
		e = tx.QueryRow(ctx, `SELECT COALESCE(sp.price_cents,p.price_cents),p.cost_cents FROM partner_prices p LEFT JOIN store_prices sp ON sp.store_id=$3 AND sp.plan=p.plan WHERE p.partner_id=$1 AND p.plan=$2 AND p.enabled AND COALESCE(sp.enabled,true) FOR SHARE OF p`, partnerID, in.Plan, storeID).Scan(&price, &cost)
		if e == pgx.ErrNoRows {
			return out, false, Conflict()
		}
		if e != nil {
			return out, false, e
		}
	}
	if upstreamID != nil {
		if e = tx.QueryRow(ctx, `SELECT cost_cents FROM partner_prices WHERE partner_id=$1 AND plan=$2 AND enabled FOR SHARE`, upstreamID, in.Plan).Scan(&upstreamCost); e == pgx.ErrNoRows {
			return out, false, Conflict()
		} else if e != nil {
			return out, false, e
		}
		if cost == nil || upstreamCost == nil || *cost < *upstreamCost || price < *cost {
			return out, false, Conflict()
		}
	}
	listPrice := price
	var couponID *string
	discount := int64(0)
	if in.CouponCode != "" {
		if partnerID == nil || storeID == nil || cost == nil {
			return out, false, fault.New(400, "优惠码仅适用于指定代理商店。")
		}
		id, d, err := couponPrice(ctx, tx, *partnerID, *storeID, in.Plan, email, in.CouponCode, price, *cost)
		if err != nil {
			return out, false, err
		}
		couponID = &id
		discount = d
		price -= discount
	}
	if cost != nil && price < *cost {
		return out, false, fault.New(409, "商品价格低于拿货价，暂不能下单。")
	}
	// Old clients implicitly expect support; never sell a newly excluded plan without disclosure.
	expectedWarranty := in.ExpectedSupportsWarranty == nil || *in.ExpectedSupportsWarranty
	if expectedWarranty != supportsWarranty {
		return out, false, fault.New(409, "商品售后质保说明已更新，请刷新页面并重新阅读下单须知。")
	}
	if price != in.ExpectedPrice {
		return out, false, fault.New(409, "套餐价格已更新，请刷新后重新确认。")
	}
	if s.ProtectCreation {
		if e = s.protectCreation(ctx, tx, access, email); e != nil {
			return out, false, e
		}
	}
	if s.VerifyCreation != nil {
		if e = s.VerifyCreation(ctx, in); e != nil {
			return out, false, e
		}
	}
	now := time.Now().UnixMilli()
	id := "YMX-" + strings.ToUpper(strings.ReplaceAll(security.ID(), "-", ""))
	event := security.ID()
	mode, createNote := "demo", "创建测试订单"
	if !s.Testing {
		mode, createNote = "live", "创建订单，等待付款"
	}
	_, e = tx.Exec(ctx, `INSERT INTO orders(id,owner_hash,idempotency_key,plan,plan_name,price_cents,mode,status,created_at,updated_at,last_event_id,customer_email,partner_id,partner_name,cost_cents,purchase_note,upstream_partner_id,upstream_cost_cents) VALUES($1,$2,$3,$4,$5,$6,$14,'pending',$7,$7,$8,$9,$10,$11,$12,$13,$15,$16)`, id, access.OwnerHash, in.Key, in.Plan, name, price, now, event, email, partnerID, partnerName, cost, note, mode, upstreamID, upstreamCost)
	if e != nil {
		return out, false, e
	}
	_, e = tx.Exec(ctx, `UPDATE orders SET store_id=$1,store_name=$2,coupon_id=$3,coupon_code=NULLIF($4,''),list_price_cents=$5,discount_cents=$6 WHERE id=$7`, storeID, storeName, couponID, strings.ToUpper(strings.TrimSpace(in.CouponCode)), listPrice, discount, id)
	if e != nil {
		return out, false, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO order_events(id,order_id,actor,action,to_status,note,order_version,created_at) VALUES($1,$2,'customer','create','pending',$4,0,$3)`, event, id, now, createNote)
	if e != nil {
		return out, false, e
	}
	out, e = Get(ctx, tx, id, &access, false)
	if e != nil {
		return out, false, e
	}
	if !s.Testing {
		if e = s.LiveSnapshot(ctx, tx, out, in); e != nil {
			return out, false, e
		}
		if s.PaymentSnapshot != nil {
			if e = s.PaymentSnapshot(ctx, tx, out, in); e != nil {
				return out, false, e
			}
		}
	}
	return out, true, tx.Commit(ctx)
}
func DemoPlan(plan string) bool {
	return plan == "plus" || plan == "pro-5x" || plan == "pro-20x" || plan == "pro-25x" || plan == "codex-sms-once"
}
func sameString(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

type ActionInput struct {
	Action  string `json:"action"`
	Version *int64 `json:"version"`
	Note    string `json:"note"`
	Blocked *bool  `json:"blocked"`
}

func (s Service) Action(ctx context.Context, id string, access *Access, adminToken string, in ActionInput) (Order, error) {
	var o Order
	if in.Version == nil || *in.Version < 0 {
		return o, fault.New(400, "请提供订单版本。")
	}
	admin := access == nil
	if admin && (strings.TrimSpace(in.Note) == "" || utf8.RuneCountInString(in.Note) > 500) {
		return o, fault.New(400, "请填写 1–500 字处理说明。")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return o, e
	}
	defer tx.Rollback(ctx)
	actor := "customer"
	if admin {
		p, e := s.Auth.Require(ctx, tx, "admin", adminToken)
		if e != nil {
			return o, e
		}
		actor = "admin:" + p.Subject
	}
	o, e = Get(ctx, tx, id, access, true)
	if e != nil {
		return o, e
	}
	if o.Version != *in.Version {
		return o, Conflict()
	}
	next := o
	if admin && in.Action == "redemption-control" {
		if in.Blocked == nil {
			return o, fault.New(400, "请选择有效兑换权限。")
		}
		n := 0
		if *in.Blocked {
			n = 1
		}
		if n == o.Blocked {
			return o, Conflict()
		}
		var busy bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recharge_orders WHERE order_id=$1 AND lease_until>$2 UNION ALL SELECT 1 FROM email_deliveries WHERE order_id=$1 AND lease_until>$2 UNION ALL SELECT 1 FROM account_supplier_orders WHERE order_id=$1 AND (lease_until>$2 OR state IN ('creating','unknown')) UNION ALL SELECT 1 FROM delivery_supplier_orders WHERE order_id=$1 AND (lease_until>$2 OR state IN ('creating','unknown','pending')) UNION ALL SELECT 1 FROM xgift_purchases WHERE order_id=$1 AND (lease_until>$2 OR state IN ('creating','unknown','pending')) UNION ALL SELECT 1 FROM payments WHERE order_id=$1 AND lease_until>$2)`, id, time.Now().UnixMilli()).Scan(&busy)
		if e != nil {
			return o, e
		}
		if busy {
			return o, Conflict()
		}
		next.Blocked = n
	} else if !(admin && in.Action == "note") {
		if o.Blocked != 0 || o.Hold != 0 || o.Refunded >= o.Price {
			return o, fault.New(409, "订单已禁止兑换、处于售后或已全额退款。")
		}
		if !admin && o.Mode != "demo" {
			return o, fault.New(403, "该订单不支持此操作。")
		}
		if !admin && !s.Testing && in.Action != "cancel" {
			return o, Conflict()
		}
		allowed := false
		if o.Mode == "demo" {
			switch in.Action {
			case "pay":
				if !admin && o.Status == "pending" {
					next.Status = "session"
					next.Payment = "simulated"
					allowed = true
				}
			case "cancel":
				if o.Status == "pending" {
					next.Status = "cancelled"
					allowed = true
				}
			case "acquire-phone":
				if !admin && o.Plan == "codex-sms-once" && o.Status == "session" && o.Phone == nil {
					phone := "+1 202-555-0147"
					next.Phone = &phone
					allowed = true
				}
			case "submit-success", "submit-failed":
				if !admin && o.Status == "session" && (o.Plan != "codex-sms-once" || o.Phone != nil) {
					now := time.Now().UnixMilli()
					outcome := strings.TrimPrefix(in.Action, "submit-")
					next.Status = "processing"
					next.Started = &now
					next.Outcome = &outcome
					allowed = true
				}
			case "retry":
				if o.Status == "failed" {
					next.Status = "session"
					next.Started = nil
					next.Outcome = nil
					allowed = true
				}
			}
		}
		if admin && o.Mode == "live" && o.Test == 0 && o.Plan == "pro-25x" && o.Payment == "paid" {
			var supplier bool
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recharge_orders WHERE order_id=$1)`, id).Scan(&supplier)
			if e != nil {
				return o, e
			}
			if !supplier {
				if in.Action == "manual-start" && o.Status == "session" {
					now := time.Now().UnixMilli()
					next.Status = "processing"
					next.Started = &now
					allowed = true
				}
				if in.Action == "manual-complete" && (o.Status == "session" || o.Status == "processing") {
					next.Status = "success"
					allowed = true
				}
			}
		}
		if !allowed {
			return o, Conflict()
		}
	}
	note := strings.TrimSpace(in.Note)
	if !admin {
		note = "内部测试操作（无实际收款或履约）：" + in.Action
	}
	next, e = Change(ctx, tx, o, next, actor, in.Action, note)
	if e != nil {
		return o, e
	}
	return next, tx.Commit(ctx)
}

// SupplyCost is the platform's agreed wholesale price, before either reseller's margin.
// It stays internal; customer and child-partner DTOs never expose the upstream price.
func SupplyCost(ctx context.Context, q Query, id string) (*int64, error) {
	var cost *int64
	e := q.QueryRow(ctx, `SELECT COALESCE(upstream_cost_cents,cost_cents) FROM orders WHERE id=$1`, id).Scan(&cost)
	return cost, e
}

func couponCodeValue(code *string) string {
	if code == nil {
		return ""
	}
	return *code
}
