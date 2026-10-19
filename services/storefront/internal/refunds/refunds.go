package refunds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/store"
)

type Supplier interface {
	RefundOriginal(context.Context, string, string, int64) (string, error)
	ReconcileRefund(context.Context, string, int64) (string, error)
}

const WarrantyUnavailableMessage = "该商品在购买前的下单须知中已明确说明不支持售后质保，因此无法提交售后申请。如有疑问，请联系商店客服。"

var ErrSupplierRejected = errors.New("supplier definitively rejected refund")
var ErrSupplierPending = errors.New("supplier accepted refund; awaiting confirmation")

type Service struct {
	Store    *store.Store
	Auth     *auth.Service
	Payments *payments.Service
	Supplier Supplier
	Testing  bool
}
type Case struct {
	ID        string  `json:"id"`
	OrderID   string  `json:"order_id"`
	Key       string  `json:"request_key"`
	Reason    string  `json:"reason"`
	Status    string  `json:"status"`
	Requested int64   `json:"requested_cents"`
	Approved  *int64  `json:"approved_cents"`
	Note      string  `json:"note"`
	Reference string  `json:"reference"`
	Version   int64   `json:"version"`
	Created   int64   `json:"created_at"`
	Updated   int64   `json:"updated_at"`
	Lease     *string `json:"-"`
}

func get(ctx context.Context, q orders.Query, id string, lock bool) (Case, error) {
	var c Case
	var raw []byte
	sql := `SELECT to_jsonb(r) FROM refund_cases r WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	e := q.QueryRow(ctx, sql, id).Scan(&raw)
	if e == pgx.ErrNoRows {
		return c, fault.New(404, "售后申请不存在。")
	}
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(raw, &c)
	return c, e
}
func text(value string, max int) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= max && !strings.ContainsRune(value, 0)
}
func lock(ctx context.Context, tx pgx.Tx) error {
	_, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(903500)`)
	return e
}
func event(ctx context.Context, tx pgx.Tx, c Case, actor, note string) error {
	_, e := tx.Exec(ctx, `INSERT INTO refund_events(id,case_id,status,note,actor,created_at) VALUES($1,$2,$3,$4,$5,$6)`, security.ID(), c.ID, c.Status, note, actor, time.Now().UnixMilli())
	return e
}
func update(ctx context.Context, tx pgx.Tx, c Case, status, note, reference string, amount *int64) (Case, error) {
	c.Status = status
	c.Note = note
	c.Reference = reference
	c.Approved = amount
	c.Version++
	c.Updated = time.Now().UnixMilli()
	_, e := tx.Exec(ctx, `UPDATE refund_cases SET status=$1,note=$2,reference=$3,approved_cents=$4,version=$5,updated_at=$6,last_event_id=$7 WHERE id=$8`, status, note, reference, amount, c.Version, c.Updated, security.ID(), c.ID)
	return c, e
}

type CustomerInput struct {
	Action  string `json:"action"`
	ID      string `json:"id"`
	Key     string `json:"key"`
	Reason  string `json:"reason"`
	Version *int64 `json:"version"`
}

