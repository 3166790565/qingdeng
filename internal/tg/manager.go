// Package tg 封装 Telegram 频道爬取：登录状态机、频道解析、增量同步、自动调度。
//
// 与 Python 版（Telethon）的设计差异：
// 原版为了让同步代码调用 asyncio 库，自己养了一个常驻事件循环线程，
// 所有协程经 run_coroutine_threadsafe 提交。Go 这边 gotd 的客户端本身就跑在
// 一个 goroutine 里（通过 Run 阻塞式驱动），所以这里的做法是：
//
//   - 一个长期存在的 goroutine 持有 telegram.Client，调用方通过 channel 发指令；
//   - 登录的"发码 / 输码 / 输密码"三个 HTTP 请求共用同一个 client 与内存里的
//     phone_code_hash，正好被这个设计一并解决；
//   - 会话存成文件（数据目录下 tg.session），与 Python 版存字符串到库里的效果等价，
//     换库不影响登录态。
//
// 增量同步仍然靠 tg_channels.last_message_id 游标，重复内容由 content_hash 兜底。
package tg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"qingdeng/internal/config"
	"qingdeng/internal/importer"
	"qingdeng/internal/parser"
	"qingdeng/internal/store"
)

const (
	// PendingTTL 登录中间态有效期；超时或进程重启都回到"输手机号"那一步
	PendingTTL = 5 * time.Minute
	// downloadTimeout 单个文件下载的硬超时，防一个大文件把任务挂死
	downloadTimeout = 10 * time.Minute
	// floodSleepCap FloodWait 最多睡 15 分钟再试，更长的等待直接放弃该文件
	floodSleepCap = 15 * time.Minute
)

// NotLoggedIn 没有可用会话（没登录过或已失效），页面应引导去登录表单。
var NotLoggedIn = errors.New("尚未登录 Telegram")

// Manager 持有 TG 客户端与登录状态。
type Manager struct {
	cfg *config.Config
	db  *store.DB
	imp *importer.Importer

	sessionPath string

	mu      sync.Mutex
	client  *telegram.Client
	api     *tg.Client
	cancel  context.CancelFunc // 关掉当前 client 的 Run 循环
	pending *pendingLogin

	syncMu sync.Mutex // 全站同时只允许一个同步任务在跑
	stopCh chan struct{}
	wg     sync.WaitGroup
}

type pendingLogin struct {
	phone     string
	codeHash  string
	step      string // code | password
	expiresAt time.Time
}

// New 构造。sessionPath 是会话文件位置，默认放在数据目录。
func New(cfg *config.Config, db *store.DB, imp *importer.Importer) *Manager {
	return &Manager{
		cfg:         cfg,
		db:          db,
		imp:         imp,
		sessionPath: filepath.Join(cfg.DataDir, "tg.session"),
		stopCh:      make(chan struct{}),
	}
}

// APIReady 应用凭证是否已配置。
func (m *Manager) APIReady() bool { return m.cfg.TGReady() }

// LoggedIn 只看会话文件在不在；真伪要等连上网才知道。
func (m *Manager) LoggedIn() bool {
	_, err := os.Stat(m.sessionPath)
	return err == nil
}

// Phone 返回上次登录用的手机号。
func (m *Manager) Phone(ctx context.Context) string {
	value, _ := m.db.GetSetting(ctx, "tg_phone", "")
	return value
}

// PendingStep 页面渲染用："" 无进行中登录，"code" 待输验证码，"password" 待输 2FA。
func (m *Manager) PendingStep() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return ""
	}
	if time.Now().After(m.pending.expiresAt) {
		m.pending = nil
		return ""
	}
	return m.pending.step
}

// SyncBusy 是否有同步任务正在跑。
func (m *Manager) SyncBusy() bool {
	// TryLock 在 Go 1.18+ 可用，这里用它探测而不占锁
	if m.syncMu.TryLock() {
		m.syncMu.Unlock()
		return false
	}
	return true
}

// DropLogin 丢弃进行中的登录流程（用户点了「换个手机号」）。
func (m *Manager) DropLogin() {
	m.mu.Lock()
	m.pending = nil
	m.mu.Unlock()
}

// ---------------------------------------------------------------- 客户端生命周期

