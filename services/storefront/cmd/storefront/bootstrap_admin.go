package main

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"gptibo/storefront/internal/security"
	"os"
	"path/filepath"
)

// Explicit local bootstrap; never exposed through an HTTP endpoint or logs.
func bootstrapVoucherAdmin() error {
	dir := os.Getenv("VOUCHER_ADMIN_SECRET_DIR")
	if dir == "" {
		return errors.New("VOUCHER_ADMIN_SECRET_DIR is required")
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	names := []string{"voucher-admin-password-hash", "voucher-admin-totp", "voucher-admin-access.txt"}
	existing := 0
	for _, name := range names {
		if _, e := os.Stat(filepath.Join(dir, name)); e == nil {
			existing++
		}
	}
	if existing == len(names) {
		fmt.Println("Administrator setup already exists; credentials unchanged.")
		return nil
	}
	if existing != 0 {
		return errors.New("partial administrator setup; preserve and reconcile private files")
	}
	password := security.Token()[:32]
	hash, e := bcrypt.GenerateFromPassword([]byte(password), 12)
	if e != nil {
		return e
	}
	key := make([]byte, 20)
	if _, e = rand.Read(key); e != nil {
		return e
	}
	totp := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	values := []string{string(hash) + "\n", totp + "\n", "后台：https://shop.aichongzhi.best/voucher-admin\n账号：voucher-admin\n密码：" + password + "\nTOTP 密钥：" + totp + "\n请将 TOTP 密钥添加到验证器应用，登录时同时输入密码与六位动态验证码。\n妥善保存此文件，不要提交仓库或发送给他人。\n"}
	for i, name := range names {
		mode := os.FileMode(0444)
		if i == 2 {
			mode = 0600
		}
		f, e := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if e != nil {
			return e
		}
		_, e = f.WriteString(values[i])
		if e == nil {
			e = f.Chmod(mode)
		}
		f.Close()
		if e != nil {
			return e
		}
	}
	fmt.Println("Administrator setup written to private files. No credentials printed.")
	return nil
}
