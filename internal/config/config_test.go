package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadCreatesDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	t.Setenv("NOVEL_DATA_DIR", dataDir)
	t.Setenv("NOVEL_DB_PATH", "")

	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, dataDir)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("数据目录没建出来：%v", err)
	}
}

// 不可写的数据目录要在启动时就报错，并给出 chown 提示。
func TestLoadRejectsUnwritableDataDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 无视权限位，这条测不出来")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的权限位不按 POSIX 语义生效，这条在 CI/Linux 上验")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o500); err != nil { // 只读目录
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dataDir, 0o755) })
	t.Setenv("NOVEL_DATA_DIR", dataDir)
	t.Setenv("NOVEL_DB_PATH", "")

	_, err := Load(root)
	if err == nil {
		t.Fatal("只读目录应报错")
	}
	if !strings.Contains(err.Error(), "不可写") || !strings.Contains(err.Error(), "chown") {
		t.Fatalf("错误信息应包含可执行的修复提示，得到：%v", err)
	}
}

func TestTGProxyDefault(t *testing.T) {
	t.Setenv("TG_PROXY_URL", "")
	t.Setenv("TG_PROXY_TIMEOUT", "")
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TGProxyURL != "" || cfg.TGProxyTimeout.Seconds() != 30 {
		t.Fatalf("代理默认值不对：%q %v", cfg.TGProxyURL, cfg.TGProxyTimeout)
	}
}

// 代理地址刻意不从环境变量读：它在后台页面管理，唯一来源是数据库。
// 设了 TG_PROXY_URL 也不该生效，防止出现两个来源。
func TestTGProxyNotReadFromEnv(t *testing.T) {
	t.Setenv("TG_PROXY_URL", "socks5://127.0.0.1:7890")
	t.Setenv("TG_PROXY_TIMEOUT", "10")
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TGProxyURL != "" {
		t.Fatalf("代理地址不该从环境变量读取，得到 %q", cfg.TGProxyURL)
	}
	if cfg.TGProxyTimeout.Seconds() != 10 {
		t.Fatalf("超时仍应从环境变量读取：%v", cfg.TGProxyTimeout)
	}
}
