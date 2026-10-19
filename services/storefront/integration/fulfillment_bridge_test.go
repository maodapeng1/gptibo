package integration_test

import (
	"context"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"testing"
	"time"
)

func TestFulfillmentBridgePaymentOwnershipAndRefundLocks(t *testing.T) {
	owner, app := fixture(t, true)
	ctx := context.Background()
	execSQL(t, owner, `INSERT INTO catalog_plans(id,product_id,name,price_cents) VALUES('plus','chatgpt','Plus',13900)`)
	id := "SALE-BRIDGE"
	now := time.Now().UnixMilli()
	_, e := owner.DB.Exec(ctx, `INSERT INTO orders(id,owner_hash,idempotency_key,plan,plan_name,price_cents,mode,status,payment_status,created_at,updated_at,last_event_id) VALUES($1,'owner',$2,'plus','Plus',13900,'live','session','paid',$3,$3,$4)`, id, security.ID(), now, security.ID())
	if e != nil {
		t.Fatal(e)
	}
	_, e = owner.DB.Exec(ctx, `INSERT INTO fulfillment_orders(order_id,plan,region) VALUES($1,'chatgptplusplan','PH')`, id)
	if e != nil {
		t.Fatal(e)
	}
	s := fulfillment.Service{Store: app}
	if _, e = orders.Get(ctx, app.DB, id, &orders.Access{OwnerHash: "attacker"}, false); e == nil {
		t.Fatal("another guest can read order")
	}
	if ok, e := s.Authorize(ctx, id, true); e != nil || !ok {
		t.Fatal("paid order authorization failed", e)
	}
	var started bool
	if e = owner.DB.QueryRow(ctx, `SELECT execution_started FROM fulfillment_orders WHERE order_id=$1`, id).Scan(&started); e != nil || !started {
		t.Fatal("execution authorization not persisted", e)
	}
	for _, mutation := range []string{`UPDATE orders SET refund_hold=1 WHERE id=$1`, `UPDATE orders SET refund_hold=0,refunded_cents=1 WHERE id=$1`, `UPDATE orders SET refunded_cents=0,redemption_blocked=1 WHERE id=$1`, `UPDATE orders SET redemption_blocked=0,payment_status='unpaid' WHERE id=$1`} {
		if _, e = owner.DB.Exec(ctx, mutation, id); e != nil {
			t.Fatal(e)
		}
		if ok, e := s.Authorize(ctx, id, true); e != nil || ok {
			t.Fatal("unpaid/refunded/held order authorized", e)
		}
	}
}
