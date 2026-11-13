// Package store 提供 SQLite 连接管理与 schema 初始化。
//
// 用的是 modernc.org/sqlite（纯 Go，无 cgo），编译产物可以直接交叉编译到
// Linux 服务器，不需要目标机装 sqlite 开发包。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB 包装 *sql.DB，附带 schema 路径。
type DB struct {
	*sql.DB
	path string
}

// Open 打开（并在需要时创建）库文件，设置 WAL 等 pragma。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建库目录失败：%w", err)
	}
	// _pragma 参数在连接建立时执行，比连接池里逐条 PRAGMA 更可靠
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单进程内并发写会互相等锁，限制连接数让 SQLite 的串行性可控。
	// 读多写少的场景下 8 个连接足够，后台任务也能跑开。
	raw.SetMaxOpenConns(8)
	raw.SetMaxIdleConns(4)
	raw.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("打开数据库失败：%w", err)
	}
	return &DB{DB: raw, path: path}, nil
}

// Path 返回库文件路径。
func (d *DB) Path() string { return d.path }

// Init 执行 schema.sql，全部是 CREATE ... IF NOT EXISTS，重启安全。
func (d *DB) Init(schemaSQL string) error {
	if _, err := d.Exec(schemaSQL); err != nil {
		return fmt.Errorf("初始化 schema 失败：%w", err)
	}
	return nil
}

// Tx 在事务里跑 fn，panic 与 error 都回滚。
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// CheckSQLite 校验当前 SQLite 构建带 FTS5 与 trigram 分词器。
//
// 中文子串检索两者缺一不可：没有 FTS5 建不了虚拟表，
// 没有 trigram 就只能按整词匹配。构建镜像时跑一次，别等用户搜索才炸。
func CheckSQLite() error {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE VIRTUAL TABLE t USING fts5(x, tokenize='trigram')`); err != nil {
		return fmt.Errorf("SQLite 不支持 FTS5 + trigram：%w", err)
	}
	return nil
}
