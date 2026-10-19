package config

import (
	"encoding/json"
	"errors"
	"gptibo/storefront/internal/fulfillment"
	"net/url"
	"os"
	"regexp"
)

func LoadFulfillment() (*fulfillment.Service, error) {
	enabled, e := flag("FULFILLMENT_ENABLED")
	if e != nil || !enabled {
		return nil, e
	}
	secret, e := Secret("FULFILLMENT_SECRET")
	if e != nil {
		return nil, e
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(secret) {
		return nil, errors.New("FULFILLMENT_SECRET must be 32 random bytes in lowercase hex")
	}
	origin := os.Getenv("FULFILLMENT_ORIGIN")
	u, e := url.Parse(origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("FULFILLMENT_ORIGIN must be an HTTPS origin")
	}
	products := map[string]fulfillment.Product{}
	if json.Unmarshal([]byte(os.Getenv("FULFILLMENT_PRODUCT_MAP")), &products) != nil || len(products) == 0 {
		return nil, errors.New("FULFILLMENT_PRODUCT_MAP is required")
	}
	for plan, p := range products {
		if plan != "plus" || p.Plan != "chatgptplusplan" || !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(p.Region) {
			return nil, errors.New("initial fulfillment release supports only mapped Plus products")
		}
	}
	return &fulfillment.Service{Secret: secret, Origin: origin, Products: products}, nil
}
