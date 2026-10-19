package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/store"
)

type Sender interface {
	SendCode(context.Context, string, string, string, string) error
}

type InvitationSender interface {
	SendInvitation(context.Context, string, string, string) error
}

func (a *Service) Invite(ctx context.Context, id, origin string) error {
	sender, ok := a.cfg.Sender.(InvitationSender)
	if !a.Ready() || !ok {
		return fault.New(503, "邀请邮件尚未配置。")
	}
	if e := a.Limit(ctx, "partner-invitation:"+id, 1, 60); e != nil {
		return e
	}
	var email string
	e := a.Store.DB.QueryRow(ctx, `SELECT email FROM partners WHERE id=$1 AND active`, id).Scan(&email)
	if errors.Is(e, pgx.ErrNoRows) {
		return fault.New(409, "请先启用代理再发送邀请邮件。")
	}
	if e != nil {
		return e
	}
	sendCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if e = sender.SendInvitation(sendCtx, email, origin, security.ID()); e != nil {
		return fault.New(503, "邀请邮件发送结果未确认，请稍后核实再重试。")
	}
	return nil
}

type Config struct {
	Secret, AdminUsername, AdminEmail, AdminTOTP string
	AdminPasswordHash                            string
	Sender                                       Sender
}
type Service struct {
	Store *store.Store
	cfg   Config
	key   []byte
}
type Principal struct{ Role, Subject, Version string }
type SendInput struct {
	Shop           string `json:"shop"`
	TurnstileToken string `json:"turnstileToken"`
	Username       string `json:"username"`
	Email          string `json:"email"`
}
type VerifyInput struct {
	TurnstileToken string `json:"turnstileToken"`
	Username       string `json:"username"`
	Email          string `json:"email"`
	Challenge      string `json:"challenge"`
	EmailCode      string `json:"emailCode"`
	Code           string `json:"code"`
}

func New(s *store.Store, cfg Config) (*Service, error) {
	a := &Service{Store: s, cfg: cfg}
	if cfg.Secret != "" {
		key, e := hex.DecodeString(cfg.Secret)
		if e != nil || len(key) != 32 {
			return nil, errors.New("AUTH_SECRET requires 32 random bytes encoded as hex")
		}
		a.key = key
	}
	if cfg.AdminPasswordHash != "" {
		cost, e := bcrypt.Cost([]byte(cfg.AdminPasswordHash))
		if e != nil || cost < 12 {
			return nil, errors.New("ADMIN_PASSWORD_HASH requires bcrypt cost >= 12")
		}
	}
	if cfg.AdminUsername != "" || cfg.AdminEmail != "" || cfg.AdminTOTP != "" || cfg.AdminPasswordHash != "" {
		email, e := Email(cfg.AdminEmail)
		if len(cfg.AdminUsername) == 0 || len(cfg.AdminUsername) > 80 || !security.ValidTOTP(cfg.AdminTOTP) || (e != nil && (cfg.AdminPasswordHash == "" || cfg.AdminEmail != "")) {
			return nil, errors.New("complete administrator username, email and TOTP configuration required")
		}
		a.cfg.AdminEmail = email
	}
	return a, nil
}
func Email(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, e := mail.ParseAddress(value)
	_, domain, hasAt := strings.Cut(value, "@")
	if e != nil || parsed.Address != value || len(value) > 254 || !hasAt || !strings.Contains(domain, ".") {
		return "", fault.New(400, "请输入有效的邮箱地址。")
	}
	return value, nil
}
func (a *Service) Ready() bool { return a != nil && len(a.key) == 32 && a.cfg.Sender != nil }
func (a *Service) AdminConfigured() bool {
	return a != nil && len(a.key) == 32 && a.cfg.AdminUsername != "" && security.ValidTOTP(a.cfg.AdminTOTP)
}
func (a *Service) Username() string { return a.cfg.AdminUsername }
func (a *Service) version(role, subject string, version int64) string {
	value := fmt.Sprintf("%s:%s:%d", role, subject, version)
	if role == "admin" {
		value += ":" + a.cfg.AdminEmail + ":" + a.cfg.AdminTOTP
		if a.cfg.AdminPasswordHash != "" {
			value += ":" + a.cfg.AdminPasswordHash
		}
	}
	return security.MAC(a.key, "credential:"+value)
}
func (a *Service) codeHash(role, id, code string) string {
	return security.MAC(a.key, "challenge:"+role+":"+id+":"+code)
}
func (a *Service) Limit(ctx context.Context, key string, max int, seconds int) error {
	return a.LimitIn(ctx, a.Store.DB, key, max, seconds)
}

