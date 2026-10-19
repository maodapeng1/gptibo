package orders

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/rechargecopy"
)

// Revalidate persisted QR data at the browser boundary; cashier URLs and tokens
// belong only to the server, including when serving historical orders.
var publicAlipayQR = regexp.MustCompile(`^https://qr\.alipay\.com/[A-Za-z0-9]{8,128}$`)

func Rows(ctx context.Context, q Query, sql string, args ...any) ([]map[string]any, error) {
	r, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []map[string]any{}
	for r.Next() {
		v, e := r.Values()
		if e != nil {
			return nil, e
		}
		m := map[string]any{}
		for i, f := range r.FieldDescriptions() {
			m[f.Name] = v[i]
		}
		out = append(out, m)
	}
	return out, r.Err()
}
func (s Service) renderView(o Order, admin bool, related viewRelations) (map[string]any, error) {
	out := map[string]any{"storeId": o.StoreID, "storeName": o.StoreName, "couponCode": o.CouponCode, "discountCents": o.Discount, "listPriceCents": o.ListPrice, "supportsWarranty": related.supportsWarranty, "purchaseNote": o.Note, "isTest": o.Test != 0, "customerEmail": o.Email, "refundedCents": o.Refunded, "refundHold": o.Hold != 0, "redemptionBlocked": o.Blocked != 0, "id": o.ID, "plan": o.Plan, "planName": o.PlanName, "price": float64(o.Price) / 100, "mode": o.Mode, "paymentStatus": o.Payment, "status": o.Status, "createdAt": o.Created, "updatedAt": o.Updated, "version": o.Version, "xgift": nil}
	if o.PartnerName != nil {
		out["partnerName"] = *o.PartnerName
	}
	if o.Started != nil {
		out["startedAt"] = *o.Started
	}
	if o.Phone != nil {
		out["smsPhone"] = *o.Phone
	}
	out["paymentSummary"] = related.payment
	out["commerceDelivery"] = related.commerce
	contacts := related.contacts
	out["supportContacts"] = contacts
	supplier := related.supplier
	out["manualFulfillment"] = (o.Mode == "live" && o.Test == 0 && o.Plan == "pro-25x" && len(supplier) == 0) || EmailKind(o.Plan) != ""
	if len(supplier) > 0 && o.Mode == "live" && o.Test == 0 {
		t := supplier[0]
		attempts := related.attempts
		a := map[string]any{}
		if len(attempts) > 0 && (o.Status != "success" || attempts[0]["status"] == "succeeded") {
			a = attempts[0]
		}
		expires := o.Created + 15*60*1000
		if n, ok := t["expires_at"].(int64); ok && n < expires {
			expires = n
		}
		payable := o.Status == "pending" && o.Payment == "unpaid" && t["payment_status"] == "pending" && o.Blocked == 0 && o.Hold == 0 && o.Refunded == 0 && expires > time.Now().UnixMilli()
		hold := t["financial_hold"] == int64(1) && t["remote_id"] != nil && o.Payment == "paid"
		f := map[string]any{"financialHold": hold, "paymentUrl": nil, "paymentQr": nil, "paymentKind": nil, "expiresAt": expires, "paymentExpired": o.Payment == "unpaid" && (t["payment_status"] == "expired" || (t["payment_status"] == "pending" && expires <= time.Now().UnixMilli())), "rechargeStatus": a["status"], "failureCode": a["failure_code"], "errorCategory": a["error_category"], "resultCode": a["result_code"], "progressStage": a["progress_stage"], "progressVersion": int64(0), "progressUpdatedAt": a["progress_updated_at"], "attemptNo": a["attempt_no"], "retryKey": nil, "retryMode": nil, "canSubmit": false, "message": nil, "recoveryAction": a["recovery_action"], "accountEmailMasked": a["account_email_masked"], "finishedAt": a["finished_at"], "syncError": false}
		if v := a["progress_version"]; v != nil {
			f["progressVersion"] = v
		}
		if payable {
			f["paymentKind"] = "qr"
			if qr, ok := t["payment_qr"].(string); ok && publicAlipayQR.MatchString(qr) {
				f["paymentQr"] = qr
			}
		}
		if message, ok := a["public_message"].(string); ok && rechargecopy.PublicMessage(message) != "" {
			f["message"] = rechargecopy.PublicMessage(message)
		}
		if a["status"] == "unknown" {
			f["retryKey"] = a["id"]
			f["retryMode"] = a["credential_mode"]
			f["message"] = "充值服务尚未确认收到本次提交，暂不能确认已开始充值。请核对原提交，勿重新付款。"
		}
		unknownExpired := false
		if created, ok := a["created_at"].(int64); ok && a["status"] == "unknown" {
			unknownExpired = time.Now().UnixMilli()-created >= 7*86400000
		}
		f["precheckRequired"] = s.SessionPrecheckRequired
		f["canSubmit"] = s.SupplierReady && o.Blocked == 0 && o.Hold == 0 && o.Refunded == 0 && t["financial_hold"] == int64(0) && t["payment_status"] == "paid" && o.Payment == "paid" && o.Status != "success" && (len(a) == 0 || (a["status"] == "unknown" && !unknownExpired) || a["status"] == "rejected" || a["fallback"] == int64(1))
		if unknownExpired {
			f["message"] = "原提交长期未确认，请联系客服核对原任务，勿重新提交。"
		} else if a["status"] == "failed" || a["status"] == "cancelled" || a["status"] == "rejected" {
			failure, _ := a["failure_code"].(string)
			category, _ := a["error_category"].(string)
			message, _ := a["public_message"].(string)
			f["message"] = rechargecopy.DisplayMessage(message, failure, category)
			f["failureReason"] = f["message"]
		}
		if o.Status == "success" {
			f["rechargeStatus"] = "succeeded"
			f["progressStage"] = "completed"
		}
		if a["recovery_action"] == "refund" {
			f["canSubmit"] = false
			if o.Refunded < o.Price {
				f["message"] = "平台正在处理退款，请在原订单查看售后进度。"
			}
		}
		if hold {
			f["message"] = "平台退款金额正在核对，充值与代理收益暂时冻结，请联系客服。"
		}
		out["fulfillment"] = f
		out["refundHold"] = o.Hold != 0 || hold
		// Show verified money returned to the customer even while local ledger
		// reconciliation is pending. Keep accounting values and supplier IDs admin-only.
		if !admin && o.Payment == "paid" && t["remote_id"] != nil {
			amount, _ := t["refunded_cents"].(int64)
			verified, _ := t["verified_at"].(int64)
			confirmed := verified > 0 && amount > 0 && ((t["payment_status"] == "refunded" && amount == o.Price) || (t["payment_status"] == "partially_refunded" && amount < o.Price))
			if confirmed && amount > o.Refunded {
				out["refundedCents"] = amount
				if amount == o.Price {
					out["refundHold"] = false
					f["message"] = "本订单已全额退款。"
				} else if hold {
					f["message"] = "本订单已部分退款，退款记录正在同步。"
				}
			}
		}
	}
	x := related.gift
	if len(x) > 0 {
		v := x[0]
		message := "付款确认后自动开始赠送。"
		if o.Payment == "paid" {
			message = "赠送正在处理，请稍候。"
		}
		switch v["state"] {
		case "succeeded":
			message = "X 赠送付款已完成，请前往 X 核对 Premium 到账。"
		case "failed":
			message = "本次赠送未完成，请通过原订单处理售后。"
		case "unknown":
			message = "结果正在核对，请勿重复下单。"
		}
		v["message"] = message
		out["xgift"] = v
	}
	if kind := EmailKind(o.Plan); kind != "" {
		d, n := related.delivery, related.purchase
		value := map[string]any{"kind": kind, "status": "waiting", "sentAt": nil}
		if len(d) > 0 {
			value["status"] = d[0]["status"]
			value["sentAt"] = d[0]["sent_at"]
		}
		if len(n) > 0 {
			value["automatic"] = true
		}
		if len(n) > 0 && n[0]["state"] == "purchased" && len(d) > 0 && o.Mode == "live" && o.Test == 0 && o.Payment == "paid" && o.Hold == 0 && o.Refunded == 0 && o.Blocked == 0 {
			value["value"] = d[0]["value"]
			value["instructions"] = d[0]["instructions"]
		}
		out["delivery"] = value
	}
	if admin {
		out["settlementStatus"] = related.settlement
		if len(related.attempts) > 0 {
			a := related.attempts[0]
			out["rechargeCredential"] = map[string]any{"attemptId": a["id"], "saved": a["credential_saved"] == true, "mode": a["credential_mode"], "savedAt": a["created_at"]}
		}
		out["platformStatus"] = nil
		if len(supplier) > 0 && o.Mode == "live" && o.Test == 0 && supplier[0]["remote_id"] != nil {
			t := supplier[0]
			out["platformStatus"] = map[string]any{"remoteId": t["remote_id"], "paymentStatus": t["payment_status"], "refundedCents": t["refunded_cents"], "checkedAt": t["verified_at"], "financialHold": t["financial_hold"] == int64(1)}
		}
	}
	return out, nil
}
func EmailKind(plan string) string {
	switch plan {
	case "gemini-pro-18m":
		return "link"
	case "duolingo-12m", "gmail-account":
		return "cdk"
	}
	return ""
}
func (s Service) CustomerList(ctx context.Context, access Access) (map[string]any, error) {
	list, e := List(ctx, s.Store.DB, `SELECT to_jsonb(o) FROM orders o WHERE is_test=0 AND (owner_hash=$1 OR customer_email=NULLIF($2,'') OR EXISTS(SELECT 1 FROM order_guest_access ga WHERE ga.order_id=o.id AND ga.buyer_hash=$1)) AND ($3='' OR partner_id=$3) ORDER BY created_at DESC,id DESC LIMIT 100`, access.OwnerHash, access.Email, domains.Current(ctx).PartnerID)
	if e != nil {
		return nil, e
	}
	// Only historical demo orders need local settlement. Live reads never call providers.
	for i, o := range list {
		if o.Mode == "demo" && o.Status == "processing" {
			list[i], e = s.Settle(ctx, o.ID, &access)
			if e != nil {
				return nil, e
			}
		}
	}
	tx, e := s.Store.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	list, e = List(ctx, tx, `SELECT to_jsonb(o) FROM orders o WHERE is_test=0 AND (owner_hash=$1 OR customer_email=NULLIF($2,'') OR EXISTS(SELECT 1 FROM order_guest_access ga WHERE ga.order_id=o.id AND ga.buyer_hash=$1)) AND ($3='' OR partner_id=$3) ORDER BY created_at DESC,id DESC LIMIT 100`, access.OwnerHash, access.Email, domains.Current(ctx).PartnerID)
	if e != nil {
		return nil, e
	}
	out, e := s.Views(ctx, tx, list, false)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	var email any
	if access.Email != "" {
		email = access.Email
	}
	return map[string]any{"orders": out, "recoveryEmail": email}, nil
}
func (s Service) Detail(ctx context.Context, id string, access *Access) (map[string]any, error) {
	o, e := s.Settle(ctx, id, access)
	if e != nil {
		return nil, e
	}
	tx, e := s.Store.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	o, e = Get(ctx, tx, id, access, false)
	if e != nil {
		return nil, e
	}
	v, e := s.View(ctx, tx, o, access == nil)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"order": v}
	if access == nil {
		events, e := Rows(ctx, tx, `SELECT id,actor,action,from_status,to_status,note,created_at FROM order_events WHERE order_id=$1 ORDER BY order_version`, id)
		if e != nil {
			return nil, e
		}
		out["events"] = events
		v["partnerSource"] = nil
		if o.PartnerID != nil {
			p, e := Rows(ctx, tx, `SELECT p.id,p.slug,p.name,p.email,COALESCE(c.wechat,'') wechat,COALESCE(c.qq,'') qq FROM partners p LEFT JOIN support_contacts c ON c.owner=p.id WHERE p.id=$1`, o.PartnerID)
			if e != nil {
				return nil, e
			}
			if len(p) > 0 {
				v["partnerSource"] = p[0]
			}
		}
	}
	return out, tx.Commit(ctx)
}
func validStatus(status string) bool {
	return status == "" || status == "pending" || status == "session" || status == "processing" || status == "success" || status == "failed" || status == "cancelled"
}
func (s Service) AdminList(ctx context.Context, page int, query, status, plan string) (map[string]any, error) {
	if page < 1 || page > 10000 || len(query) > 320 || len(plan) > 64 || !validStatus(status) {
		return nil, fault.New(400, "筛选条件无效。")
	}
	if e := s.SettleDue(ctx); e != nil {
		return nil, e
	}
	tx, e := s.Store.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	query = strings.TrimSpace(query)
	where := ` WHERE ($1='' OR strpos(upper(o.id),upper($1))>0 OR strpos(lower(COALESCE(o.customer_email,'')),lower($1))>0 OR EXISTS(SELECT 1 FROM recharge_orders t WHERE t.order_id=o.id AND strpos(lower(t.remote_id),lower($1))>0)) AND ($2='' OR o.status=$2) AND ($3='' OR o.plan=$3)`
	var total int64
	e = tx.QueryRow(ctx, `SELECT count(*) FROM orders o`+where, query, status, plan).Scan(&total)
	if e != nil {
		return nil, e
	}
	list, e := List(ctx, tx, `SELECT to_jsonb(o) FROM orders o`+where+` ORDER BY created_at DESC,id DESC LIMIT 20 OFFSET $4`, query, status, plan, (page-1)*20)
	if e != nil {
		return nil, e
	}
	views, e := s.Views(ctx, tx, list, true)
	if e != nil {
		return nil, e
	}
	return map[string]any{"orders": views, "total": total, "page": page}, tx.Commit(ctx)
}
func (s Service) Overview(ctx context.Context) (map[string]any, error) {
	if e := s.SettleDue(ctx); e != nil {
		return nil, e
	}
	tx, e := s.Store.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	stats, e := Rows(ctx, tx, `SELECT count(*) total,count(*) FILTER(WHERE status='pending') pending,count(*) FILTER(WHERE status IN ('session','processing','failed') AND refunded_cents<price_cents) active,count(*) FILTER(WHERE status='success') completed,COALESCE(sum(price_cents) FILTER(WHERE mode='demo' AND payment_status='simulated'),0)::bigint demo_amount,COALESCE(sum(price_cents+COALESCE(p.fee_cents,0)-greatest(refunded_cents,COALESCE((SELECT t.refunded_cents FROM recharge_orders t WHERE t.order_id=o.id),0))-COALESCE(r.fee,0)) FILTER(WHERE mode='live' AND payment_status='paid'),0)::bigint paid_amount,COALESCE(sum(greatest(refunded_cents,COALESCE((SELECT t.refunded_cents FROM recharge_orders t WHERE t.order_id=o.id),0))+COALESCE(r.fee,0)) FILTER(WHERE mode='live'),0)::bigint refund_amount FROM orders o LEFT JOIN payments p ON p.order_id=o.id LEFT JOIN (SELECT order_id,sum(fee_cents) fee FROM payment_refund_attempts WHERE state='confirmed' GROUP BY order_id) r ON r.order_id=o.id WHERE is_test=0`)
	if e != nil {
		return nil, e
	}
	var interest int64
	e = tx.QueryRow(ctx, `SELECT count(*) FROM launch_interest`).Scan(&interest)
	if e != nil {
		return nil, e
	}
	list, e := List(ctx, tx, `SELECT to_jsonb(o) FROM orders o WHERE is_test=0 ORDER BY created_at DESC,id DESC LIMIT 6`)
	if e != nil {
		return nil, e
	}
	recent, e := s.Views(ctx, tx, list, true)
	if e != nil {
		return nil, e
	}
	mode := "demo"
	if s.SupplierCheckout {
		mode = "live"
	}
	return map[string]any{"stats": stats[0], "interest": map[string]any{"total": interest}, "recent": recent, "integrations": map[string]any{"alipay": s.PaymentReady, "supplier": s.SupplierReady, "supplierCheckout": s.SupplierCheckout, "resend": s.Auth.Ready()}, "mode": mode}, tx.Commit(ctx)
}
