package orders

import (
	"context"
	"gptibo/storefront/internal/catalog"
)

type viewRelations struct {
	supportsWarranty                             bool
	contacts                                     catalog.Contacts
	supplier, attempts, gift, delivery, purchase []map[string]any
	settlement                                   any
	payment                                      map[string]any
	commerce                                     map[string]any
}

// Views loads each relation once per page, rather than once per order.
// Callers use a repeatable-read transaction to keep all fields consistent.
func (s Service) Views(ctx context.Context, q Query, list []Order, admin bool) ([]map[string]any, error) {
	out := []map[string]any{}
	if len(list) == 0 {
		return out, nil
	}
	ids, owners := []string{}, []string{"platform"}
	related := map[string]*viewRelations{}
	for _, o := range list {
		ids = append(ids, o.ID)
		related[o.ID] = &viewRelations{}
		if o.PartnerID != nil {
			owners = append(owners, *o.PartnerID)
			if o.StoreID != nil {
				owners = append(owners, *o.StoreID)
			}
		}
	}
	contacts, err := Rows(ctx, q, `SELECT owner,wechat,qq,email FROM support_contacts c WHERE owner=ANY($1::text[]) AND NOT EXISTS(SELECT 1 FROM partner_stores s WHERE s.id=c.owner) UNION ALL SELECT s.id,COALESCE(s.settings->>'wechat',c.wechat,''),COALESCE(s.settings->>'qq',c.qq,''),COALESCE(s.settings->>'email',c.email,'') FROM partner_stores s LEFT JOIN support_contacts c ON c.owner=s.partner_id AND s.slot=1 WHERE s.id=ANY($1::text[])`, owners)
	if err != nil {
		return nil, err
	}
	byOwner := map[string]catalog.Contacts{}
	for _, c := range contacts {
		byOwner[c["owner"].(string)] = catalog.Contacts{Wechat: c["wechat"].(string), QQ: c["qq"].(string), Email: c["email"].(string)}
	}
	queries := []struct {
		sql    string
		assign func(*viewRelations, map[string]any)
	}{
		{`SELECT o.id order_id,COALESCE(c.supports_warranty,true) supported FROM orders o LEFT JOIN catalog_plans c ON c.id=o.plan WHERE o.id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.supportsWarranty = r["supported"].(bool) }},
		{`SELECT order_id,amount_cents "amountCents",fee_cents "feeCents",fee_bps "feeBps",COALESCE((SELECT sum(r.fee_cents) FROM payment_refund_attempts r WHERE r.order_id=p.order_id AND r.state='confirmed'),0)::bigint "refundedFeeCents" FROM payments p WHERE order_id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.payment = r }},
		{`SELECT order_id,status,message,checked_at,remote_updated_at,sync_error,sync_failures,account_email_masked FROM fulfillment_orders WHERE order_id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.commerce = r }},
		{`SELECT order_id,remote_id,payment_status,refunded_cents,financial_hold,verified_at,payment_qr,expires_at FROM recharge_orders WHERE order_id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.supplier = append(v.supplier, r) }},
		{`SELECT DISTINCT ON(order_id) order_id,id,EXISTS(SELECT 1 FROM recharge_credentials c WHERE c.attempt_id=recharge_redemptions.id) credential_saved,status,fallback,public_message,recovery_action,failure_code,error_category,result_code,progress_stage,progress_version,progress_updated_at,attempt_no,credential_mode,account_email_masked,finished_at,created_at FROM recharge_redemptions WHERE order_id=ANY($1::text[]) ORDER BY order_id,attempt_no DESC`, func(v *viewRelations, r map[string]any) { v.attempts = append(v.attempts, r) }},
		{`SELECT order_id,username,state,receipt,failure_code "failureCode" FROM xgift_purchases WHERE order_id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.gift = append(v.gift, r) }},
		{`SELECT order_id,status,sent_at,value,instructions FROM email_deliveries WHERE order_id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.delivery = append(v.delivery, r) }},
		{`SELECT order_id,state FROM delivery_supplier_orders WHERE order_id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.purchase = append(v.purchase, r) }},
	}
	if admin {
		queries = append(queries, struct {
			sql    string
			assign func(*viewRelations, map[string]any)
		}{`SELECT o.id order_id,p.status FROM orders o LEFT JOIN partner_payouts p ON p.id=o.payout_id WHERE o.id=ANY($1::text[])`, func(v *viewRelations, r map[string]any) { v.settlement = r["status"] }})
	}
	for _, query := range queries {
		rows, e := Rows(ctx, q, query.sql, ids)
		if e != nil {
			return nil, e
		}
		for _, r := range rows {
			id := r["order_id"].(string)
			delete(r, "order_id")
			query.assign(related[id], r)
		}
	}
	for _, o := range list {
		owner := "platform"
		if o.PartnerID != nil {
			owner = *o.PartnerID
		}
		if o.StoreID != nil {
			owner = *o.StoreID
		}
		related[o.ID].contacts = byOwner[owner]
		v, e := s.renderView(o, admin, *related[o.ID])
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

func (s Service) View(ctx context.Context, q Query, o Order, admin bool) (map[string]any, error) {
	views, e := s.Views(ctx, q, []Order{o}, admin)
	if e != nil {
		return nil, e
	}
	return views[0], nil
}
