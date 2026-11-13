package store

import (
	"context"
	"database/sql"
)

// ListChannels 全部频道，按添加顺序。
func (d *DB) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, COALESCE(title,''), username, COALESCE(chat_id,0), enabled,
		        last_message_id, COALESCE(last_sync_at,''), COALESCE(created_at,'')
		   FROM tg_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanChannel(sc interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	var enabled int
	err := sc.Scan(&c.ID, &c.Title, &c.Username, &c.ChatID, &enabled,
		&c.LastMessageID, &c.LastSyncAt, &c.CreatedAt)
	c.Enabled = enabled != 0
	return c, err
}

// EnabledChannels 只看启用的。
func (d *DB) EnabledChannels(ctx context.Context) ([]Channel, error) {
	all, err := d.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	var out []Channel
	for _, c := range all {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out, nil
}

// GetChannel 按 id 取频道。
func (d *DB) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	row := d.QueryRowContext(ctx,
		`SELECT id, COALESCE(title,''), username, COALESCE(chat_id,0), enabled,
		        last_message_id, COALESCE(last_sync_at,''), COALESCE(created_at,'')
		   FROM tg_channels WHERE id = ?`, id)
	c, err := scanChannel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ChannelExists 按用户名或 chat_id 判重。
func (d *DB) ChannelExists(ctx context.Context, username string, chatID int64) (bool, error) {
	var count int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tg_channels WHERE username = ?
		    OR (chat_id IS NOT NULL AND chat_id = ? AND ? <> 0)`,
		username, chatID, chatID).Scan(&count)
	return count > 0, err
}

// AddChannel 落库。
func (d *DB) AddChannel(ctx context.Context, title, username string, chatID int64) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO tg_channels(title, username, chat_id, created_at) VALUES(?,?,?,?)`,
		title, username, chatID, Now())
	return err
}

// RemoveChannel 删除。
func (d *DB) RemoveChannel(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM tg_channels WHERE id = ?`, id)
	return err
}

// ToggleChannel 启用/停用。
func (d *DB) ToggleChannel(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `UPDATE tg_channels SET enabled = 1 - enabled WHERE id = ?`, id)
	return err
}

// BumpCursor 游标只前进不后退，同步中断也能从断点续跑。
func (d *DB) BumpCursor(ctx context.Context, id int64, messageID int) error {
	_, err := d.ExecContext(ctx,
		`UPDATE tg_channels SET last_message_id = MAX(last_message_id, ?) WHERE id = ?`,
		messageID, id)
	return err
}

// MarkChannelSynced 记录本次同步完成时间。
func (d *DB) MarkChannelSynced(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `UPDATE tg_channels SET last_sync_at = ? WHERE id = ?`, Now(), id)
	return err
}

// UpdateChannelMeta 同步频道标题与 chat_id（频道改名后取实体时回填）。
func (d *DB) UpdateChannelMeta(ctx context.Context, id int64, title string, chatID int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE tg_channels SET title = COALESCE(NULLIF(?,''), title), chat_id = ? WHERE id = ?`,
		title, chatID, id)
	return err
}
