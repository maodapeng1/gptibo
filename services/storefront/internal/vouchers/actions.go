package vouchers

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"strings"
	"time"
)

type ActionInput struct {
	Action    string `json:"action"`
	Note      string `json:"note"`
	Reference string `json:"reference"`
	Version   int64  `json:"version"`
	Confirmed bool   `json:"confirmed"`
}

func (s Service) Action(ctx context.Context, actor, id string, in ActionInput) (map[string]any, error) {
	in.Note = strings.TrimSpace(in.Note)
	in.Reference = strings.TrimSpace(in.Reference)
	if len(in.Note) < 1 || len(in.Note) > 500 {
		return nil, fault.New(400, "请填写处理说明，不要填写 Session 或卡片信息。")
	}
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	// Order locks always precede voucher locks, including confirmation and refund paths.
	var sale *string
	if e = tx.QueryRow(ctx, `SELECT sales_order_id FROM vouchers WHERE id=$1`, id).Scan(&sale); e != nil {
		return nil, fault.New(404, "卡密不存在。")
	}
	var o orders.Order
	if sale != nil {
		o, e = orders.Get(ctx, tx, *sale, nil, true)
		if e != nil {
			return nil, e
		}
	}
	var state, batch string
	var version int64
	var expiry *int64
	if e = tx.QueryRow(ctx, `SELECT state,batch_id,version,expires_at FROM vouchers WHERE id=$1 FOR UPDATE`, id).Scan(&state, &batch, &version, &expiry); e != nil {
		return nil, e
	}
	if version != in.Version {
		return nil, orders.Conflict()
	}
	result := map[string]any{"ok": true}
	switch in.Action {
	case "recover_access":
		if sale == nil || !in.Confirmed || in.Reference == "" || len(in.Reference) > 160 {
			return nil, fault.New(400, "请先核实购买记录，填写核对依据并确认重发。")
		}
		var redeemed bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM voucher_redemptions WHERE order_id=$1)`, *sale).Scan(&redeemed); e != nil {
			return nil, e
		}
		if !redeemed {
			return nil, fault.New(409, "尚未绑定兑换，请客户使用未消费卡密。")
		}
		secret := security.Token()
		_, e = tx.Exec(ctx, `INSERT INTO order_recovery_secrets(order_id,token_hash,created_at) VALUES($1,$2,$3) ON CONFLICT(order_id) DO UPDATE SET token_hash=excluded.token_hash,created_at=excluded.created_at,version=order_recovery_secrets.version+1`, *sale, security.Hash(secret), time.Now().UnixMilli())
		if e != nil {
			return nil, e
		}
		result["orderId"] = *sale
		result["recoverySecret"] = secret
		in.Note += " 核对依据：" + in.Reference
	case "note":
	case "revoke", "replace":
		if state != "inactive" && state != "active" {
			return nil, fault.New(409, "已绑定或处理中的卡密不能停用或补发，请先核对原充值结果。")
		}
		if !in.Confirmed {
			return nil, fault.New(400, "请确认本次停用或补发。")
		}
		if sale != nil {
			var started bool
			if e = tx.QueryRow(ctx, `SELECT execution_started FROM fulfillment_orders WHERE order_id=$1`, *sale).Scan(&started); e != nil {
				return nil, e
			}
			if started {
				return nil, fault.New(409, "执行授权已发放，暂不能补发。")
			}
		}
		if _, e = tx.Exec(ctx, `UPDATE vouchers SET state='revoked',version=version+1 WHERE id=$1`, id); e != nil {
			return nil, e
		}
		if in.Action == "replace" {
			raw := make([]byte, 20)
			if _, e = rand.Read(raw); e != nil {
				return nil, e
			}
			value := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
			code := "AI-" + value[:8] + "-" + value[8:16] + "-" + value[16:24] + "-" + value[24:]
			newID := security.ID()
			_, e = tx.Exec(ctx, `INSERT INTO vouchers(id,batch_id,code_hash,mask,state,sales_order_id,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, newID, batch, s.hash(code), "AI-••••-"+value[24:], state, sale, expiry, time.Now().UnixMilli())
			if e != nil {
				return nil, e
			}
			if e = event(ctx, tx, newID, actor, "replacement", "替代旧权益 "+id+"："+in.Note); e != nil {
				return nil, e
			}
			result["code"] = code
			result["voucherId"] = newID
		} else if sale != nil {
			next := o
			next.Blocked = 1
			if _, e = orders.Change(ctx, tx, o, next, "admin:"+actor, "voucher-revoke", in.Note); e != nil {
				return nil, e
			}
		}
	case "refund":
		if sale == nil || o.Refunded != 0 || !in.Confirmed || len(in.Reference) < 1 || len(in.Reference) > 160 {
			return nil, fault.New(409, "请确认已实际退款，并填写唯一退款流水；该权益需存在未退款销售订单。")
		}
		var fs string
		var started bool
		if e = tx.QueryRow(ctx, `SELECT status,execution_started FROM fulfillment_orders WHERE order_id=$1`, *sale).Scan(&fs, &started); e != nil {
			return nil, e
		}
		if started && fs != "failed_safe" && fs != "succeeded" {
			return nil, fault.New(409, "充值执行结果未明确，不能登记退款或解除执行锁。")
		}
		_, e = tx.Exec(ctx, `INSERT INTO offline_refunds VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, security.ID(), *sale, in.Reference, o.Price, in.Note, actor, time.Now().UnixMilli())
		if e != nil {
			return nil, e
		}
		var saved bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM offline_refunds WHERE order_id=$1 AND transaction_reference=$2)`, *sale, in.Reference).Scan(&saved); e != nil {
			return nil, e
		}
		if !saved {
			return nil, fault.New(409, "退款流水已用于其他订单。")
		}
		next := o
		next.Blocked = 1
		if _, e = orders.Change(ctx, tx, o, next, "admin:"+actor, "offline-refund", in.Note); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `UPDATE orders SET refunded_cents=price_cents,refund_hold=0 WHERE id=$1`, *sale); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `UPDATE vouchers SET state='revoked',version=version+1 WHERE id=$1`, id); e != nil {
			return nil, e
		}
	case "cancel_unstarted":
		if sale == nil || !in.Confirmed || (state != "locked" && state != "review_required") || o.Refunded != 0 || o.Hold != 0 {
			return nil, fault.New(409, "原订单不支持取消未开始的执行。")
		}
		if e = event(ctx, tx, id, actor, "cancel-unstarted-request", in.Note); e != nil {
			return nil, e
		}
		if e = tx.Commit(ctx); e != nil {
			return nil, e
		}
		if e = s.Delivery.Call(ctx, map[string]any{"action": "cancel_unstarted", "salesOrderId": *sale}, &result); e != nil {
			return nil, e
		}
		if result["cancelled"] != true {
			return nil, fault.New(409, "取消结果未确认，请勿登记退款。")
		}
		if e = s.Delivery.Sync(ctx, *sale); e != nil {
			return nil, e
		}
		return result, nil
	case "resume", "close_unconfirmed":
		if sale == nil || state != "review_required" || !in.Confirmed || o.Blocked != 0 || o.Refunded != 0 || o.Hold != 0 {
			return nil, fault.New(409, "当前权益不能恢复支付。")
		}
		if in.Action == "close_unconfirmed" && (in.Reference == "" || len(in.Reference) > 160) {
			return nil, fault.New(400, "请填写卡台确认未受理的核对依据编号。")
		}
		if e = event(ctx, tx, id, actor, in.Action+"-request", in.Note+" 核对依据："+in.Reference); e != nil {
			return nil, e
		}
		if e = tx.Commit(ctx); e != nil {
			return nil, e
		}
		remoteAction := "resume"
		if in.Action == "close_unconfirmed" {
			remoteAction = "close_unknown"
		}
		if e = s.Delivery.Call(ctx, map[string]any{"action": remoteAction, "salesOrderId": *sale}, &result); e != nil {
			return nil, e
		}
		if e = s.Delivery.Sync(ctx, *sale); e != nil {
			return nil, e
		}
		return result, nil
	default:
		return nil, fault.New(400, "不支持的处理方式。")
	}
	if e = event(ctx, tx, id, actor, in.Action, in.Note); e != nil {
		return nil, e
	}
	return result, tx.Commit(ctx)
}