func (s Service) CustomerWrite(ctx context.Context, id string, access *orders.Access, in CustomerInput) (bool, error) {
	if in.Version == nil || *in.Version < 0 {
		return false, fault.New(400, "请提供当前版本。")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	if e = lock(ctx, tx); e != nil {
		return false, e
	}
	o, e := orders.Get(ctx, tx, id, access, true)
	if e != nil {
		return false, e
	}
	if o.Mode == "demo" && !s.Testing {
		return false, orders.Conflict()
	}
	if in.Action == "cancel" {
		c, e := get(ctx, tx, in.ID, true)
		if e != nil {
			return false, e
		}
		if c.OrderID != id || c.Status != "pending" || c.Version != *in.Version {
			return false, orders.Conflict()
		}
		c, e = update(ctx, tx, c, "cancelled", "客户撤回申请", "", c.Approved)
		if e != nil {
			return false, e
		}
		if _, e = tx.Exec(ctx, `UPDATE orders SET refund_hold=0,version=version+1,updated_at=$1 WHERE id=$2`, time.Now().UnixMilli(), id); e != nil {
			return false, e
		}
		if e = event(ctx, tx, c, "customer", "客户撤回申请"); e != nil {
			return false, e
		}
		return false, tx.Commit(ctx)
	}
	if (in.Action != "" && in.Action != "request") || !security.ValidID(in.Key) || !text(in.Reason, 500) {
		return false, fault.New(400, "请填写有效申请编号和售后原因。")
	}
	var existing string
	e = tx.QueryRow(ctx, `SELECT reason FROM refund_cases WHERE order_id=$1 AND request_key=$2`, id, in.Key).Scan(&existing)
	if e == nil {
		if existing != strings.TrimSpace(in.Reason) {
			return false, orders.Conflict()
		}
		return false, tx.Commit(ctx)
	}
	if e != pgx.ErrNoRows {
		return false, e
	}
	supported, e := orders.CurrentWarranty(ctx, tx, o.Plan, true)
	if e != nil {
		return false, e
	}
	if !supported {
		return false, fault.New(409, WarrantyUnavailableMessage)
	}
	if o.Payment == "unpaid" || o.Status == "cancelled" || o.Refunded >= o.Price || o.Hold != 0 || o.Version != *in.Version {
		return false, orders.Conflict()
	}
	var busy bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fulfillment_orders WHERE order_id=$1 AND execution_started AND status!='failed_safe' UNION ALL SELECT 1 FROM email_deliveries WHERE order_id=$1 AND lease_until>$2 UNION ALL SELECT 1 FROM account_supplier_orders WHERE order_id=$1 AND (lease_until>$2 OR state IN ('creating','unknown')) UNION ALL SELECT 1 FROM delivery_supplier_orders WHERE order_id=$1 AND (lease_until>$2 OR state IN ('creating','unknown','pending')) UNION ALL SELECT 1 FROM xgift_purchases WHERE order_id=$1 AND (lease_until>$2 OR state IN ('creating','unknown','pending')) UNION ALL SELECT 1 FROM recharge_orders WHERE order_id=$1 AND (financial_hold=1 OR lease_until>$2) UNION ALL SELECT 1 FROM payments WHERE order_id=$1 AND lease_until>$2)`, id, time.Now().UnixMilli()).Scan(&busy)
	if e != nil {
		return false, e
	}
	if busy {
		return false, fault.New(409, "原订单正在履约或核对资金，请稍后申请。")
	}
	now := time.Now().UnixMilli()
	c := Case{ID: security.ID(), OrderID: id, Status: "pending", Requested: o.Price - o.Refunded, Reason: strings.TrimSpace(in.Reason)}
	_, e = tx.Exec(ctx, `INSERT INTO refund_cases(id,order_id,request_key,reason,requested_cents,created_at,updated_at,last_event_id) VALUES($1,$2,$3,$4,$5,$6,$6,$7)`, c.ID, id, in.Key, c.Reason, c.Requested, now, security.ID())
	if e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, `UPDATE orders SET refund_hold=1,version=version+1,updated_at=$1 WHERE id=$2`, now, id); e != nil {
		return false, e
	}
	rejected, e := tx.Query(ctx, `UPDATE partner_payouts SET status='rejected',note='关联订单申请售后，收益已重新核算，请稍后重新申请',handled_at=$1,version=version+1 WHERE id IN (SELECT payout_id FROM partner_earning_orders WHERE id=$2) AND status='pending' AND reviewed_at IS NULL RETURNING id`, now, id)
	if e != nil {
		return false, e
	}
	payoutIDs := []string{}
	for rejected.Next() {
		var pid string
		if e = rejected.Scan(&pid); e != nil {
			rejected.Close()
			return false, e
		}
		payoutIDs = append(payoutIDs, pid)
	}
	e = rejected.Err()
	rejected.Close()
	if e != nil {
		return false, e
	}
	for _, pid := range payoutIDs {
		if _, e = tx.Exec(ctx, `UPDATE partner_debits SET payout_id=NULL WHERE payout_id=$1`, pid); e != nil {
			return false, e
		}
		if _, e = tx.Exec(ctx, `UPDATE orders SET payout_id=CASE WHEN payout_id=$1 THEN NULL ELSE payout_id END,upstream_payout_id=CASE WHEN upstream_payout_id=$1 THEN NULL ELSE upstream_payout_id END WHERE payout_id=$1 OR upstream_payout_id=$1`, pid); e != nil {
			return false, e
		}
	}
	if e = event(ctx, tx, c, "customer", c.Reason); e != nil {
		return false, e
	}
	platform, e := automaticPlatformRefund(ctx, tx, o)
	if e != nil {
		return false, e
	}
	if platform {
		// Local approval authorizes submitting the customer's full remaining claim;
		// only a verified supplier refund can settle it. Persist the exact body in
		// the same transaction so a crash before the first HTTP call is recoverable.
		c, e = update(ctx, tx, c, "approved", "退款申请已保存，正在提交处理；是否退款及到账结果以核对为准。", "", &c.Requested)
		if e != nil {
			return false, e
		}
		body, err := json.Marshal(struct {
			Merchant string `json:"merchant_refund_no"`
			Type     string `json:"type"`
			Amount   string `json:"amount"`
			Reason   string `json:"reason"`
		}{c.ID, "full", fmt.Sprintf("%d.%02d", c.Requested/100, c.Requested%100), c.Reason})
		if err != nil {
			return false, err
		}
		if _, e = tx.Exec(ctx, `INSERT INTO recharge_refunds(case_id,order_id,create_body,updated_at) VALUES($1,$2,$3,0)`, c.ID, id, string(body)); e != nil {
			return false, e
		}
		if e = event(ctx, tx, c, "system", c.Note); e != nil {
			return false, e
		}
		if _, e = tx.Exec(ctx, `SELECT pg_notify('storefront_work',current_schema())`); e != nil {
			return false, e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return false, e
	}
	if platform {
		// The request is already durably accepted. A timeout/conflict must not
		// invite a new claim; the worker will resume this exact persisted intent.
		run, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = s.executeAs(run, "", c.ID, AdminInput{Action: "execute-refund", Version: &c.Version, Note: "自动核对客户申请的退款结果"}, true)
	}
	return true, nil
}

type AdminInput struct {
	Action    string `json:"action"`
	Version   *int64 `json:"version"`
	Note      string `json:"note"`
	Reference string `json:"reference"`
	Amount    int64  `json:"amountCents"`
}

func (s Service) AdminWrite(ctx context.Context, token, id string, in AdminInput) error {
	if in.Version == nil || *in.Version < 0 || !text(in.Note, 500) {
		return fault.New(400, "请填写有效版本和处理说明。")
	}
	if in.Action == "confirm-payment-refund" || in.Action == "complete" || in.Action == "execute-refund" || in.Action == "reconcile-platform-refund" {
		return s.execute(ctx, token, id, in)
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = lock(ctx, tx); e != nil {
		return e
	}
	p, e := s.Auth.Require(ctx, tx, "admin", token)
	if e != nil {
		return e
	}
	c, e := get(ctx, tx, id, true)
	if e != nil {
		return e
	}
	o, e := orders.Get(ctx, tx, c.OrderID, nil, true)
	if e != nil {
		return e
	}
	if c.Version != *in.Version || c.Status != "pending" || o.Hold != 1 {
		return orders.Conflict()
	}
	if in.Action == "approve-submit-platform" {
		var eligible bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recharge_orders WHERE order_id=$1 AND collection_mode='platform_collect' AND remote_id IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM payments WHERE order_id=$1)`, o.ID).Scan(&eligible); e != nil {
			return e
		}
		if !eligible || o.Mode != "live" || s.Supplier == nil {
			return fault.New(409, "该订单不能提交上游退款。")
		}
	}
	switch in.Action {
	case "approve", "approve-submit-platform":
		if in.Amount < 1 || in.Amount > c.Requested || in.Amount > o.Price-o.Refunded || (o.Status != "success" && in.Amount != o.Price-o.Refunded) {
			return fault.New(400, "退款金额超出可退范围，未交付订单必须全额退还剩余金额。")
		}
		c, e = update(ctx, tx, c, "approved", strings.TrimSpace(in.Note), "", &in.Amount)
	case "reject":
		c, e = update(ctx, tx, c, "rejected", strings.TrimSpace(in.Note), "", c.Approved)
		if e == nil {
			_, e = tx.Exec(ctx, `UPDATE orders SET refund_hold=0,version=version+1,updated_at=$1 WHERE id=$2`, time.Now().UnixMilli(), o.ID)
		}
	default:
		return orders.Conflict()
	}
	if e != nil {
		return e
	}
	if e = event(ctx, tx, c, "admin:"+p.Subject, in.Note); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	if in.Action == "approve-submit-platform" {
		in.Action = "execute-refund"
		in.Version = &c.Version
		return s.execute(ctx, token, id, in)
	}
	return nil
}
func (s Service) execute(ctx context.Context, token, id string, in AdminInput) error {
	return s.executeAs(ctx, token, id, in, false)
}
func (s Service) executeAs(ctx context.Context, token, id string, in AdminInput, system bool) error {
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = lock(ctx, tx); e != nil {
		return e
	}
	actor := "system"
	if !system {
		p, err := s.Auth.Require(ctx, tx, "admin", token)
		if err != nil {
			return err
		}
		actor = "admin:" + p.Subject
	}
	c, e := get(ctx, tx, id, true)
	if e != nil {
		return e
	}
	o, e := orders.Get(ctx, tx, c.OrderID, nil, true)
	if e != nil {
		return e
	}
	var bridgeBusy bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fulfillment_orders WHERE order_id=$1 AND execution_started AND status!='failed_safe')`, o.ID).Scan(&bridgeBusy); e != nil {
		return e
	}
	if bridgeBusy {
		return fault.New(409, "充值已开始，请先核对卡台扣款和订阅结果，暂不能执行退款。")
	}
	if c.Status != "approved" || c.Version != *in.Version || c.Approved == nil || o.Hold != 1 || o.Refunded+*c.Approved > o.Price {
		return orders.Conflict()
	}
	var pending, hasSupplier, hasPayment bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partner_payouts WHERE id IN (SELECT payout_id FROM partner_earning_orders WHERE id=$1) AND status='pending'),EXISTS(SELECT 1 FROM recharge_orders WHERE order_id=$1),EXISTS(SELECT 1 FROM payments WHERE order_id=$1)`, o.ID).Scan(&pending, &hasSupplier, &hasPayment)
	if e != nil {
		return e
	}
	if pending {
		return fault.New(409, "请先核对并完成或退回关联提现，再登记退款。")
	}
	if system {
		var started bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recharge_refunds WHERE case_id=$1 AND order_id=$2)`, id, o.ID).Scan(&started); e != nil {
			return e
		}
		if !hasSupplier || hasPayment || !started || in.Action != "execute-refund" {
			return orders.Conflict()
		}
	}
	channel := o.Mode == "live" && (hasSupplier || hasPayment)
	manualPayment := in.Action == "confirm-payment-refund"
	if manualPayment {
		if system || !hasPayment || hasSupplier || !text(in.Reference, 100) {
			return orders.Conflict()
		}
		var valid bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_refund_attempts r JOIN payments p ON p.order_id=r.order_id WHERE r.id=$1 AND r.order_id=$2 AND r.state='submitted' AND r.product_cents=$3 AND r.product_cents+r.fee_cents=$4 AND p.provider IN ('gatewayb-alipay','gatewayb-wxpay') AND p.state='paid')`, c.ID, o.ID, *c.Approved, in.Amount).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return fault.New(409, "请核对卓付退款流水及含手续费的实际退款总额。")
		}
	} else if in.Action == "reconcile-platform-refund" {
		if !hasSupplier {
			return orders.Conflict()
		}
	} else if (channel && in.Action != "execute-refund") || (!channel && in.Action != "complete") {
		return orders.Conflict()
	}
	if hasSupplier && s.Supplier == nil {
		return fault.New(503, "原平台退款通道未配置。")
	}
	if hasPayment && s.Payments == nil {
		return fault.New(503, "原支付退款通道未配置。")
	}
	reference := "DEMO-" + c.ID
	if o.Mode == "live" && !channel {
		if !text(in.Reference, 100) {
			return fault.New(400, "请填写实际退款流水号。")
		}
		reference = strings.TrimSpace(in.Reference)
	}
	lease := security.ID()
	now := time.Now().UnixMilli()
	tag, e := tx.Exec(ctx, `UPDATE refund_cases SET lease_token=$1,lease_until=$2 WHERE id=$3 AND lease_until<=$4`, lease, now+120000, id, now)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return orders.Conflict()
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = s.Store.DB.Exec(cleanup, `UPDATE refund_cases SET lease_token=NULL,lease_until=0 WHERE id=$1 AND lease_token=$2`, id, lease)
	}()
	if manualPayment {
		reference = strings.TrimSpace(in.Reference)
	} else if channel {
		if hasSupplier {
			if in.Action == "reconcile-platform-refund" {
				reference, e = s.Supplier.ReconcileRefund(ctx, o.ID, *c.Approved)
			} else {
				reference, e = s.Supplier.RefundOriginal(ctx, o.ID, c.ID, *c.Approved)
			}
		} else {
			reference, e = s.Payments.RefundOriginal(ctx, o.ID, c.ID, *c.Approved)
		}
	}
	rejected := errors.Is(e, ErrSupplierRejected)
	if errors.Is(e, ErrSupplierPending) {
		return nil
	}
	if e != nil && !rejected {
		return e
	}
	tx, e = s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = lock(ctx, tx); e != nil {
		return e
	}
	fresh, e := get(ctx, tx, id, true)
	if e != nil {
		return e
	}
	o, e = orders.Get(ctx, tx, c.OrderID, nil, true)
	if e != nil {
		return e
	}
	var owned bool
	e = tx.QueryRow(ctx, `SELECT lease_token=$1 AND lease_until>$2 FROM refund_cases WHERE id=$3`, lease, time.Now().UnixMilli(), id).Scan(&owned)
	if e != nil {
		return e
	}
	if !owned || fresh.Version != c.Version || fresh.Status != "approved" || o.Hold != 1 || o.Refunded+*c.Approved > o.Price {
		return orders.Conflict()
	}
	if manualPayment {
		tag, err := tx.Exec(ctx, `UPDATE payment_refund_attempts SET state='confirmed',updated_at=now() WHERE id=$1 AND order_id=$2 AND state='submitted'`, c.ID, o.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return orders.Conflict()
		}
	}
	if rejected {
		fresh, e = update(ctx, tx, fresh, "rejected", "平台已明确拒绝或取消本次退款："+in.Note, "", fresh.Approved)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE orders SET refund_hold=0,version=version+1,updated_at=$1 WHERE id=$2`, time.Now().UnixMilli(), o.ID)
		if e != nil {
			return e
		}
		if e = event(ctx, tx, fresh, actor, "平台已确认退款未执行，解除本次冻结"); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if e = settle(ctx, tx, fresh, o, in.Note, reference, actor, hasSupplier, in.Action == "reconcile-platform-refund"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Both initiated refunds and verified external refunds use the same ledger and
// partner debit checks. The caller must hold the refund lock and order row lock.
func settle(ctx context.Context, tx pgx.Tx, c Case, o orders.Order, note, reference, actor string, hasSupplier, exactSupplier bool) error {
	if c.Approved == nil || *c.Approved <= 0 || c.Status != "approved" || o.Refunded+*c.Approved > o.Price {
		return orders.Conflict()
	}
	var pending bool
	var e error
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partner_earning_orders o JOIN partner_payouts p ON p.id=o.payout_id WHERE o.id=$1 AND p.status='pending')`, o.ID).Scan(&pending); e != nil {
		return e
	}
	if pending {
		return orders.Conflict()
	}
	after := o.Refunded + *c.Approved
	if hasSupplier {
		var supplierRefund int64
		var supplierMargin *int64
		var supplierStatus string
		e = tx.QueryRow(ctx, `SELECT refunded_cents,merchant_margin_cents,payment_status FROM recharge_orders WHERE order_id=$1`, o.ID).Scan(&supplierRefund, &supplierMargin, &supplierStatus)
		if e != nil {
			return e
		}
		if exactSupplier && (supplierRefund != after || (supplierStatus != "partially_refunded" && supplierStatus != "refunded")) {
			return orders.Conflict()
		}
		if supplierMargin == nil {
			var paid bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partner_earning_orders o JOIN partner_payouts p ON p.id=o.payout_id WHERE o.id=$1 AND p.status='paid')`, o.ID).Scan(&paid); e != nil {
				return e
			}
			if paid {
				return orders.Conflict()
			}
		}
	}
	fresh, e := update(ctx, tx, c, "refunded", strings.TrimSpace(note), reference, c.Approved)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE orders SET refunded_cents=$1,refund_hold=0,version=version+1,updated_at=$2 WHERE id=$3`, after, time.Now().UnixMilli(), o.ID)
	if e != nil {
		return e
	}
	if hasSupplier {
		_, e = tx.Exec(ctx, `UPDATE recharge_orders SET financial_hold=CASE WHEN refunded_cents=$1 THEN 0 ELSE 1 END WHERE order_id=$2`, after, o.ID)
		if e != nil {
			return e
		}
	}
	if o.Mode == "live" {
		// Recompute each beneficiary from the original price snapshot after the refund.
		// Previously deducted amounts include both reserved and already settled debts.
		debts, e := tx.Query(ctx, `SELECT o.partner_id,greatest(0,COALESCE(i.profit_cents,o.price_cents-o.cost_cents)-partner_order_profit(o.id,o.partner_id)-COALESCE((SELECT sum(d.amount_cents) FROM partner_debits d JOIN refund_cases r ON r.id=d.refund_id WHERE r.order_id=o.id AND d.partner_id=o.partner_id),0))::bigint FROM partner_earning_orders o JOIN partner_payouts p ON p.id=o.payout_id AND p.status='paid' LEFT JOIN partner_payout_items i ON i.payout_id=p.id AND i.order_id=o.id WHERE o.id=$1`, o.ID)
		if e != nil {
			return e
		}
		type debt struct {
			partner string
			amount  int64
		}
		pendingDebts := []debt{}
		for debts.Next() {
			var d debt
			if e = debts.Scan(&d.partner, &d.amount); e != nil {
				debts.Close()
				return e
			}
			pendingDebts = append(pendingDebts, d)
		}
		e = debts.Err()
		debts.Close()
		if e != nil {
			return e
		}
		for _, d := range pendingDebts {
			if d.amount > 0 {
				if _, e = tx.Exec(ctx, `INSERT INTO partner_debits(id,partner_id,refund_id,amount_cents,created_at) VALUES($1,$2,$3,$4,$5)`, security.ID(), d.partner, c.ID, d.amount, time.Now().UnixMilli()); e != nil {
					return e
				}
			}
		}
	}
	if e = event(ctx, tx, fresh, actor, "已核对退款并完成记账："+note); e != nil {
		return e
	}
	return nil
}

