package fulfillment

import (
	"context"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/security"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPaidRealUnblockedOrdersOnly(t *testing.T) {
	good := orders.Order{Mode: "live", Payment: "paid", Status: "session"}
	if !Eligible(good) {
		t.Fatal("paid order rejected")
	}
	tests := []orders.Order{good, good, good, good, good, good}
	tests[0].Payment = "unpaid"
	tests[1].Mode = "demo"
	tests[2].Test = 1
	tests[3].Hold = 1
	tests[4].Refunded = 1
	tests[5].Blocked = 1
	for _, o := range tests {
		if Eligible(o) {
			t.Fatal("unsafe order authorized")
		}
	}
}
func TestMerchantClientSignsExactBodyAndPath(t *testing.T) {
	secret := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := "POST\n/api/fulfillment\n" + r.Header.Get("X-Commerce-Time") + "\n" + r.Header.Get("X-Commerce-Nonce") + "\n" + string(body)
		if r.URL.Path != "/api/fulfillment" || !security.ValidID(r.Header.Get("X-Commerce-Nonce")) || !security.Equal(r.Header.Get("X-Commerce-Signature"), security.MAC([]byte(secret), text)) {
			t.Error("invalid signed request")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"awaiting_session"}`)
	}))
	defer server.Close()
	s := Service{Origin: server.URL, Secret: secret}
	var out struct {
		Status string `json:"status"`
	}
	if e := s.Call(context.Background(), map[string]string{"action": "status", "salesOrderId": "sale"}, &out); e != nil || out.Status != "awaiting_session" {
		t.Fatal("request failed", e)
	}
}
