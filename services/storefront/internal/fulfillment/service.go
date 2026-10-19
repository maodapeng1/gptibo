// Package fulfillment bridges paid storefront orders to order-scoped delivery.
package fulfillment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/store"
)

type Product struct {
	Plan   string `json:"plan"`
	Region string `json:"region"`
}
type Service struct {
	Store          *store.Store
	Origin, Secret string
	Products       map[string]Product
}

func (s *Service) Enabled(plan string) bool { return s != nil && s.Products[plan].Plan != "" }
func (s *Service) Snapshot(ctx context.Context, tx pgx.Tx, o orders.Order, _ orders.CreateInput) error {
	p := s.Products[o.Plan]
	_, err := tx.Exec(ctx, `INSERT INTO fulfillment_orders(order_id,plan,region) VALUES($1,$2,$3)`, o.ID, p.Plan, p.Region)
	return err
}
func (s *Service) Call(ctx context.Context, body any, out any) error {
	raw, e := json.Marshal(body)
	if e != nil {
		return e
	}
	path := "/api/fulfillment"
	stamp := stringTime()
	nonce := security.ID()
	req, e := http.NewRequestWithContext(ctx, "POST", s.Origin+path, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Commerce-Time", stamp)
	req.Header.Set("X-Commerce-Nonce", nonce)
	req.Header.Set("X-Commerce-Signature", security.MAC([]byte(s.Secret), "POST\n"+path+"\n"+stamp+"\n"+nonce+"\n"+string(raw)))
	client := http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, e := client.Do(req)
	if e != nil {
		return fault.New(503, "交付服务暂不可用，请稍后从原订单重试。")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fault.New(503, "交付服务尚未就绪，请联系管理员核对配置。")
	}
	return json.NewDecoder(io.LimitReader(res.Body, 32768)).Decode(out)
}
func Eligible(o orders.Order) bool {
	return o.Mode == "live" && o.Test == 0 && o.Payment == "paid" && o.Hold == 0 && o.Blocked == 0 && o.Refunded == 0 && o.Status != "cancelled"
}
func (s *Service) Ticket(ctx context.Context, id string, a *orders.Access) (string, error) {
	o, e := orders.Get(ctx, s.Store.DB, id, a, false)
	if e != nil {
		return "", e
	}
	if !Eligible(o) {
		return "", fault.New(409, "请确认付款，且订单未进入售后或关闭。")
	}
	var plan, region string
	if e = s.Store.DB.QueryRow(ctx, `SELECT plan,region FROM fulfillment_orders WHERE order_id=$1`, id).Scan(&plan, &region); e != nil {
		return "", fault.New(404, "该订单不使用独立充值交付服务。")
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	e = s.Call(ctx, map[string]any{"action": "ticket", "salesOrderId": o.ID, "plan": plan, "region": region, "priceCents": o.Price}, &result)
	if e != nil {
		return "", e
	}
	if len(result.Ticket) < 30 || len(result.Ticket) > 100 {
		return "", errors.New("invalid fulfillment ticket")
	}
	return s.Origin + "/deliver/#ticket=" + url.QueryEscape(result.Ticket), nil
}
func (s *Service) Authorize(ctx context.Context, id string, execute bool) (bool, error) {
	return s.AuthorizeContext(ctx, id, execute, "", false)
}
func (s *Service) AuthorizeContext(ctx context.Context, id string, execute bool, contextID string, preview bool) (bool, error) {
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	o, e := orders.Get(ctx, tx, id, nil, true)
	if e != nil {
		return false, e
	}
	if !Eligible(o) {
		return false, nil
	}
	var state string
	if e = tx.QueryRow(ctx, `SELECT status FROM fulfillment_orders WHERE order_id=$1 FOR UPDATE`, id).Scan(&state); e != nil {
		return false, e
	}
	var voucherID string
	ev := tx.QueryRow(ctx, `SELECT id FROM vouchers WHERE sales_order_id=$1 AND state<>'revoked'`, id).Scan(&voucherID)
	if ev != nil && ev != pgx.ErrNoRows {
		return false, ev
	}
	if ev == nil {
		var valid bool
		if preview {
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM vouchers v JOIN voucher_contexts c ON c.voucher_id=v.id WHERE v.id=$1 AND c.id=$2 AND ((v.state='active' AND v.expires_at>$3 AND c.expires_at>$3) OR (v.state IN ('locked','review_required','redeemed') AND EXISTS(SELECT 1 FROM voucher_redemptions vr WHERE vr.voucher_id=v.id AND vr.context_id=c.id))))`, voucherID, contextID, time.Now().UnixMilli()).Scan(&valid)
		} else {
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM vouchers v JOIN voucher_redemptions vr ON vr.voucher_id=v.id WHERE v.id=$1 AND v.state IN ('locked','review_required','redeemed') AND ($2=false OR vr.context_id=$3))`, voucherID, execute, contextID).Scan(&valid)
		}
		if e != nil {
			return false, e
		}
		if !valid {
			return false, nil
		}
	}
	if execute {
		if state == "succeeded" || state == "failed_safe" {
			return false, nil
		}
		if _, e = tx.Exec(ctx, `UPDATE fulfillment_orders SET execution_started=true WHERE order_id=$1`, id); e != nil {
			return false, e
		}
	}
	return true, tx.Commit(ctx)
}
func (s *Service) Verify(ctx context.Context, headers http.Header, path string, raw []byte) error {
	stamp, nonce := headers.Get("X-Commerce-Time"), headers.Get("X-Commerce-Nonce")
	n, e := parseTime(stamp)
	if e != nil || n < time.Now().UnixMilli()-60000 || n > time.Now().UnixMilli()+60000 || !security.ValidID(nonce) {
		return fault.New(401, "内部授权已失效。")
	}
	expected := security.MAC([]byte(s.Secret), "POST\n"+path+"\n"+stamp+"\n"+nonce+"\n"+string(raw))
	if !security.Equal(expected, headers.Get("X-Commerce-Signature")) {
		return fault.New(401, "内部授权无效。")
	}
	tag, e := s.Store.DB.Exec(ctx, `INSERT INTO fulfillment_nonces VALUES($1,$2) ON CONFLICT DO NOTHING`, nonce, time.Now().UnixMilli()+120000)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return fault.New(409, "内部请求已处理。")
	}
	_, e = s.Store.DB.Exec(ctx, `DELETE FROM fulfillment_nonces WHERE expires_at<$1`, time.Now().UnixMilli())
	return e
}

