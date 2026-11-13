// Package config 集中定义路径、密钥与可选开关。
//
// 读取优先级：系统环境变量 > 项目根目录的 .env > 内置默认值。
// 管理员密码与 SECRET_KEY 没有默认值，缺了就每次启动随机生成并打印到日志。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 是进程级的只读配置，启动时加载一次。
type Config struct {
	BaseDir     string
	DataDir     string
	DBPath      string
	TemplateDir string
	StaticDir   string
	LegadoDir   string

	SiteName    string
	SiteTagline string
	SiteBaseURL string

	AdminPassword string
	// GeneratedAdminPassword 非空表示密码是本次启动随机生成的，需要提示管理员。
	GeneratedAdminPassword string
	SecretKey              string
	GeneratedSecretKey     bool
	SessionCookie          string
	SessionMaxAge          int // 秒

	APIToken string

	TGAPIID   int
	TGAPIHash string

	Host string
	Port int

	// .env 实际生效的键，仅用于启动日志
	EnvFile    string
	EnvApplied []string
}

const (
	// MaxTxtBytes 单本 txt 上限，TG 单文件下载也用它兜底
	MaxTxtBytes = 64 * 1024 * 1024
	// MaxZipEntryBytes 单个解压条目上限
	MaxZipEntryBytes = 64 * 1024 * 1024
	// MaxZipTotalBytes 解压总体积上限
	MaxZipTotalBytes = 512 * 1024 * 1024
	// MaxZipEntries 压缩包条目数上限
	MaxZipEntries = 2000

	ShelfPageSize   = 24
	CatalogPageSize = 100
	AdminPageSize   = 30
	APIPageSize     = 20
)

// Load 读取配置。root 是项目根目录（含 templates/static/legado 的那一级）。
func Load(root string) (*Config, error) {
	cfg := &Config{
		BaseDir:       root,
		SiteName:      "青灯书斋",
		SiteTagline:   "一盏灯，一本书",
		SessionCookie: "novel_admin",
		SessionMaxAge: 7 * 24 * 3600,
		Host:          "127.0.0.1",
		Port:          8000,
	}

	envFile := os.Getenv("NOVEL_ENV_FILE")
	if envFile == "" {
		envFile = filepath.Join(root, ".env")
	}
	cfg.EnvFile = envFile
	cfg.EnvApplied = loadEnvFile(envFile)

	dataDir := os.Getenv("NOVEL_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(root, "data")
	}
	cfg.DataDir = absPath(root, dataDir)

	dbPath := os.Getenv("NOVEL_DB_PATH")
	if dbPath == "" {
		dbPath = filepath.Join(cfg.DataDir, "novel.db")
	}
	cfg.DBPath = absPath(root, dbPath)

	cfg.TemplateDir = filepath.Join(root, "templates")
	cfg.StaticDir = filepath.Join(root, "static")
	cfg.LegadoDir = filepath.Join(root, "legado")

	cfg.SiteName = envOr("SITE_NAME", cfg.SiteName)
	cfg.SiteTagline = envOr("SITE_TAGLINE", cfg.SiteTagline)
	cfg.SiteBaseURL = strings.TrimRight(envOr("SITE_BASE_URL", "http://127.0.0.1:8000"), "/")

	cfg.AdminPassword = strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
	if cfg.AdminPassword == "" {
		cfg.GeneratedAdminPassword = randomToken(12)
		cfg.AdminPassword = cfg.GeneratedAdminPassword
	}

	cfg.SecretKey = strings.TrimSpace(os.Getenv("SECRET_KEY"))
	if cfg.SecretKey == "" {
		cfg.GeneratedSecretKey = true
		cfg.SecretKey = randomToken(32)
	}

	cfg.APIToken = strings.TrimSpace(os.Getenv("API_TOKEN"))

	cfg.TGAPIID, _ = strconv.Atoi(strings.TrimSpace(os.Getenv("TG_API_ID")))
	cfg.TGAPIHash = strings.TrimSpace(os.Getenv("TG_API_HASH"))

	cfg.Host = envOr("HOST", cfg.Host)
	if port, err := strconv.Atoi(os.Getenv("PORT")); err == nil && port > 0 {
		cfg.Port = port
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s 失败：%w", cfg.DataDir, err)
	}
	return cfg, nil
}

// TGReady 表示 Telegram 应用凭证是否已配置。
func (c *Config) TGReady() bool {
	return c.TGAPIID != 0 && c.TGAPIHash != ""
}

// loadEnvFile 把 .env 里的键值填进进程环境，返回真正生效的键名。
//
// 系统环境变量优先：已存在的键一律不覆盖。
// 支持 # 整行注释、可选的 export 前缀、成对的引号；不支持行尾注释
// （密码里带 # 很正常，截断了更麻烦）。
func loadEnvFile(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var applied []string
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[0] == value[len(value)-1] {
			value = value[1 : len(value)-1]
		}
		os.Setenv(key, value)
		applied = append(applied, key)
	}
	return applied
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func absPath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// randomToken 生成 URL 安全的随机串，用于随机管理员密码与 SECRET_KEY。
func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用：" + err.Error())
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(buf), "=")
}
