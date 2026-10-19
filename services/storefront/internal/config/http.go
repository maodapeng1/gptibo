package config

import (
	"errors"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"gptibo/storefront/internal/access"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/mailer"
	"gptibo/storefront/internal/turnstile"
)

type HTTP struct {
	SecurityAlertEmail string
	AdminAccess        *access.Service
	SecurityAudit      bool
	Turnstile          *turnstile.Service
	AuthTurnstile      bool
	TrafficProtection  bool
	Rehearsal          bool
	LegacyBuyer        bool
	PaymentTesting     bool
	OrderTesting       bool
	Origin             string
	Auth               auth.Config
	TrustedProxies     []string
}

func optionalSecret(name string) (string, error) {
	if os.Getenv(name) == "" && os.Getenv(name+"_FILE") == "" {
		return "", nil
	}
	return Secret(name)
}
func LoadHTTP() (HTTP, error) {
	h := HTTP{Origin: os.Getenv("PUBLIC_ORIGIN")}
	var err error
	h.TrafficProtection, err = flag("TRAFFIC_PROTECTION_ENABLED")
	if err != nil {
		return h, err
	}
	h.SecurityAudit, err = flag("SECURITY_AUDIT_ENABLED")
	if err != nil {
		return h, err
	}
	h.Rehearsal, err = flag("MIGRATION_REHEARSAL")
	if err != nil {
		return h, err
	}
	if h.Origin == "" {
		h.Origin = "http://127.0.0.1:8093"
	}
	u, e := url.Parse(h.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return h, errors.New("PUBLIC_ORIGIN must be an exact origin")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(local && u.Scheme == "http") {
		return h, errors.New("PUBLIC_ORIGIN requires HTTPS outside loopback")
	}
	if raw := os.Getenv("TRUSTED_PROXY_CIDRS"); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			prefix, err := netip.ParsePrefix(value)
			if err != nil || prefix.Bits() == 0 {
				return h, errors.New("TRUSTED_PROXY_CIDRS requires explicit CIDRs, never all addresses")
			}
			h.TrustedProxies = append(h.TrustedProxies, prefix.Masked().String())
		}
	}
	testing := os.Getenv("ORDER_TESTING_ENABLED")
	if testing != "" && testing != "false" && testing != "true" {
		return h, errors.New("ORDER_TESTING_ENABLED must be true or false")
	}
	h.OrderTesting = testing == "true" && !h.Rehearsal
	h.LegacyBuyer, e = flag("LEGACY_BUYER_COOKIE_ENABLED")
	if e != nil {
		return h, e
	}
	h.PaymentTesting, e = flag("PAYMENT_TEST_ENABLED")
	if e != nil {
		return h, e
	}
	enabledTurnstile, e := flag("TURNSTILE_ENABLED")
	if e != nil {
		return h, e
	}
	if enabledTurnstile {
		filename := os.Getenv("TURNSTILE_CONFIG_FILE")
		if filename == "" {
			return h, errors.New("TURNSTILE_CONFIG_FILE required when Turnstile is enabled")
		}
		h.Turnstile, e = turnstile.NewFile(filename)
		if e != nil {
			return h, e
		}
		if _, e = h.Turnstile.SiteKey(u.Hostname()); e != nil {
			return h, errors.New("Turnstile configuration must include PUBLIC_ORIGIN hostname")
		}
	}
	h.AuthTurnstile, e = flag("AUTH_TURNSTILE_ENABLED")
	if e != nil {
		return h, e
	}
	if h.AuthTurnstile && h.Turnstile == nil {
		return h, errors.New("AUTH_TURNSTILE_ENABLED requires TURNSTILE_ENABLED")
	}
	h.Auth.Secret, e = optionalSecret("AUTH_SECRET")
	if e != nil {
		return h, e
	}
	if h.SecurityAudit && h.Auth.Secret == "" {
		return h, errors.New("SECURITY_AUDIT_ENABLED requires AUTH_SECRET")
	}
	h.SecurityAlertEmail = os.Getenv("SECURITY_ALERT_EMAIL")
	if h.SecurityAlertEmail == "" {
		h.SecurityAlertEmail = os.Getenv("ADMIN_EMAIL")
	}
	if h.SecurityAudit {
		if _, e = auth.Email(h.SecurityAlertEmail); e != nil {
			return h, errors.New("security audit requires an alert recipient")
		}
	}
	h.Auth.AdminPasswordHash, e = optionalSecret("ADMIN_PASSWORD_HASH")
	if e != nil {
		return h, e
	}
	h.Auth.AdminUsername = os.Getenv("ADMIN_USERNAME")
	h.Auth.AdminEmail = os.Getenv("ADMIN_EMAIL")
	if os.Getenv("ADMIN_ACCESS_ISSUER") != "" || os.Getenv("ADMIN_ACCESS_AUDIENCE") != "" {
		h.AdminAccess, e = access.New(os.Getenv("ADMIN_ACCESS_ISSUER"), os.Getenv("ADMIN_ACCESS_AUDIENCE"), h.Auth.AdminEmail)
		if e != nil {
			return h, e
		}
	}
	h.Auth.AdminTOTP, e = optionalSecret("ADMIN_TOTP_SECRET")
	if e != nil {
		return h, e
	}
	enabled := strings.ToLower(os.Getenv("MAIL_ENABLED"))
	if enabled != "" && enabled != "false" && enabled != "true" {
		return h, errors.New("MAIL_ENABLED must be true or false")
	}
	if enabled == "true" && !h.Rehearsal {
		if h.Auth.Secret == "" {
			return h, errors.New("AUTH_SECRET required before enabling mail")
		}
		key, e := Secret("RESEND_API_KEY")
		if e != nil {
			return h, e
		}
		sender, e := mailer.NewResend(key, os.Getenv("MAIL_FROM"))
		if e != nil {
			return h, e
		}
		h.Auth.Sender = sender
		sender.Origin = h.Origin
	}
	return h, nil
}
