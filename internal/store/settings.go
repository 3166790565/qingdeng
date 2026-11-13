package store

import (
	"context"
	"database/sql"
	"strconv"

	"qingdeng/internal/parser"
)

// SettingsDefaults 可持久化的解析选项默认值。
// 注意 strip_header_block 与 fix_yao_variant 按方案定为默认关闭。
var SettingsDefaults = map[string]string{
	"target_chars":       "3000",
	"strip_header_block": "0",
	"fix_yao_variant":    "0",
	"add_subtitle":       "1",
}

var boolSettingKeys = map[string]bool{
	"strip_header_block": true,
	"fix_yao_variant":    true,
	"add_subtitle":       true,
}

// Settings 当前解析设置，键名与 SettingsDefaults 一致。
type Settings map[string]string

// LoadSettings 读全部解析设置，缺失的用默认值补齐。
func (d *DB) LoadSettings(ctx context.Context) (Settings, error) {
	values := make(Settings, len(SettingsDefaults))
	for k, v := range SettingsDefaults {
		values[k] = v
	}
	rows, err := d.QueryContext(ctx, `SELECT key, COALESCE(value,'') FROM app_settings`)
	if err != nil {
		return values, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return values, err
		}
		if _, ok := SettingsDefaults[key]; ok {
			values[key] = value
		}
	}
	return values, rows.Err()
}

// SaveSettings 只写白名单键；布尔键存 "1"/"0"，数值键做钳制。
func (d *DB) SaveSettings(ctx context.Context, values map[string]any) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		for key, raw := range values {
			if _, ok := SettingsDefaults[key]; !ok {
				continue
			}
			var stored string
			if boolSettingKeys[key] {
				if asBool(raw) {
					stored = "1"
				} else {
					stored = "0"
				}
			} else {
				n, ok := asInt(raw)
				if !ok {
					continue
				}
				stored = strconv.Itoa(n)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO app_settings(key, value) VALUES(?,?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, stored); err != nil {
				return err
			}
		}
		return nil
	})
}

// DefaultTargetChars 返回当前的目标分节字数。
func (s Settings) DefaultTargetChars() int {
	n, err := strconv.Atoi(s["target_chars"])
	if err != nil || n <= 0 {
		return 3000
	}
	return n
}

// ParseOptions 造解析选项。forceMode 来自上传表单/重新分章，targetChars>0 时覆盖设置值。
func (s Settings) ParseOptions(forceMode string, targetChars int) parser.ParseOptions {
	target := s.DefaultTargetChars()
	if targetChars > 0 {
		target = targetChars
	}
	return parser.ParseOptions{
		TargetChars:      target,
		StripHeaderBlock: s["strip_header_block"] == "1",
		FixYaoVariant:    s["fix_yao_variant"] == "1",
		AddSubtitle:      s["add_subtitle"] == "1",
		StripAds:         true,
		MergeHardWraps:   true,
		ForceMode:        forceMode,
	}.Normalize()
}

func asBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		return value != "" && value != "0" && value != "false"
	case int:
		return value != 0
	}
	return false
}

func asInt(v any) (int, bool) {
	switch value := v.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case string:
		n, err := strconv.Atoi(value)
		return n, err == nil
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