// Reconcile advances only the existing authorized delivery order, never creates a new card.
func (s *Service) Reconcile(ctx context.Context) error {
	rows, e := s.Store.DB.Query(ctx, `SELECT f.order_id FROM fulfillment_orders f JOIN orders o ON o.id=f.order_id WHERE o.payment_status='paid' AND (NOT EXISTS(SELECT 1 FROM vouchers v WHERE v.sales_order_id=o.id) OR EXISTS(SELECT 1 FROM voucher_dispatch vd WHERE vd.order_id=o.id)) AND f.status NOT IN ('succeeded','failed_safe') AND f.checked_at<$1 AND f.next_sync_at<$2 ORDER BY f.next_sync_at,f.checked_at LIMIT 30`, time.Now().UnixMilli()-10000, time.Now().UnixMilli())
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	var failures []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e = s.Sync(ctx, id); e != nil {
			failures = append(failures, e)
			// Preserve confirmed status; schedule failed orders behind other due work.
			if _, saveErr := s.Store.DB.Exec(ctx, `UPDATE fulfillment_orders SET sync_failures=LEAST(sync_failures+1,8),sync_error='暂时无法核对最新进度，已保留上次确认结果。',next_sync_at=$1+LEAST(300000,15000*(1::bigint<<LEAST(sync_failures,4))) WHERE order_id=$2`, time.Now().UnixMilli(), id); saveErr != nil {
				failures = append(failures, saveErr)
			}
		}
	}
	return errors.Join(failures...)
}
func (s *Service) Sync(ctx context.Context, id string) error {
	var result struct {
		Status              string  `json:"status"`
		Message             string  `json:"message"`
		UpdatedAt           int64   `json:"updatedAt"`
		RemoteOrderID       *string `json:"fulfillmentId"`
		CardOrderID         *string `json:"cardOrderId"`
		ProviderOperationID *string `json:"providerOperationId"`
		ErrorCode           *string `json:"errorCode"`
		ErrorRequestID      *string `json:"errorRequestId"`
		AccountEmailMasked  *string `json:"accountEmailMasked"`
	}
	if e := s.Call(ctx, map[string]any{"action": "progress", "salesOrderId": id}, &result); e != nil {
		return e
	}
	valid := map[string]bool{"awaiting_session": true, "issuing": true, "opening": true, "card_issued": true, "submitting": true, "pending": true, "unknown": true, "payment_blocked": true, "action_required": true, "succeeded": true, "failed_safe": true}
	if !valid[result.Status] || len(result.Message) > 2000 {
		return errors.New("invalid fulfillment snapshot")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	o, e := orders.Get(ctx, tx, id, nil, true)
	if e != nil {
		return e
	}
	tag, e := tx.Exec(ctx, `UPDATE fulfillment_orders SET status=$1,message=$2,remote_updated_at=$3,checked_at=$4,sync_failures=0,sync_error='',next_sync_at=0,remote_order_id=COALESCE($6,remote_order_id),card_order_id=COALESCE($7,card_order_id),provider_operation_id=COALESCE($8,provider_operation_id),error_code=COALESCE($9,error_code),error_request_id=COALESCE($10,error_request_id),account_email_masked=COALESCE($11,account_email_masked) WHERE order_id=$5 AND remote_updated_at<=$3 AND status NOT IN ('succeeded','failed_safe')`, result.Status, result.Message, result.UpdatedAt, time.Now().UnixMilli(), id, result.RemoteOrderID, result.CardOrderID, result.ProviderOperationID, result.ErrorCode, result.ErrorRequestID, result.AccountEmailMasked)
	if e != nil {
		return e
	}
	if tag.RowsAffected() > 0 && Eligible(o) {
		next := o
		switch result.Status {
		case "succeeded":
			next.Status = "success"
		case "failed_safe":
			next.Status = "failed"
		case "awaiting_session":
		default:
			next.Status = "processing"
		}
		if next.Status != o.Status {
			if _, e = orders.Change(ctx, tx, o, next, "system", "fulfillment-sync", "同步充值系统执行状态"); e != nil {
				return e
			}
		}
	}
	if tag.RowsAffected() > 0 {
		nextVoucher := "locked"
		nextRedemption := "processing"
		if result.Status == "succeeded" {
			nextVoucher = "redeemed"
			nextRedemption = "succeeded"
		} else if result.Status == "failed_safe" || result.Status == "unknown" || result.Status == "payment_blocked" || result.Status == "action_required" {
			nextVoucher = "review_required"
			nextRedemption = "review_required"
		}
		_, e = tx.Exec(ctx, `UPDATE vouchers SET state=$1,version=version+1 WHERE sales_order_id=$2 AND state IN ('locked','review_required') AND state<>$1`, nextVoucher, id)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE voucher_redemptions SET state=$1 WHERE order_id=$2`, nextRedemption, id)
		if e != nil {
			return e
		}
		if result.Status != "awaiting_session" {
			if _, e = tx.Exec(ctx, `UPDATE voucher_dispatch SET state='accepted' WHERE order_id=$1`, id); e != nil {
				return e
			}
		}
	}
	return tx.Commit(ctx)
}