// Reuse a caller's transaction instead of consuming a second pooled connection.
func (a *Service) LimitIn(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string, max, seconds int) error {
	var n int
	e := q.QueryRow(ctx, `INSERT INTO auth_limits(key,attempts,reset_at) VALUES($1,1,now()+$2*interval '1 second')
 ON CONFLICT(key) DO UPDATE SET attempts=CASE WHEN auth_limits.reset_at<=now() THEN 1 ELSE least(auth_limits.attempts+1,1000000) END,
 reset_at=CASE WHEN auth_limits.reset_at<=now() THEN excluded.reset_at ELSE auth_limits.reset_at END RETURNING attempts`, security.MAC(a.key, key), seconds).Scan(&n)
	if e != nil {
		return e
	}
	if n > max {
		return fault.New(429, "操作过于频繁，请稍后重试。")
	}
	return nil
}
func invalid() error { return fault.New(401, "登录信息无效、验证码已使用或已过期。") }

func (a *Service) Send(ctx context.Context, role string, in SendInput, ip string) (string, error) {
	if !a.Ready() || (role == "admin" && !a.AdminConfigured()) {
		return "", fault.New(503, "邮箱验证尚未配置。")
	}
	subject, email, version := "", "", int64(0)
	switch role {
	case "admin":
		if strings.TrimSpace(in.Username) == "" || len(in.Username) > 80 {
			return "", fault.New(400, "请填写管理员用户名。")
		}
		subject = in.Username
		email = a.cfg.AdminEmail
	case "partner", "customer":
		var e error
		email, e = Email(in.Email)
		if e != nil {
			return "", e
		}
		subject = email
	default:
		return "", fault.New(400, "无效登录类型。")
	}
	// Apply identical recipient throttles even for unknown accounts.
	for _, limit := range []struct {
		key          string
		max, seconds int
	}{{role + ":send:ip:" + ip, 10, 3600}, {role + ":send:minute:" + subject, 1, 60}, {role + ":send:hour:" + subject, 5, 3600}} {
		if e := a.Limit(ctx, limit.key, limit.max, limit.seconds); e != nil {
			return "", e
		}
	}
	id := security.ID()
	if role == "admin" && !security.Equal(in.Username, a.cfg.AdminUsername) {
		return id, nil
	}
	if role == "partner" {
		e := a.Store.DB.QueryRow(ctx, "SELECT id,auth_version FROM partners WHERE email=$1 AND active AND ($2='' OR id=$2 OR parent_id=$2)", email, domains.Current(ctx).Owner()).Scan(&subject, &version)
		if errors.Is(e, pgx.ErrNoRows) {
			return id, nil
		}
		if e != nil {
			return "", e
		}
	}
	credential := a.version(role, subject, version)
	code := security.Code()
	tx, e := a.Store.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", role+":"+subject); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM auth_challenges WHERE role=$1 AND subject=$2 AND domain_scope=$3", role, subject, domains.Current(ctx).AuthScope(role)); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO auth_challenges(id,role,subject,credential_version,code_hash,domain_scope) VALUES($1,$2,$3,$4,$5,$6)", id, role, subject, credential, a.codeHash(role, id, code), domains.Current(ctx).AuthScope(role)); e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	sendCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	e = a.cfg.Sender.SendCode(sendCtx, role, email, code, id)
	cancel()
	if e != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = a.Store.DB.Exec(cleanup, "DELETE FROM auth_challenges WHERE id=$1", id)
		return "", fault.New(503, "验证码发送失败，请稍后重试。")
	}
	tag, e := a.Store.DB.Exec(ctx, "UPDATE auth_challenges SET ready=true WHERE id=$1 AND expires_at>now()", id)
	if e != nil {
		return "", e
	}
	if tag.RowsAffected() != 1 {
		return "", fault.New(409, "验证码已更新，请重新获取。")
	}
	return id, nil
}