// Only follows approved supplier refunds with a durable submission intent.
func (s Service) Reconcile(ctx context.Context) error {
	list, e := orders.Rows(ctx, s.Store.DB, `SELECT r.id,r.version FROM refund_cases r JOIN recharge_refunds t ON t.case_id=r.id WHERE r.status='approved' AND r.lease_until<=$1 AND t.updated_at<=$1-15000 ORDER BY t.updated_at LIMIT 10`, time.Now().UnixMilli())
	if e != nil {
		return e
	}
	var first error
	for _, row := range list {
		version := row["version"].(int64)
		id := row["id"].(string)
		if e = s.executeAs(ctx, "", id, AdminInput{Action: "execute-refund", Version: &version, Note: "自动核对已发起的退款结果"}, true); e != nil {
			if first == nil {
				first = e
			}
			_, _ = s.Store.DB.Exec(ctx, `UPDATE recharge_refunds SET updated_at=$1 WHERE case_id=$2`, time.Now().UnixMilli(), id)
		}
	}
	return first
}

// Completed orders require local review, including an earlier successful attempt
// hidden by a later retry. Keep the customer policy and submission gate identical.
func automaticPlatformRefund(ctx context.Context, q orders.Query, o orders.Order) (bool, error) {
	if o.Mode != "live" || o.Status == "success" {
		return false, nil
	}
	var eligible bool
	e := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recharge_orders WHERE order_id=$1 AND collection_mode='platform_collect' AND remote_id IS NOT NULL)
		AND NOT EXISTS(SELECT 1 FROM payments WHERE order_id=$1)
		AND NOT EXISTS(SELECT 1 FROM recharge_redemptions WHERE order_id=$1 AND status='succeeded')`, o.ID).Scan(&eligible)
	return eligible, e
}
