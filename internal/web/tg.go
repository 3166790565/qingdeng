package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	tgmanager "qingdeng/internal/tg"
)

// tgContext 页面上下文。
func (a *App) tgContext(r *http.Request) map[string]any {
	ctx := r.Context()
	apiReady := a.TG.APIReady()
	channels, _ := a.DB.ListChannels(ctx)
	jobs, _ := a.DB.RecentJobs(ctx, 8)
	interval, _ := a.DB.GetSetting(ctx, "tg_sync_interval", "0")

	var list []map[string]any
	for _, c := range channels {
		list = append(list, map[string]any{
			"ID": c.ID, "Title": c.Title, "Username": c.Username,
			"Enabled": c.Enabled, "LastMessageID": c.LastMessageID,
			"LastSyncAt": c.LastSyncAt,
		})
	}

	pending := ""
	if apiReady {
		pending = a.TG.PendingStep()
	}
	return map[string]any{
		"Active":       "telegram",
		"APIReady":     apiReady,
		"LoggedIn":     apiReady && a.TG.LoggedIn(),
		"Phone":        a.TG.Phone(ctx),
		"PendingStep":  pending,
		"Channels":     list,
		"SyncInterval": interval,
		"SyncBusy":     a.TG.SyncBusy(),
		"ProxyURL":     a.TG.ProxyURL(),
		"Jobs":         jobs,
		"Flash":        r.URL.Query().Get("flash"),
		"Kind":         r.URL.Query().Get("kind"),
	}
}

func (a *App) telegramPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("restart") != "" {
		a.TG.DropLogin() // 「换个手机号」：丢弃进行中的登录流程
	}
	a.Render.HTML(w, 200, "admin/telegram.html", a.tgContext(r))
}

// ---------------------------------------------------------------- 登录（fetch JSON）

