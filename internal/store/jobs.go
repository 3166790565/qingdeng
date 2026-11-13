package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

// JobStatus 任务状态常量。
const (
	JobPending = "pending"
	JobRunning = "running"
	JobDone    = "done"
	JobError   = "error"
)

// NewJobID 生成 16 位十六进制任务号。
func NewJobID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用：" + err.Error())
	}
	return hex.EncodeToString(buf)
}

// CreateJob 建一条 pending 任务行。
func (d *DB) CreateJob(ctx context.Context, total int) (string, error) {
	id := NewJobID()
	_, err := d.ExecContext(ctx,
		`INSERT INTO import_jobs(id, status, total, done, ok, failed, log, created_at)
		 VALUES(?, 'pending', ?, 0, 0, 0, '[]', ?)`, id, total, Now())
	return id, err
}

// GetJob 读任务，log 列解析成结构化记录。
func (d *DB) GetJob(ctx context.Context, id string) (*Job, error) {
	var job Job
	var raw string
	err := d.QueryRowContext(ctx,
		`SELECT id, COALESCE(status,'pending'), total, done, ok, failed,
		        COALESCE(log,'[]'), COALESCE(created_at,'')
		   FROM import_jobs WHERE id = ?`, id).
		Scan(&job.ID, &job.Status, &job.Total, &job.Done, &job.OK, &job.Failed, &raw, &job.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &job.Log); err != nil {
		job.Log = nil
	}
	return &job, nil
}

// JobSummary 列表页用的任务摘要（不带 log）。
type JobSummary struct {
	ID        string
	Status    string
	Total     int
	Done      int
	OK        int
	Failed    int
	CreatedAt string
}

// RecentJobs 最近若干任务。
func (d *DB) RecentJobs(ctx context.Context, limit int) ([]JobSummary, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, COALESCE(status,''), total, done, ok, failed, COALESCE(created_at,'')
		   FROM import_jobs ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []JobSummary
	for rows.Next() {
		var j JobSummary
		if err := rows.Scan(&j.ID, &j.Status, &j.Total, &j.Done, &j.OK, &j.Failed, &j.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// SaveJob 重算计数并整体写回 log。
//
// 导入与同步每处理一本就调一次，前端轮询 /admin/api/jobs/{id} 就能逐本看到结果。
func (d *DB) SaveJob(ctx context.Context, id, status string, total int, log []JobRecord) error {
	ok, failed, skipped := 0, 0, 0
	for _, rec := range log {
		switch rec.Status {
		case "ok":
			ok++
		case "skip":
			skipped++
		case "error":
			failed++
		}
	}
	if log == nil {
		log = []JobRecord{}
	}
	payload, err := json.Marshal(log)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx,
		`UPDATE import_jobs SET status=?, total=?, done=?, ok=?, failed=?, log=? WHERE id=?`,
		status, total, ok+skipped+failed, ok, failed, string(payload), id)
	return err
}