// ensureClient 取已授权的客户端；没有会话就返回 NotLoggedIn。
func (m *Manager) ensureClient(ctx context.Context) (*tg.Client, error) {
	if !m.APIReady() {
		return nil, fmt.Errorf("未配置 TG_API_ID / TG_API_HASH，请先写进 .env 再重启")
	}

	m.mu.Lock()
	if m.api != nil {
		api := m.api
		m.mu.Unlock()
		return api, nil
	}
	m.mu.Unlock()

	if !m.LoggedIn() {
		return nil, NotLoggedIn
	}

	if err := m.startClient(ctx); err != nil {
		return nil, err
	}

	m.mu.Lock()
	api := m.api
	m.mu.Unlock()
	if api == nil {
		return nil, NotLoggedIn
	}
	return api, nil
}

// startClient 启动 Run 循环，等到授权成功才返回。
func (m *Manager) startClient(ctx context.Context) error {
	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(m.cfg.TGAPIID, m.cfg.TGAPIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: m.sessionPath},
		UpdateHandler:  dispatcher,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)

	m.mu.Lock()
	m.client = client
	m.cancel = cancel
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := client.Run(runCtx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				ready <- err
				return err
			}
			if !status.Authorized {
				ready <- NotLoggedIn
				return NotLoggedIn
			}
			m.mu.Lock()
			m.api = client.API()
			m.mu.Unlock()
			ready <- nil
			<-runCtx.Done()
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			select {
			case ready <- err:
			default:
			}
		}
		// Run 退出后清空状态，下次调用会重建
		m.mu.Lock()
		if m.client == client {
			m.client = nil
			m.api = nil
		}
		m.mu.Unlock()
	}()

	select {
	case err := <-ready:
		if err != nil {
			cancel()
			m.mu.Lock()
			if m.client == client {
				m.client = nil
				m.api = nil
			}
			m.mu.Unlock()
			if errors.Is(err, NotLoggedIn) {
				m.invalidateSession()
				return NotLoggedIn
			}
			return fmt.Errorf("连接 Telegram 失败：%s", friendly(err))
		}
		return nil
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	case <-time.After(60 * time.Second):
		cancel()
		return errors.New("连接 Telegram 超时")
	}
}

// invalidateSession 会话被判失效时调用：删会话文件并弃掉 client。
func (m *Manager) invalidateSession() {
	m.mu.Lock()
	cancel := m.cancel
	m.client = nil
	m.api = nil
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = os.Remove(m.sessionPath)
}

// Shutdown 进程退出时调：停调度器、断客户端。
func (m *Manager) Shutdown() {
	close(m.stopCh)
	m.mu.Lock()
	cancel := m.cancel
	m.client = nil
	m.api = nil
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// ---------------------------------------------------------------- 登录状态机

// SendCode 第一步：输手机号，发验证码。
func (m *Manager) SendCode(ctx context.Context, phone string) error {
	number := strings.ReplaceAll(strings.TrimSpace(phone), " ", "")
	if !regexp.MustCompile(`^\+?\d{5,15}$`).MatchString(number) {
		return errors.New("手机号格式不对，要带国家区号，比如 +8613800138000")
	}
	if !strings.HasPrefix(number, "+") {
		number = "+" + number
	}

	// 每次发码都用新的空会话，避免上一次半截状态干扰
	if err := os.MkdirAll(filepath.Dir(m.sessionPath), 0o755); err != nil {
		return err
	}
	_ = os.Remove(m.sessionPath)

	if err := m.startClientAnon(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil {
		return errors.New("登录状态异常，请重试一次")
	}

	sent, err := client.Auth().SendCode(ctx, number, auth.SendCodeOptions{})
	if err != nil {
		m.teardownClient()
		return fmt.Errorf("发送验证码失败：%s", friendly(err))
	}
	codeHash := ""
	switch s := sent.(type) {
	case *tg.AuthSentCode:
		codeHash = s.PhoneCodeHash
	case *tg.AuthSentCodeSuccess:
		// 极少数情况直接成功（已有授权），交给后续状态查询处理
		m.finishLogin(number)
		return nil
	}
	if codeHash == "" {
		m.teardownClient()
		return errors.New("发送验证码失败：服务端未返回 code hash")
	}

	m.mu.Lock()
	m.pending = &pendingLogin{
		phone: number, codeHash: codeHash, step: "code",
		expiresAt: time.Now().Add(PendingTTL),
	}
	m.mu.Unlock()
	return nil
}

// startClientAnon 启动一个未授权的 client（登录流程专用）。
func (m *Manager) startClientAnon(ctx context.Context) error {
	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(m.cfg.TGAPIID, m.cfg.TGAPIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: m.sessionPath},
		UpdateHandler:  dispatcher,
	})
	runCtx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)

	m.mu.Lock()
	m.client = client
	m.cancel = cancel
	m.api = nil
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = client.Run(runCtx, func(ctx context.Context) error {
			ready <- nil
			<-runCtx.Done()
			return nil
		})
		m.mu.Lock()
		if m.client == client {
			m.client = nil
			m.api = nil
		}
		m.mu.Unlock()
	}()

	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	case <-time.After(30 * time.Second):
		cancel()
		return errors.New("连接 Telegram 超时")
	}
}

