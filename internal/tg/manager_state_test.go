package tg

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"qingdeng/internal/config"
	"qingdeng/internal/store"
)

func TestSessionFileWithoutPhoneIsNotLoggedIn(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &Manager{db: db, sessionPath: filepath.Join(t.TempDir(), "tg.session")}
	if err := os.WriteFile(m.sessionPath, []byte("anonymous session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.LoggedIn() {
		t.Fatal("只有 MTProto 会话文件、没有已授权手机号时不能视为已登录")
	}
}

func TestPendingLoginIsNotLoggedIn(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &Manager{
		db:          db,
		sessionPath: filepath.Join(t.TempDir(), "tg.session"),
		pending: &pendingLogin{
			step:      "code",
			expiresAt: time.Now().Add(time.Minute),
		},
	}
	if err := os.WriteFile(m.sessionPath, []byte("anonymous session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.LoggedIn() {
		t.Fatal("匿名会话在等待验证码时不能视为已登录")
	}
}

func TestEnsureClientDoesNotReplacePendingLoginClient(t *testing.T) {
	m := &Manager{
		cfg: &config.Config{TGAPIID: 1, TGAPIHash: "hash"},
		pending: &pendingLogin{
			step:      "code",
			expiresAt: time.Now().Add(time.Minute),
		},
	}
	if _, err := m.ensureClient(context.Background()); err == nil || err.Error() != "Telegram 登录尚未完成，请先验证验证码或云密码" {
		t.Fatalf("等待验证码时应阻止授权客户端重建，得到：%v", err)
	}
}
