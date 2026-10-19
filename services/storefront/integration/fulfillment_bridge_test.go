package integration_test

import (
	"context"
	"encoding/json"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestFulfillmentSyncFailureDoesNotBlockOtherOrders(t *testing.T) {
	owner, app := fixture(t, true)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	execSQL(t, owner, `INSERT INTO catalog_plans(id,product_id,name,price_cents) VALUES('plus','chatgpt','Plus',13900)`)
	for _, id := range []string{"SALE-BAD", "SALE-GOOD"} {
		_, e := owner.DB.Exec(ctx, `INSERT INTO orders(id,owner_hash,idempotency_key,plan,plan_name,price_cents,mode,status,payment_status,created_at,updated_at,last_event_id) VALUES($1,'owner',$2,'plus','Plus',13900,'live','session','paid',$3,$3,$4)`, id, security.ID(), now, security.ID())
		if e != nil {
			t.Fatal(e)
		}
		if _, e = owner.DB.Exec(ctx, `INSERT INTO fulfillment_orders(order_id,plan,region) VALUES($1,'chatgptplusplan','PH')`, id); e != nil {
			t.Fatal(e)
		}
	}
	var fail atomic.Bool
	fail.Store(true)
	execSQL(t, owner, `UPDATE fulfillment_orders SET next_sync_at=1 WHERE order_id='SALE-GOOD'`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"salesOrderId"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.ID == "SALE-BAD" && fail.Load() {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"pending","message":"等待订阅结果","updatedAt":123}`)
	}))
	defer server.Close()
	s := fulfillment.Service{Store: app, Origin: server.URL, Secret: strings.Repeat("b", 64)}
	if e := s.Reconcile(ctx); e == nil {
		t.Fatal("query failure not reported")
	}
	var state, message string
	var failures int
	var due, checked int64
	if e := owner.DB.QueryRow(ctx, `SELECT status,sync_error,sync_failures,next_sync_at,checked_at FROM fulfillment_orders WHERE order_id='SALE-BAD'`).Scan(&state, &message, &failures, &due, &checked); e != nil {
		t.Fatal(e)
	}
	if state != "awaiting_session" || failures != 1 || message == "" || due <= now || checked != 0 {
		t.Fatal("failed query overwrote status or did not back off")
	}
	if e := owner.DB.QueryRow(ctx, `SELECT checked_at FROM fulfillment_orders WHERE order_id='SALE-GOOD'`).Scan(&checked); e != nil || checked == 0 {
		t.Fatal("second order not synchronized", e)
	}
	fail.Store(false)
	execSQL(t, owner, `UPDATE fulfillment_orders SET next_sync_at=0 WHERE order_id='SALE-BAD'`)
	if e := s.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if e := owner.DB.QueryRow(ctx, `SELECT sync_failures,sync_error FROM fulfillment_orders WHERE order_id='SALE-BAD'`).Scan(&failures, &message); e != nil || failures != 0 || message != "" {
		t.Fatal("successful retry did not clear synchronization warning", e)
	}
}