// VerifyCode 第二步：输验证码。开了两步验证返回 needPassword=true。
func (m *Manager) VerifyCode(ctx context.Context, code string) (needPassword bool, err error) {
	m.mu.Lock()
	pending := m.pending
	m.mu.Unlock()
	if pending == nil || pending.step != "code" {
		return false, errors.New("请先获取验证码")
	}
	if time.Now().After(pending.expiresAt) {
		m.DropLogin()
		return false, errors.New("登录流程已过期，请重新获取验证码")
	}

	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil {
		return false, errors.New("登录状态异常，请重新获取验证码")
	}

	_, err = client.Auth().SignIn(ctx, pending.phone, strings.TrimSpace(code), pending.codeHash)
	switch {
	case errors.Is(err, auth.ErrPasswordAuthNeeded):
		m.mu.Lock()
		pending.step = "password"
		pending.expiresAt = time.Now().Add(PendingTTL)
		m.mu.Unlock()
		return true, nil
	case err != nil:
		return false, fmt.Errorf("验证码不对或已过期：%s", friendly(err))
	}

	m.finishLogin(pending.phone)
	return false, nil
}

// VerifyPassword 第三步（可选）：输两步验证的云密码。
func (m *Manager) VerifyPassword(ctx context.Context, password string) error {
	m.mu.Lock()
	pending := m.pending
	m.mu.Unlock()
	if pending == nil || pending.step != "password" {
		return errors.New("不需要输云密码，或登录流程已过期")
	}

	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil {
		return errors.New("登录状态异常，请重新登录")
	}

	if _, err := client.Auth().Password(ctx, strings.TrimSpace(password)); err != nil {
		return fmt.Errorf("云密码不对：%s", friendly(err))
	}
	m.finishLogin(pending.phone)
	return nil
}

// finishLogin 登录成功：会话文件已由 FileSessionStorage 落盘，这里只更新内存状态。
func (m *Manager) finishLogin(phone string) {
	m.mu.Lock()
	if m.client != nil {
		m.api = m.client.API()
	}
	m.pending = nil
	m.mu.Unlock()
	_ = m.db.SetSetting(context.Background(), "tg_phone", phone)
}