func (a *Service) Verify(ctx context.Context, role string, in VerifyInput, ip string) (string, error) {
	if !a.Ready() || (role == "admin" && !a.AdminConfigured()) {
		return "", fault.New(503, "邮箱验证尚未配置。")
	}
	if e := a.Limit(ctx, role+":verify:ip:"+ip, 20, 900); e != nil {
		return "", e
	}
	if role == "admin" {
		if e := a.Limit(ctx, "admin:verify:account", 30, 900); e != nil {
			return "", e
		}
	}
	id := strings.ToLower(in.Challenge)
	if role == "partner" && in.Email != "" {
		email, e := Email(in.Email)
		if e != nil {
			return "", invalid()
		}
		if e = a.Limit(ctx, "partner:verify:email:"+email, 10, 900); e != nil {
			return "", e
		}
		e = a.Store.DB.QueryRow(ctx, `SELECT c.id::text FROM auth_challenges c JOIN partners p ON p.id=c.subject
 WHERE c.role='partner' AND p.email=$1 AND p.active AND NOT c.used AND c.ready AND c.expires_at>now() ORDER BY c.created_at DESC LIMIT 1`, email).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			return "", invalid()
		}
		if e != nil {
			return "", e
		}
	}
	code := in.Code
	if role == "admin" {
		code = in.EmailCode
	}
	if !security.ValidID(id) || !security.ValidCode(code) {
		return "", invalid()
	}
	var subject, credential, hash string
	e := a.Store.DB.QueryRow(ctx, `UPDATE auth_challenges SET attempts=attempts+1 WHERE id=$1 AND role=$2 AND domain_scope=$3 AND ready AND NOT used AND attempts<5 AND expires_at>now()
 RETURNING subject,credential_version,code_hash`, id, role, domains.Current(ctx).AuthScope(role)).Scan(&subject, &credential, &hash)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", invalid()
	}
	if e != nil {
		return "", e
	}
	if !security.Equal(hash, a.codeHash(role, id, code)) {
		return "", invalid()
	}
	step := int64(-1)
	if role == "admin" {
		if in.Username != a.cfg.AdminUsername || subject != a.cfg.AdminUsername {
			return "", invalid()
		}
		step = security.VerifyTOTP(a.cfg.AdminTOTP, in.Code, time.Now())
		if step < 0 {
			return "", invalid()
		}
	}
	tx, e := a.Store.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	p := Principal{role, subject, credential}
	if e = a.ValidatePrincipal(ctx, tx, p); e != nil {
		return "", e
	}
	tag, e := tx.Exec(ctx, "UPDATE auth_challenges SET used=true WHERE id=$1 AND role=$2 AND NOT used AND ready AND attempts<=5 AND expires_at>now()", id, role)
	if e != nil {
		return "", e
	}
	if tag.RowsAffected() != 1 {
		return "", invalid()
	}
	if role == "admin" {
		tag, e = tx.Exec(ctx, `INSERT INTO admin_totp_usage(credential_version,last_step) VALUES($1,$2)
 ON CONFLICT(credential_version) DO UPDATE SET last_step=excluded.last_step WHERE admin_totp_usage.last_step<excluded.last_step`, credential, step)
		if e != nil {
			return "", e
		}
		if tag.RowsAffected() != 1 {
			return "", invalid()
		}
	}
	token := security.Token()
	if _, e = tx.Exec(ctx, "INSERT INTO auth_sessions(token_hash,role,subject,credential_version,domain_scope) VALUES($1,$2,$3,$4,$5)", security.Hash(token), role, subject, credential, domains.Current(ctx).AuthScope(role)); e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	return token, nil
}

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (a *Service) ValidatePrincipal(ctx context.Context, q Querier, p Principal) error {
	if a == nil || len(a.key) != 32 {
		return invalid()
	}
	var v int64
	switch p.Role {
	case "partner":
		e := q.QueryRow(ctx, "SELECT auth_version FROM partners WHERE id=$1 AND active AND ($2='' OR id=$2 OR parent_id=$2) FOR SHARE", p.Subject, domains.Current(ctx).Owner()).Scan(&v)
		if errors.Is(e, pgx.ErrNoRows) {
			return invalid()
		}
		if e != nil {
			return e
		}
	case "admin":
		if !a.AdminConfigured() || p.Subject != a.cfg.AdminUsername {
			return invalid()
		}
	case "customer":
	default:
		return invalid()
	}
	if !security.Equal(p.Version, a.version(p.Role, p.Subject, v)) {
		return invalid()
	}
	return nil
}
func (a *Service) Require(ctx context.Context, q Querier, role, token string) (Principal, error) {
	p := Principal{Role: role}
	if a == nil || len(a.key) != 32 || len(token) != 64 {
		return p, invalid()
	}
	e := q.QueryRow(ctx, "SELECT subject,credential_version FROM auth_sessions WHERE token_hash=$1 AND role=$2 AND domain_scope=$3 AND expires_at>now()", security.Hash(token), role, domains.Current(ctx).AuthScope(role)).Scan(&p.Subject, &p.Version)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, invalid()
	}
	if e != nil {
		return p, e
	}
	return p, a.ValidatePrincipal(ctx, q, p)
}
func (a *Service) Logout(ctx context.Context, role, token string) error {
	_, e := a.Store.DB.Exec(ctx, "DELETE FROM auth_sessions WHERE token_hash=$1 AND role=$2 AND domain_scope=$3", security.Hash(token), role, domains.Current(ctx).AuthScope(role))
	return e
}

// Fingerprint is domain-separated and keyed; it must never expose session tokens.
func (a *Service) Fingerprint(scope, value string) string {
	if value == "" {
		return ""
	}
	return security.MAC(a.key, "audit:"+scope+":"+value)
}
