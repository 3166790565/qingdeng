// Command server 启动青灯书斋：加载配置、初始化数据库、装配路由并监听。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"qingdeng/internal/config"
	"qingdeng/internal/importer"
	"qingdeng/internal/store"
	"qingdeng/internal/tg"
	"qingdeng/internal/web"
)

func main() {
	root := flag.String("root", "", "项目根目录（含 templates/static/legado），默认按可执行文件位置推断")
	checkSQLite := flag.Bool("check-sqlite", false, "只校验 SQLite 是否支持 FTS5 + trigram 分词，然后退出")
	flag.Parse()

	if *checkSQLite {
		if err := store.CheckSQLite(); err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println("sqlite FTS5 + trigram OK")
		return
	}

	baseDir := *root
	if baseDir == "" {
		baseDir = detectRoot()
	}

	cfg, err := config.Load(baseDir)
	if err != nil {
		log.Fatalf("加载配置失败：%v", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer db.Close()

	schema, err := os.ReadFile(filepath.Join(cfg.BaseDir, "schema.sql"))
	if err != nil {
		log.Fatalf("读取 schema.sql 失败：%v", err)
	}
	if err := db.Init(string(schema)); err != nil {
		log.Fatalf("%v", err)
	}

	imp := importer.New(db, cfg)
	mgr := tg.New(cfg, db, imp)
	app, err := web.NewApp(cfg, db, imp, mgr)
	if err != nil {
		log.Fatalf("%v", err)
	}

	printStartup(cfg)
	mgr.StartScheduler() // TG 频道定期自动同步（间隔在后台设置，0 为关闭）

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 大 ZIP 上传与批量导入可能跑很久，别让读写超时把连接掐断
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("监听 http://%s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败：%v", err)
		}
	}()

	// 等 Ctrl+C / SIGTERM，给在跑的请求 15 秒收尾
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("收到退出信号，正在收尾…")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	mgr.Shutdown()
	log.Println("已退出")
}

// detectRoot 找包含 templates/ 与 schema.sql 的目录：
// 先看当前工作目录，再看可执行文件所在目录及其上级。
func detectRoot() string {
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, dir, filepath.Dir(dir))
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "templates")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "schema.sql")); err == nil {
				return dir
			}
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func printStartup(cfg *config.Config) {
	if len(cfg.EnvApplied) > 0 {
		log.Printf("已从 %s 读取：%v", cfg.EnvFile, cfg.EnvApplied)
	}
	if cfg.GeneratedAdminPassword != "" {
		log.Printf("没有配置 ADMIN_PASSWORD，本次启动的随机管理员密码：%s（写进 %s 可长期固定）",
			cfg.GeneratedAdminPassword, cfg.EnvFile)
	}
	if cfg.GeneratedSecretKey {
		log.Printf("没有配置 SECRET_KEY，本次启动随机生成，重启后已登录的会话会失效")
	}
	if cfg.APIToken == "" {
		log.Printf("API_TOKEN 未设置，/api/* 为公开只读接口（书源抓取需要如此）")
	}
	if !cfg.TGReady() {
		log.Printf("未配置 TG_API_ID / TG_API_HASH，TG 频道导入不可用")
	}
	log.Printf("数据库 %s", cfg.DBPath)
}