// teardownClient 关掉当前 client（登录失败时用）。
func (m *Manager) teardownClient() {
	m.mu.Lock()
	cancel := m.cancel
	m.client = nil
	m.api = nil
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Logout 退出登录：让 TG 那边作废会话并删本地会话文件。
func (m *Manager) Logout(ctx context.Context) error {
	m.DropLogin()
	m.mu.Lock()
	api := m.api
	client := m.client
	m.mu.Unlock()

	if api != nil {
		// auth.logOut 不关心返回内容；失败也不阻止本地清理
		_, _ = api.AuthLogOut(ctx)
	}
	if client != nil {
		m.teardownClient()
	}
	_ = os.Remove(m.sessionPath)
	_ = m.db.DeleteSetting(ctx, "tg_session")
	return nil
}

// ---------------------------------------------------------------- 频道管理

var reUsername = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]{3,}`)

// ParseUsername 接受 t.me/xxx、https://t.me/xxx、@xxx、裸用户名；只支持公开频道。
func ParseUsername(link string) (string, error) {
	text := strings.TrimSpace(link)
	if m := regexp.MustCompile(`t\.me/(\+?[A-Za-z0-9_]+)`).FindStringSubmatch(text); m != nil {
		if strings.HasPrefix(m[1], "+") {
			return "", errors.New("私有频道（+开头的邀请链接）暂不支持，请用公开频道的用户名")
		}
		text = m[1]
	}
	text = strings.TrimPrefix(text, "@")
	if !reUsername.MatchString(text) {
		return "", errors.New("看不出频道用户名，试试粘贴 t.me/xxx 这样的链接")
	}
	return text, nil
}

// resolveChannel 按用户名取频道实体。
func (m *Manager) resolveChannel(ctx context.Context, api *tg.Client, username string) (*tg.Channel, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("找不到频道 @%s：%s", username, friendly(err))
	}
	for _, chat := range resolved.Chats {
		if channel, ok := chat.(*tg.Channel); ok {
			return channel, nil
		}
	}
	return nil, fmt.Errorf("找不到频道 @%s：它不是一个频道", username)
}

// AddChannel 解析用户名、连网取频道标题与 chat_id，落库。
func (m *Manager) AddChannel(ctx context.Context, link string) (string, error) {
	username, err := ParseUsername(link)
	if err != nil {
		return "", err
	}
	api, err := m.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	channel, err := m.resolveChannel(ctx, api, username)
	if err != nil {
		return "", err
	}

	title := strings.TrimSpace(channel.Title)
	if title == "" {
		title = "@" + username
	}
	exists, err := m.db.ChannelExists(ctx, username, channel.ID)
	if err != nil {
		return "", err
	}
	if exists {
		return "", errors.New("这个频道已经在列表里")
	}
	if err := m.db.AddChannel(ctx, title, username, channel.ID); err != nil {
		return "", err
	}
	return title, nil
}

// ---------------------------------------------------------------- 同步

// SyncChannel 增量同步单个频道：逐个下载 txt 并入库，每本写一次进度。
func (m *Manager) SyncChannel(ctx context.Context, channelID int64, jobID, forceMode string, targetChars int) {
	var log []store.JobRecord
	finish := func(status string, total int) {
		_ = m.db.SaveJob(ctx, jobID, status, total, log)
	}

	if !m.syncMu.TryLock() {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "已有同步任务在进行，请稍后再试"})
		finish(store.JobError, len(log))
		return
	}
	defer m.syncMu.Unlock()

	channel, err := m.db.GetChannel(ctx, channelID)
	if err != nil || channel == nil {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "频道不存在"})
		finish(store.JobError, len(log))
		return
	}

	api, err := m.ensureClient(ctx)
	if err != nil {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: err.Error()})
		finish(store.JobError, len(log))
		return
	}

	settings, err := m.db.LoadSettings(ctx)
	if err != nil {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "读取设置失败：" + err.Error()})
		finish(store.JobError, len(log))
		return
	}
	opts := settings.ParseOptions(forceMode, targetChars)

	inputPeer, err := m.channelPeer(ctx, api, channel)
	if err != nil {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "频道取不到：" + friendly(err)})
		finish(store.JobError, len(log))
		return
	}

	messages, err := m.collectTxt(ctx, api, inputPeer, channel.LastMessageID)
	if err != nil {
		if isAuthError(err) {
			m.invalidateSession()
		}
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "翻频道消息失败：" + friendly(err)})
		finish(store.JobError, len(log))
		return
	}

	// 超过大小上限的不浪费流量下载，直接记错误
	var toDownload []*tg.Message
	for _, msg := range messages {
		if doc := documentOf(msg); doc != nil && doc.Size > config.MaxTxtBytes {
			log = append(log, store.JobRecord{
				File: filenameOf(doc), Status: "error",
				Message: fmt.Sprintf("文件超过 %d MB 上限，未下载", config.MaxTxtBytes/1024/1024),
			})
			_ = m.db.BumpCursor(ctx, channelID, msg.ID)
			continue
		}
		toDownload = append(toDownload, msg)
	}
	total := len(messages)
	if total > 0 {
		finish(store.JobRunning, total)
	} else {
		_ = m.db.MarkChannelSynced(ctx, channelID)
		finish(store.JobDone, 0)
		return
	}

	for _, msg := range toDownload {
		record := m.syncOne(ctx, api, msg, opts)
		log = append(log, record)
		// 游标只在成功或判重跳过时推进：下载失败的留给下次同步重试
		if record.Status == "ok" || record.Status == "skip" {
			_ = m.db.BumpCursor(ctx, channelID, msg.ID)
		}
		finish(store.JobRunning, total)
		if record.Message == "TG 会话已失效，请重新登录" {
			log = append(log, store.JobRecord{
				File: "-", Status: "error", Message: "TG 会话已失效，剩余文件留待重新登录后再同步",
			})
			break
		}
	}

	_ = m.db.MarkChannelSynced(ctx, channelID)
	finish(store.JobDone, total)
}

// channelPeer 优先按 chat_id 取实体（防频道改名），失败后退回用户名。
func (m *Manager) channelPeer(ctx context.Context, api *tg.Client, channel *store.Channel) (tg.InputPeerClass, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: channel.Username,
	})
	if err != nil {
		return nil, err
	}
	for _, chat := range resolved.Chats {
		if c, ok := chat.(*tg.Channel); ok {
			// 顺便把最新的标题与 chat_id 回填进去（频道改名/重加后 ID 可能变化）
			if c.ID != channel.ChatID || (c.Title != "" && c.Title != channel.Title) {
				_ = m.db.UpdateChannelMeta(ctx, channel.ID, c.Title, c.ID)
			}
			return &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash}, nil
		}
	}
	return nil, errors.New("频道不是公开频道结构")
}

// collectTxt 翻页遍历频道消息，只收 txt 附件，返回按 id 升序的列表。
//
// messages.getHistory 从最新往前翻：不带 OffsetID 时返回最靠后的一页，
// 之后用上一页最小的 id 作为下一轮的 OffsetID。MinID 在服务端就过滤掉了
// 断点之前的消息，所以增量同步不会重复拉历史。
func (m *Manager) collectTxt(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, minID int) ([]*tg.Message, error) {
	var out []*tg.Message
	offsetID := 0
	for {
		result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: offsetID,
			Limit:    100,
			MinID:    minID,
		})
		if err != nil {
			return nil, err
		}

		msgs, ok := historyMessages(result)
		if !ok || len(msgs) == 0 {
			break
		}
		// 一页里可能混着服务端注入的服务消息，取最小的 id 作为翻页游标
		lowest := 0
		for _, msg := range msgs {
			if msg.ID <= minID {
				continue // 断点之前的旧消息，防御性跳过
			}
			if lowest == 0 || msg.ID < lowest {
				lowest = msg.ID
			}
			doc := documentOf(msg)
			if doc == nil {
				continue
			}
			if !importer.IsTxt(filenameOf(doc)) {
				continue // 只收 txt，图片/视频/epub 等直接忽略
			}
			out = append(out, msg)
		}
		if lowest == 0 || lowest == offsetID {
			// 没有再往前推进就收尾，防死循环
			break
		}
		offsetID = lowest
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func historyMessages(result tg.MessagesMessagesClass) ([]*tg.Message, bool) {
	switch r := result.(type) {
	case *tg.MessagesMessages:
		return messageList(r.Messages), true
	case *tg.MessagesMessagesSlice:
		return messageList(r.Messages), true
	case *tg.MessagesChannelMessages:
		return messageList(r.Messages), true
	}
	return nil, false
}

func messageList(items []tg.MessageClass) []*tg.Message {
	out := make([]*tg.Message, 0, len(items))
	for _, item := range items {
		if msg, ok := item.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out
}

// documentOf 取消息里的文件文档，非文件消息返回 nil。
func documentOf(msg *tg.Message) *tg.Document {
	media, ok := msg.GetMedia()
	if !ok {
		return nil
	}
	doc, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		return nil
	}
	switch d := doc.Document.(type) {
	case *tg.Document:
		return d
	}
	return nil
}

func filenameOf(doc *tg.Document) string {
	for _, attr := range doc.Attributes {
		if name, ok := attr.(*tg.DocumentAttributeFilename); ok {
			if strings.TrimSpace(name.FileName) != "" {
				return name.FileName
			}
		}
	}
	return ""
}

// syncOne 下载单条消息的 txt 附件并走完整导入管线。
func (m *Manager) syncOne(ctx context.Context, api *tg.Client, msg *tg.Message, opts parser.ParseOptions) store.JobRecord {
	doc := documentOf(msg)
	name := "unnamed.txt"
	if doc != nil {
		if n := filenameOf(doc); n != "" {
			name = n
		}
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		dlCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
		buf, err := downloadDocument(dlCtx, api, doc)
		cancel()
		if err == nil {
			return m.imp.ImportOne(ctx, name, buf, opts)
		}
		lastErr = err

		if isAuthError(err) {
			m.invalidateSession()
			return store.JobRecord{File: name, Status: "error", Message: "TG 会话已失效，请重新登录"}
		}
		if wait, ok := tgerr.AsFloodWait(err); ok {
			if attempt == 2 {
				return store.JobRecord{
					File: name, Status: "error",
					Message: fmt.Sprintf("触发限流，需等 %d 秒，已跳过", int(wait.Seconds())),
				}
			}
			sleep := wait + time.Second
			if sleep > floodSleepCap {
				sleep = floodSleepCap
			}
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return store.JobRecord{File: name, Status: "error", Message: "同步被取消"}
			}
			continue
		}
		return store.JobRecord{File: name, Status: "error", Message: "下载失败：" + friendly(err)}
	}
	if lastErr != nil {
		return store.JobRecord{File: name, Status: "error", Message: "下载失败：" + friendly(lastErr)}
	}
	return store.JobRecord{File: name, Status: "error", Message: "下载失败"}
}

func downloadDocument(ctx context.Context, api *tg.Client, doc *tg.Document) ([]byte, error) {
	if doc == nil {
		return nil, errors.New("消息里没有文件")
	}
	loc := &tg.InputDocumentFileLocation{
		ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference,
	}
	var buf bytes.Buffer
	// 单文件上限 64MB，预分配能少几次扩容
	buf.Grow(int(doc.Size))
	if _, err := downloader.NewDownloader().Download(api, loc).Stream(ctx, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SyncChannels 「全部同步」入口：每频道一个 job，串行跑完。
func (m *Manager) SyncChannels(ctx context.Context, entries []struct {
	ChannelID int64
	JobID     string
}, forceMode string, targetChars int) {
	for _, e := range entries {
		m.SyncChannel(ctx, e.ChannelID, e.JobID, forceMode, targetChars)
	}
}

// ---------------------------------------------------------------- 定期自动同步

// StartScheduler 启动自动同步调度器。
func (m *Manager) StartScheduler() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.autoTick()
			}
		}
	}()
}

func (m *Manager) autoTick() {
	ctx := context.Background()
	raw, _ := m.db.GetSetting(ctx, "tg_sync_interval", "0")
	var interval int
	fmt.Sscanf(raw, "%d", &interval)
	if interval <= 0 {
		return
	}

	lastRaw, _ := m.db.GetSetting(ctx, "tg_last_auto_run", "")
	if lastRaw != "" {
		var last int64
		if _, err := fmt.Sscanf(lastRaw, "%d", &last); err == nil {
			if time.Now().Unix()-last < int64(interval)*60 {
				return
			}
		}
	}
	if !m.LoggedIn() {
		return
	}
	channels, err := m.db.EnabledChannels(ctx)
	if err != nil || len(channels) == 0 {
		return
	}
	// 先占坑，防下一轮重叠触发
	_ = m.db.SetSetting(ctx, "tg_last_auto_run", fmt.Sprintf("%d", time.Now().Unix()))
	for _, channel := range channels {
		jobID, err := m.db.CreateJob(ctx, 0)
		if err != nil {
			continue
		}
		m.SyncChannel(ctx, channel.ID, jobID, "", 0)
	}
}

// ---------------------------------------------------------------- 杂项

func isAuthError(err error) bool {
	return tgerr.IsCode(err, 401) || tgerr.Is(err, "AUTH_KEY_UNREGISTERED")
}

// friendly 把 gotd 的异常翻成能给管理员看的中文。
func friendly(err error) string {
	if err == nil {
		return ""
	}
	if wait, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf("触发限流，需等 %d 秒", int(wait.Seconds()))
	}
	msg := err.Error()
	replacements := map[string]string{
		"PHONE_NUMBER_INVALID":  "手机号格式不对",
		"PHONE_CODE_INVALID":    "验证码不对",
		"PHONE_CODE_EXPIRED":    "验证码已过期",
		"PASSWORD_HASH_INVALID": "云密码不对",
		"USERNAME_NOT_OCCUPIED": "用户名不存在",
		"USERNAME_INVALID":      "用户名不可用",
		"AUTH_KEY_UNREGISTERED": "TG 会话已失效，请重新登录",
		"TIMEOUT":               "连接 Telegram 超时",
	}
	for code, text := range replacements {
		if strings.Contains(msg, code) {
			return text
		}
	}
	return msg
}