func (a *App) tgLoginSend(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := a.TG.SendCode(r.Context(), r.FormValue("phone")); err != nil {
		JSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	JSON(w, 200, map[string]any{"ok": true, "step": "code"})
}

func (a *App) tgLoginVerify(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	needPassword, err := a.TG.VerifyCode(r.Context(), r.FormValue("code"))
	if err != nil {
		JSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	step := "done"
	if needPassword {
		step = "password"
	}
	JSON(w, 200, map[string]any{"ok": true, "step": step})
}

func (a *App) tgLoginPassword(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := a.TG.VerifyPassword(r.Context(), r.FormValue("password")); err != nil {
		JSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	JSON(w, 200, map[string]any{"ok": true, "step": "done"})
}

func (a *App) tgLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.TG.Logout(r.Context()); err != nil {
		JSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	JSON(w, 200, map[string]any{"ok": true, "step": "logged_out"})
}

// ---------------------------------------------------------------- 频道管理

func (a *App) tgChannelAdd(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	title, err := a.TG.AddChannel(r.Context(), r.FormValue("link"))
	if err != nil {
		flashRedirect(w, r, "/admin/telegram", err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/telegram", "已添加频道："+title, true)
}

func (a *App) tgChannelDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		flashRedirect(w, r, "/admin/telegram", "频道编号不对", false)
		return
	}
	channel, _ := a.DB.GetChannel(r.Context(), id)
	if channel == nil {
		flashRedirect(w, r, "/admin/telegram", "频道不存在", false)
		return
	}
	if err := a.DB.RemoveChannel(r.Context(), id); err != nil {
		flashRedirect(w, r, "/admin/telegram", "删除失败："+err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/telegram", "已移除频道", true)
}

func (a *App) tgChannelToggle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		flashRedirect(w, r, "/admin/telegram", "频道编号不对", false)
		return
	}
	channel, _ := a.DB.GetChannel(r.Context(), id)
	if channel == nil {
		flashRedirect(w, r, "/admin/telegram", "频道不存在", false)
		return
	}
	if err := a.DB.ToggleChannel(r.Context(), id); err != nil {
		flashRedirect(w, r, "/admin/telegram", "切换失败："+err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/telegram", "已切换频道启用状态", true)
}

// ---------------------------------------------------------------- 自动同步设置

func (a *App) tgProxySave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rawURL := strings.TrimSpace(r.FormValue("proxy_url"))
	// 先校验格式，别把明显写错的地址存进库，否则下次建连才报错
	if rawURL != "" {
		cfg := tgmanager.ProxyConfig{URL: rawURL}
		if err := cfg.Validate(); err != nil {
			flashRedirect(w, r, "/admin/telegram", "代理地址有误："+err.Error(), false)
			return
		}
	}
	if err := a.TG.SetProxyURL(r.Context(), rawURL); err != nil {
		flashRedirect(w, r, "/admin/telegram", "保存失败："+err.Error(), false)
		return
	}
	if rawURL == "" {
		flashRedirect(w, r, "/admin/telegram", "已改为直连", true)
		return
	}
	flashRedirect(w, r, "/admin/telegram", "代理已保存："+rawURL, true)
}

func (a *App) tgSettingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	interval := formInt(r, "sync_interval")
	if interval < 0 {
		interval = 0
	}
	if interval > 1440 {
		interval = 1440
	}
	if err := a.DB.SetSetting(r.Context(), "tg_sync_interval", strconv.Itoa(interval)); err != nil {
		flashRedirect(w, r, "/admin/telegram", "保存失败："+err.Error(), false)
		return
	}
	message := "自动同步已关闭"
	if interval > 0 {
		message = "自动同步间隔已存为 " + strconv.Itoa(interval) + " 分钟"
	}
	flashRedirect(w, r, "/admin/telegram", message, true)
}

// ---------------------------------------------------------------- 触发同步

func (a *App) tgSync(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if a.TG.SyncBusy() {
		JSON(w, http.StatusConflict, map[string]any{"ok": false, "message": "已有同步任务在进行，请稍后再试"})
		return
	}
	channelID := strings.TrimSpace(r.FormValue("channel_id"))
	forceMode := r.FormValue("force_mode")
	targetChars := formInt(r, "target_chars")

	ctx := context.Background()
	if channelID == "all" {
		channels, err := a.DB.EnabledChannels(ctx)
		if err != nil || len(channels) == 0 {
			JSON(w, 400, map[string]any{"ok": false, "message": "没有启用的频道"})
			return
		}
		var entries []struct {
			ChannelID int64
			JobID     string
		}
		var jobIDs []string
		for _, channel := range channels {
			jobID, err := a.DB.CreateJob(ctx, 0)
			if err != nil {
				continue
			}
			entries = append(entries, struct {
				ChannelID int64
				JobID     string
			}{ChannelID: channel.ID, JobID: jobID})
			jobIDs = append(jobIDs, jobID)
		}
		if len(entries) == 0 {
			JSON(w, 500, map[string]any{"ok": false, "message": "建任务失败"})
			return
		}
		go a.TG.SyncChannels(ctx, entries, forceMode, targetChars)
		JSON(w, 200, map[string]any{
			"ok": true, "job_id": jobIDs[0], "job_ids": jobIDs, "count": len(jobIDs),
		})
		return
	}

	id, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		JSON(w, 400, map[string]any{"ok": false, "message": "参数不对"})
		return
	}
	channel, err := a.DB.GetChannel(ctx, id)
	if err != nil || channel == nil {
		JSON(w, 404, map[string]any{"ok": false, "message": "频道不存在"})
		return
	}
	jobID, err := a.DB.CreateJob(ctx, 0)
	if err != nil {
		JSON(w, 500, map[string]any{"ok": false, "message": "建任务失败：" + err.Error()})
		return
	}
	go a.TG.SyncChannel(ctx, id, jobID, forceMode, targetChars)
	JSON(w, 200, map[string]any{"ok": true, "job_id": jobID, "job_ids": []string{jobID}, "count": 1})
}
