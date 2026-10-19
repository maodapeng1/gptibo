package integration_test

import (
	"context"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/fulfillment"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/vouchers"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func voucherFixture(t *testing.T) (vouchers.Service, []map[string]any) {
	_, app := fixture(t, true)
	a, e := auth.New(app, auth.Config{Secret: strings.Repeat("a", 64)})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"ticket": strings.Repeat("t", 44)})
	}))
	t.Cleanup(server.Close)
	s := vouchers.Service{Store: app, Auth: a, Delivery: &fulfillment.Service{Store: app, Origin: server.URL, Secret: strings.Repeat("b", 64), Products: map[string]fulfillment.Product{"plus": {Plan: "chatgptplusplan", Region: "PH"}}}}
	batch, e := s.Batch(context.Background(), "operator", vouchers.BatchInput{Count: 2, ValidDays: 30, Title: "isolated-test"})
	if e != nil {
		t.Fatal(e)
	}
	return s, batch["codes"].([]map[string]any)
}
func sellVoucher(t *testing.T, s vouchers.Service, id string, ref string) string {
	t.Helper()
	r, e := s.Sell(context.Background(), "operator", vouchers.SaleInput{Confirmed: true, VoucherID: id, Amount: 10000, Channel: "wechat", Account: "test-account", Reference: ref, Note: "仅隔离测试"})
	if e != nil {
		t.Fatal(e)
	}
	return r["orderId"].(string)
}
func TestVoucherSalesConcurrencyAndRecovery(t *testing.T) {
	s, codes := voucherFixture(t)
	ctx := context.Background()
	id, code := codes[0]["id"].(string), codes[0]["code"].(string)
	if _, e := s.Preview(ctx, "one", code); e == nil {
		t.Fatal("inactive code accepted")
	}
	order := sellVoucher(t, s, id, "receipt-1")
	if _, e := s.Sell(ctx, "operator", vouchers.SaleInput{Confirmed: true, VoucherID: codes[1]["id"].(string), Amount: 10000, Channel: "wechat", Account: "test-account", Reference: "receipt-1", Note: "duplicate"}); e == nil {
		t.Fatal("receipt reused")
	}
	for _, buyer := range []string{"one", "two"} {
		if _, e := s.Preview(ctx, buyer, code); e != nil {
			t.Fatal(e)
		}
	}
	var a, b string
	if e := s.Store.DB.QueryRow(ctx, `SELECT id FROM voucher_contexts WHERE buyer_hash='one'`).Scan(&a); e != nil {
		t.Fatal(e)
	}
	if e := s.Store.DB.QueryRow(ctx, `SELECT id FROM voucher_contexts WHERE buyer_hash='two'`).Scan(&b); e != nil {
		t.Fatal(e)
	}
	operation := security.ID()
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for _, c := range []string{a, b} {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			_, e := s.Confirm(ctx, vouchers.ConfirmInput{ContextID: c, OrderID: order, OperationID: operation, AccountHash: strings.Repeat("a", 44)})
			if e == nil {
				results <- c
			} else {
				results <- ""
			}
		}(c)
	}
	wg.Wait()
	close(results)
	won := ""
	n := 0
	for c := range results {
		if c != "" {
			n++
			won = c
		}
	}
	if n != 1 {
		t.Fatal("concurrent redemption did not select exactly one owner", n)
	}
	var buyer string
	if e := s.Store.DB.QueryRow(ctx, `SELECT buyer_hash FROM voucher_contexts WHERE id=$1`, won).Scan(&buyer); e != nil {
		t.Fatal(e)
	}
	if _, e := orders.Get(ctx, s.Store.DB, order, &orders.Access{OwnerHash: buyer}, false); e != nil {
		t.Fatal("winner cannot read", e)
	}
	other := "one"
	if buyer == other {
		other = "two"
	}
	if _, e := orders.Get(ctx, s.Store.DB, order, &orders.Access{OwnerHash: other}, false); e == nil {
		t.Fatal("loser can read order")
	}
	if _, e := s.Confirm(ctx, vouchers.ConfirmInput{ContextID: won, OrderID: order, OperationID: operation, AccountHash: strings.Repeat("a", 44)}); e != nil {
		t.Fatal("same confirmation is not idempotent", e)
	}
	if ok, e := s.Delivery.AuthorizeContext(ctx, order, true, won, false); e != nil || !ok {
		t.Fatal("bound operation not authorized", e)
	}
	if ok, e := s.Delivery.Authorize(ctx, order, true); e != nil || ok {
		t.Fatal("unscoped ticket bypassed binding", e)
	}
	if _, e := s.Action(ctx, "operator", id, vouchers.ActionInput{Action: "refund", Note: "must reject unknown", Reference: "refund-1", Confirmed: true, Version: 2}); e == nil {
		t.Fatal("in-flight operation refunded")
	}
	secret, e := s.Recovery(ctx, order, buyer)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Recover(ctx, order, "new-device", security.Token()); fault.Status(e) != 401 {
		t.Fatal("wrong recovery secret accepted", e)
	}
	if e = s.Recover(ctx, order, "new-device", secret); e != nil {
		t.Fatal(e)
	}
	if _, e = orders.Get(ctx, s.Store.DB, order, &orders.Access{OwnerHash: "new-device"}, false); e != nil {
		t.Fatal(e)
	}
	var roots, dispatch int
	s.Store.DB.QueryRow(ctx, `SELECT count(*) FROM voucher_redemptions`).Scan(&roots)
	s.Store.DB.QueryRow(ctx, `SELECT count(*) FROM voucher_dispatch`).Scan(&dispatch)
	if roots != 1 || dispatch != 1 {
		t.Fatal("multiple roots or dispatches", roots, dispatch)
	}
}
func TestVoucherReplacementRevokesOldPreviewAndRefundRevokesEntitlement(t *testing.T) {
	s, codes := voucherFixture(t)
	ctx := context.Background()
	id := codes[0]["id"].(string)
	oldCode := codes[0]["code"].(string)
	order := sellVoucher(t, s, id, "receipt-replace")
	if _, e := s.Preview(ctx, "old-browser", oldCode); e != nil {
		t.Fatal(e)
	}
	var oldContext string
	s.Store.DB.QueryRow(ctx, `SELECT id FROM voucher_contexts`).Scan(&oldContext)
	result, e := s.Action(ctx, "operator", id, vouchers.ActionInput{Action: "replace", Version: 1, Note: "未使用权益补发测试", Confirmed: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Preview(ctx, "old-browser", oldCode); e == nil {
		t.Fatal("old code still valid")
	}
	if ok, e := s.Delivery.AuthorizeContext(ctx, order, false, oldContext, true); e != nil || ok {
		t.Fatal("old preview still authorized", e)
	}
	newCode := result["code"].(string)
	newID := result["voucherId"].(string)
	if _, e = s.Preview(ctx, "new", newCode); e != nil {
		t.Fatal("replacement lost paid entitlement", e)
	}
	if _, e = s.Action(ctx, "operator", newID, vouchers.ActionInput{Action: "refund", Version: 0, Note: "已实际全额退款，隔离测试", Reference: "refund-replace", Confirmed: true}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Preview(ctx, "new", newCode); e == nil {
		t.Fatal("refunded code accepted")
	}
	var refunded int64
	if e = s.Store.DB.QueryRow(ctx, `SELECT refunded_cents FROM orders WHERE id=$1`, order).Scan(&refunded); e != nil || refunded != 10000 {
		t.Fatal("cash refund not recorded", e, refunded)
	}
}
func TestAdminPasswordTOTPWithoutMailAndReplayRejection(t *testing.T) {
	_, app := fixture(t, true)
	hash, e := bcrypt.GenerateFromPassword([]byte("isolated-test-password"), 12)
	if e != nil {
		t.Fatal(e)
	}
	cfg := auth.Config{Secret: strings.Repeat("a", 64), AdminUsername: "operator", AdminPasswordHash: string(hash), AdminTOTP: strings.Repeat("A", 32)}
	a, e := auth.New(app, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if a.Ready() || !a.PasswordReady() {
		t.Fatal("mail-free password mode invalid")
	}
	code := security.TOTP(cfg.AdminTOTP, time.Now().Unix()/30)
	if _, e = a.PasswordLogin(context.Background(), "operator", "wrong-password-value", code, "127.0.0.1"); fault.Status(e) != 401 {
		t.Fatal("bad password accepted", e)
	}
	token, e := a.PasswordLogin(context.Background(), "operator", "isolated-test-password", code, "127.0.0.1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.Require(context.Background(), app.DB, "admin", token); e != nil {
		t.Fatal(e)
	}
	if _, e = a.PasswordLogin(context.Background(), "operator", "isolated-test-password", code, "127.0.0.1"); fault.Status(e) != 401 {
		t.Fatal("TOTP replay accepted", e)
	}
	hash2, _ := bcrypt.GenerateFromPassword([]byte("rotated-test-password"), 12)
	cfg.AdminPasswordHash = string(hash2)
	changed, e := auth.New(app, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = changed.Require(context.Background(), app.DB, "admin", token); fault.Status(e) != 401 {
		t.Fatal("password rotation did not revoke old session", e)
	}
}

func TestVoucherActivationRequiresReceiptAttestationAndPrivateListing(t *testing.T) {
	s, codes := voucherFixture(t)
	ctx := context.Background()
	id := codes[0]["id"].(string)
	if _, e := s.Sell(ctx, "operator", vouchers.SaleInput{VoucherID: id, Amount: 10000, Channel: "wechat", Account: "test", Reference: "proof", Note: "not confirmed"}); fault.Status(e) != 400 {
		t.Fatal("unconfirmed receipt activated code", e)
	}
	order := sellVoucher(t, s, id, "private-list")
	rows, e := s.List(ctx, "private", "")
	if e != nil {
		t.Fatal(e)
	}
	_ = rows
	rows, e = s.List(ctx, order, "active")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), codes[0]["code"].(string)) || strings.Contains(string(raw), "code_hash") {
		t.Fatal("private code leaked in listing")
	}
	if !strings.Contains(string(raw), "transaction_reference") {
		t.Fatal("operator cannot inspect receipt")
	}
	if _, e = s.Action(ctx, "operator", id, vouchers.ActionInput{Action: "recover_access", Version: 1, Reference: "proof", Note: "not yet redeemed", Confirmed: true}); fault.Status(e) != 409 {
		t.Fatal("unbound sale was granted recovery access", e)
	}
}
