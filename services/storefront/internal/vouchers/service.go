package vouchers

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/store"
)

type Service struct {
	Store    *store.Store
	Auth     *auth.Service
	Delivery *fulfillment.Service
}
type BatchInput struct {
	Count     int    `json:"count"`
	ValidDays int    `json:"validDays"`
	Title     string `json:"title"`
}
type SaleInput struct {
	Confirmed bool   `json:"confirmed"`
	VoucherID string `json:"voucherId"`
	Version   int64  `json:"version"`
	Amount    int64  `json:"amountCents"`
	Channel   string `json:"channel"`
	Account   string `json:"account"`
	Reference string `json:"reference"`
	Note      string `json:"note"`
}
type ConfirmInput struct {
	ContextID   string `json:"contextId"`
	OrderID     string `json:"salesOrderId"`
	OperationID string `json:"operationId"`
	AccountHash string `json:"accountHash"`
}

func normalize(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}
func (s Service) hash(code string) string { return s.Auth.Fingerprint("voucher", normalize(code)) }
func event(ctx context.Context, tx pgx.Tx, id, actor, action, note string) error {
	_, e := tx.Exec(ctx, `INSERT INTO voucher_events VALUES($1,$2,$3,$4,$5,$6)`, security.ID(), id, actor, action, note, time.Now().UnixMilli())
	return e
}
func (s Service) enabled() error {
	if !s.Delivery.Enabled("plus") {
		return fault.New(503, "套餐卡密交付尚未配置。")
	}
	return nil
}
func (s Service) Batch(ctx context.Context, actor string, in BatchInput) (map[string]any, error) {
	if e := s.enabled(); e != nil {
		return nil, e
	}
	if in.Count < 1 || in.Count > 200 || in.ValidDays < 1 || in.ValidDays > 3650 || len(strings.TrimSpace(in.Title)) < 1 || len(in.Title) > 160 {
		return nil, fault.New(400, "批次数量 1–200，有效期 1–3650 天，请填写批次名称。")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	id, now := security.ID(), time.Now().UnixMilli()
	p := s.Delivery.Products["plus"]
	if _, e = tx.Exec(ctx, `INSERT INTO voucher_batches VALUES($1,$2,$3,$4,$5,$6,$7)`, id, strings.TrimSpace(in.Title), p.Plan, p.Region, in.ValidDays, actor, now); e != nil {
		return nil, e
	}
	codes := []map[string]any{}
	for i := 0; i < in.Count; i++ {
		raw := make([]byte, 20)
		if _, e = rand.Read(raw); e != nil {
			return nil, e
		}
		value := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		code := "AI-" + value[:8] + "-" + value[8:16] + "-" + value[16:24] + "-" + value[24:]
		vid := security.ID()
		mask := "AI-••••-" + value[24:]
		if _, e = tx.Exec(ctx, `INSERT INTO vouchers(id,batch_id,code_hash,mask,created_at) VALUES($1,$2,$3,$4,$5)`, vid, id, s.hash(code), mask, now); e != nil {
			return nil, e
		}
		if e = event(ctx, tx, vid, actor, "generate", "生成未激活库存"); e != nil {
			return nil, e
		}
		codes = append(codes, map[string]any{"id": vid, "code": code, "mask": mask})
	}
	return map[string]any{"batchId": id, "codes": codes}, tx.Commit(ctx)
}
func (s Service) List(ctx context.Context, query, status string) (any, error) {
	return orders.Rows(ctx, s.Store.DB, `SELECT v.id,v.batch_id,v.mask,v.state,v.version,v.sales_order_id,v.expires_at,b.title,b.valid_days,b.plan,b.region,f.status fulfillment_status,f.message,f.sync_error,COALESCE(f.remote_order_id,vr.operation_id) fulfillment_id,f.card_order_id,f.provider_operation_id,f.error_code,f.error_request_id,f.account_email_masked,r.amount_cents,r.channel,r.account_label,r.transaction_reference,r.note receipt_note,rf.amount_cents refunded_cents,rf.transaction_reference refund_reference FROM vouchers v JOIN voucher_batches b ON b.id=v.batch_id LEFT JOIN fulfillment_orders f ON f.order_id=v.sales_order_id LEFT JOIN voucher_redemptions vr ON vr.order_id=v.sales_order_id LEFT JOIN offline_receipts r ON r.order_id=v.sales_order_id LEFT JOIN offline_refunds rf ON rf.order_id=v.sales_order_id WHERE ($1='' OR strpos(lower(v.mask||' '||v.id||' '||COALESCE(v.sales_order_id,'')||' '||b.title),lower($1))>0) AND ($2='' OR v.state=$2) ORDER BY v.created_at DESC,v.id LIMIT 300`, strings.TrimSpace(query), status)
}
func (s Service) Sell(ctx context.Context, actor string, in SaleInput) (map[string]any, error) {
	if e := s.enabled(); e != nil {
		return nil, e
	}
	in.Reference = strings.ToUpper(strings.TrimSpace(in.Reference))
	in.Account = strings.ToLower(strings.TrimSpace(in.Account))
	in.Note = strings.TrimSpace(in.Note)
	if !in.Confirmed || in.Amount < 1 || in.Amount > 10000000 || !regexp.MustCompile(`^(wechat|alipay|bank)$`).MatchString(in.Channel) || in.Reference == "" || len(in.Reference) > 160 || in.Account == "" || len(in.Account) > 100 || in.Note == "" || len(in.Note) > 500 {
		return nil, fault.New(400, "请核实实收金额并填写渠道、收款账户标识、交易流水和处理说明。")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	var state, plan, region string
	var days int
	var version int64
	var sale *string
	e = tx.QueryRow(ctx, `SELECT v.state,v.version,v.sales_order_id,b.plan,b.region,b.valid_days FROM vouchers v JOIN voucher_batches b ON b.id=v.batch_id WHERE v.id=$1 FOR UPDATE OF v`, in.VoucherID).Scan(&state, &version, &sale, &plan, &region, &days)
	if e != nil {
		return nil, fault.New(404, "卡密不存在。")
	}
	if sale != nil {
		var equal bool
		e = tx.QueryRow(ctx, `SELECT channel=$2 AND account_label=$3 AND transaction_reference=$4 AND amount_cents=$5 FROM offline_receipts WHERE order_id=$1`, *sale, in.Channel, in.Account, in.Reference, in.Amount).Scan(&equal)
		if e == nil && equal {
			return map[string]any{"orderId": *sale, "alreadyActivated": true}, tx.Commit(ctx)
		}
		return nil, fault.New(409, "卡密已售出，收款信息不一致。")
	}
	if state != "inactive" || version != in.Version {
		return nil, orders.Conflict()
	}
	now := time.Now().UnixMilli()
	id := "YMX-" + strings.ToUpper(strings.ReplaceAll(security.ID(), "-", ""))
	rid := security.ID()
	_, e = tx.Exec(ctx, `INSERT INTO catalog_plans(id,product_id,name,price_cents,enabled) VALUES('plus','chatgpt','Plus',$1,false) ON CONFLICT(id) DO NOTHING`, in.Amount)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO orders(id,owner_hash,idempotency_key,plan,plan_name,price_cents,mode,status,payment_status,created_at,updated_at,last_event_id,purchase_note) VALUES($1,$2,$3,'plus','GPT Plus 卡密充值',$4,'live','session','paid',$5,$5,$6,'私域实收后激活；卡密确认后开始充值。')`, id, security.Hash(security.Token()), security.ID(), in.Amount, now, security.ID())
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO offline_receipts VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(channel,account_label,transaction_reference) DO NOTHING`, rid, id, in.Channel, in.Account, in.Reference, in.Amount, in.Note, actor, now)
	if e != nil {
		return nil, e
	}
	var saved string
	if e = tx.QueryRow(ctx, `SELECT order_id FROM offline_receipts WHERE id=$1`, rid).Scan(&saved); e != nil {
		return nil, fault.New(409, "该收款流水已登记，不能再次激活另一枚卡密。")
	}
	_, e = tx.Exec(ctx, `INSERT INTO fulfillment_orders(order_id,plan,region) VALUES($1,$2,$3)`, id, plan, region)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `UPDATE vouchers SET state='active',sales_order_id=$1,expires_at=$2,version=version+1 WHERE id=$3`, id, now+int64(days)*86400000, in.VoucherID)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO order_events(id,order_id,actor,action,to_status,note,order_version,created_at) SELECT last_event_id,id,$2,'manual-receipt','session',$3,version,$4 FROM orders WHERE id=$1`, id, "admin:"+actor, "已核实私域实际到账并激活卡密："+in.Note, now)
	if e != nil {
		return nil, e
	}
	if e = event(ctx, tx, in.VoucherID, actor, "activate", in.Note); e != nil {
		return nil, e
	}
	return map[string]any{"orderId": id}, tx.Commit(ctx)
}
func (s Service) Preview(ctx context.Context, buyer, code string) (map[string]any, error) {
	if e := s.enabled(); e != nil {
		return nil, e
	}
	if !regexp.MustCompile(`^AI[A-Z2-7]{32}$`).MatchString(normalize(code)) {
		return nil, fault.New(400, "请输入完整有效的卡密。")
	}
	var vid, id, plan, region, state string
	var expires int64
	var price int64
	e := s.Store.DB.QueryRow(ctx, `SELECT v.id,v.sales_order_id,v.state,v.expires_at,f.plan,f.region,o.price_cents FROM vouchers v JOIN orders o ON o.id=v.sales_order_id JOIN fulfillment_orders f ON f.order_id=o.id WHERE v.code_hash=$1 AND o.payment_status='paid' AND o.refund_hold=0 AND o.refunded_cents=0 AND o.redemption_blocked=0`, s.hash(code)).Scan(&vid, &id, &state, &expires, &plan, &region, &price)
	if e != nil || state != "active" || expires <= time.Now().UnixMilli() {
		return nil, fault.New(409, "卡密不可兑换：可能未激活、已绑定、过期或已停用。已兑换请查询原订单。")
	}
	contextID, now := security.ID(), time.Now().UnixMilli()
	_, e = s.Store.DB.Exec(ctx, `INSERT INTO voucher_contexts VALUES($1,$2,$3,$4,$5)`, contextID, vid, buyer, now+900000, now)
	if e != nil {
		return nil, e
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	e = s.Delivery.Call(ctx, map[string]any{"action": "ticket", "salesOrderId": id, "plan": plan, "region": region, "priceCents": price, "previewContextId": contextID}, &result)
	if e != nil {
		return nil, e
	}
	if len(result.Ticket) < 30 || len(result.Ticket) > 100 {
		return nil, errors.New("invalid ticket")
	}
	return map[string]any{"url": s.Delivery.Origin + "/deliver/#ticket=" + url.QueryEscape(result.Ticket), "plan": "GPT Plus", "expiresAt": expires}, nil
}
func (s Service) Confirm(ctx context.Context, in ConfirmInput) (map[string]any, error) {
	if !security.ValidID(in.ContextID) || !security.ValidID(in.OperationID) || len(in.AccountHash) < 20 || len(in.AccountHash) > 128 {
		return nil, fault.New(400, "预检上下文无效。")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	o, e := orders.Get(ctx, tx, in.OrderID, nil, true)
	if e != nil {
		return nil, e
	}
	if !fulfillment.Eligible(o) {
		return nil, fault.New(409, "订单已暂停或退款。")
	}
	var vid, buyer, state string
	var expiry, contextExpiry int64
	e = tx.QueryRow(ctx, `SELECT v.id,c.buyer_hash,v.state,v.expires_at,c.expires_at FROM voucher_contexts c JOIN vouchers v ON v.id=c.voucher_id WHERE c.id=$1 AND v.sales_order_id=$2 FOR UPDATE OF v`, in.ContextID, in.OrderID).Scan(&vid, &buyer, &state, &expiry, &contextExpiry)
	if e != nil {
		return nil, fault.New(409, "预检授权无效。")
	}
	var oldContext, oldAccount, operation string
	e = tx.QueryRow(ctx, `SELECT context_id,account_hash,operation_id FROM voucher_redemptions WHERE voucher_id=$1`, vid).Scan(&oldContext, &oldAccount, &operation)
	if e == nil {
		if oldContext != in.ContextID || oldAccount != in.AccountHash || operation != in.OperationID {
			return nil, fault.New(409, "卡密已绑定其他兑换，请通过原订单查询。")
		}
		return map[string]any{"authorized": true, "salesOrderId": in.OrderID, "alreadyConfirmed": true}, tx.Commit(ctx)
	}
	if e != pgx.ErrNoRows {
		return nil, e
	}
	now := time.Now().UnixMilli()
	if state != "active" || expiry <= now || contextExpiry <= now {
		return nil, fault.New(409, "卡密或检测授权已经失效，请重新进入兑换页。")
	}
	_, e = tx.Exec(ctx, `INSERT INTO voucher_redemptions(voucher_id,order_id,context_id,operation_id,account_hash,created_at) VALUES($1,$2,$3,$4,$5,$6)`, vid, in.OrderID, in.ContextID, in.OperationID, in.AccountHash, now)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `UPDATE vouchers SET state='locked',version=version+1 WHERE id=$1`, vid)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO order_guest_access VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, in.OrderID, buyer, now)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO voucher_dispatch(order_id,operation_id,context_id,created_at) VALUES($1,$2,$3,$4)`, in.OrderID, in.OperationID, in.ContextID, now)
	if e != nil {
		return nil, e
	}
	if e = event(ctx, tx, vid, "guest", "confirm", "客户确认账号并锁定权益"); e != nil {
		return nil, e
	}
	return map[string]any{"authorized": true, "salesOrderId": in.OrderID}, tx.Commit(ctx)
}
func (s Service) Recovery(ctx context.Context, id, buyer string) (string, error) {
	if _, e := orders.Get(ctx, s.Store.DB, id, &orders.Access{OwnerHash: buyer}, false); e != nil {
		return "", e
	}
	token := security.Token()
	_, e := s.Store.DB.Exec(ctx, `INSERT INTO order_recovery_secrets(order_id,token_hash,created_at) VALUES($1,$2,$3) ON CONFLICT(order_id) DO UPDATE SET token_hash=excluded.token_hash,created_at=excluded.created_at,version=order_recovery_secrets.version+1`, id, security.Hash(token), time.Now().UnixMilli())
	return token, e
}
func (s Service) Recover(ctx context.Context, id, buyer, secret string) error {
	if !security.ValidToken(secret) {
		return fault.New(401, "订单或找回口令无效。")
	}
	tag, e := s.Store.DB.Exec(ctx, `INSERT INTO order_guest_access SELECT order_id,$2,$3 FROM order_recovery_secrets WHERE order_id=$1 AND token_hash=$4 ON CONFLICT DO NOTHING`, id, buyer, time.Now().UnixMilli(), security.Hash(secret))
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		e = s.Store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM order_recovery_secrets WHERE order_id=$1 AND token_hash=$2)`, id, security.Hash(secret)).Scan(&exists)
		if e != nil {
			return e
		}
		if !exists {
			return fault.New(401, "订单或找回口令无效。")
		}
	}
	return nil
}
