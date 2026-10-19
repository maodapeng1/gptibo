package auth

import (
	"context"
	"golang.org/x/crypto/bcrypt"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/fault"
	"gptibo/storefront/internal/security"
	"strings"
	"time"
)

func (a *Service) PasswordReady() bool { return a.AdminConfigured() && a.cfg.AdminPasswordHash != "" }
func (a *Service) PasswordLogin(ctx context.Context, username, password, code, ip string) (string, error) {
	if !a.PasswordReady() {
		return "", fault.New(503, "管理员密码登录尚未配置。")
	}
	if e := a.Limit(ctx, "admin-password:ip:"+ip, 10, 900); e != nil {
		return "", e
	}
	if e := a.Limit(ctx, "admin-password:account", 30, 900); e != nil {
		return "", e
	}
	if len(password) > 200 || len(password) < 12 {
		return "", invalid()
	}
	matched := bcrypt.CompareHashAndPassword([]byte(a.cfg.AdminPasswordHash), []byte(password)) == nil
	if !matched || !security.Equal(strings.TrimSpace(username), a.cfg.AdminUsername) {
		return "", invalid()
	}
	step := security.VerifyTOTP(a.cfg.AdminTOTP, code, time.Now())
	if step < 0 {
		return "", invalid()
	}
	tx, e := a.Store.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	version := a.version("admin", a.cfg.AdminUsername, 0)
	tag, e := tx.Exec(ctx, `INSERT INTO admin_totp_usage(credential_version,last_step) VALUES($1,$2) ON CONFLICT(credential_version) DO UPDATE SET last_step=excluded.last_step WHERE admin_totp_usage.last_step<excluded.last_step`, version, step)
	if e != nil {
		return "", e
	}
	if tag.RowsAffected() != 1 {
		return "", invalid()
	}
	token := security.Token()
	_, e = tx.Exec(ctx, `INSERT INTO auth_sessions(token_hash,role,subject,credential_version,domain_scope) VALUES($1,'admin',$2,$3,$4)`, security.Hash(token), a.cfg.AdminUsername, version, domains.Current(ctx).AuthScope("admin"))
	if e != nil {
		return "", e
	}
	return token, tx.Commit(ctx)
}
