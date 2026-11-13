package tg

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
)

// ProxyConfig 描述出站代理。空 ProxyURL 表示直连。
type ProxyConfig struct {
	// URL 形如 socks5://127.0.0.1:7890 或 http://127.0.0.1:7890
	URL string
	// Timeout 建连超时，0 用默认 30 秒
	Timeout time.Duration
}

// Enabled 是否配置了代理。
func (p ProxyConfig) Enabled() bool { return strings.TrimSpace(p.URL) != "" }

// Dial 返回一个按代理建连的拨号函数；未配置代理时返回直接 dial 的闭包。
// 单独导出是为了让测试与排障工具能复用同一套拨号逻辑。
func (p ProxyConfig) Dial(network string) (func(context.Context, string) (net.Conn, error), error) {
	dial, err := p.dialFunc()
	if err != nil {
		return nil, err
	}
	if dial == nil {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		return func(ctx context.Context, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: timeout}
			return dialer.DialContext(ctx, "tcp", addr)
		}, nil
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		return dial(ctx, network, addr)
	}, nil
}

// Validate 只做地址与协议校验，不发起连接。给设置页的即时反馈用。
func (p ProxyConfig) Validate() error {
	if !p.Enabled() {
		return nil
	}
	_, err := p.dialFunc()
	return err
}

// dialFunc 按代理协议返回一个 DialFunc。
//
// 支持两种形态：
//   - socks5://host:port —— 用 x/net/proxy 的 SOCKS5 实现（支持用户名密码）
//   - http://host:port 或 https://host:port —— 走 HTTP CONNECT 隧道
//
// Clash / v2ray 这类工具通常同一个端口同时接受 HTTP 与 SOCKS5，
// 填哪种都行；协议不对会在握手时报错，日志里能看出来。
func (p ProxyConfig) dialFunc() (dcs.DialFunc, error) {
	if !p.Enabled() {
		return nil, nil
	}
	raw := strings.TrimSpace(p.URL)
	// 没写协议时按 http 处理：Clash 的 Mixed 端口同时支持 HTTP 与 SOCKS5，
	// 而用户粘贴 127.0.0.1:7890 是最常见的形态，不该因为少个前缀直接失败。
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理地址解析失败：%w", err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("代理地址缺少主机与端口：%s（应形如 socks5://127.0.0.1:7890）", p.URL)
	}
	if parsed.Port() == "" {
		return nil, fmt.Errorf("代理地址缺少端口：%s（应形如 socks5://127.0.0.1:7890）", p.URL)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	switch strings.ToLower(parsed.Scheme) {
	case "socks5", "socks5h", "socks":
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, socksAuth(parsed), proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("初始化 SOCKS5 代理失败：%w", err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS5 代理不支持 context")
		}
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 代理本身不可达时也别无限等
			dialCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return ctxDialer.DialContext(dialCtx, network, addr)
		}, nil

	case "http", "https":
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialViaHTTPProxy(ctx, parsed, network, addr, timeout)
			if err != nil {
				// Clash 的 Mixed 端口两种协议都收，但填错协议时握手会失败；
				// 这里补一句提示，省得用户盯着晦涩的报错猜。
				return nil, fmt.Errorf("%w（若代理是 SOCKS5 专用端口，请改用 socks5:// 前缀）", err)
			}
			return conn, nil
		}, nil

	default:
		return nil, fmt.Errorf("不支持的代理协议 %q，请用 socks5:// 或 http://", parsed.Scheme)
	}
}

func socksAuth(u *url.URL) *proxy.Auth {
	if u.User == nil {
		return nil
	}
	auth := &proxy.Auth{User: u.User.Username()}
	if password, ok := u.User.Password(); ok {
		auth.Password = password
	}
	return auth
}

// dialViaHTTPProxy 用 CONNECT 方法建隧道。
//
// 同一条 TCP 连接上完成隧道后，后续读到的就是 Telegram 的 MTProto 流量，
// 所以这里只负责握手，不缓冲任何数据。
func dialViaHTTPProxy(ctx context.Context, proxyURL *url.URL, network, addr string, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("连接代理 %s 失败：%w", proxyURL.Host, err)
	}

	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	// 目标地址在 CONNECT 里必须是 host:port，不能带 scheme
	host := addr
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: host},
		Host:   host,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString(
			[]byte(proxyURL.User.Username() + ":" + password))
		req.Header.Set("Proxy-Authorization", "Basic "+token)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("向代理发送 CONNECT 失败：%w", err)
	}

	// 读响应头；用 bufio 是为了不吞掉隧道里可能已经到达的首批字节
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取代理响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		conn.Close()
		return nil, fmt.Errorf("代理拒绝 CONNECT（%d %s）：%s",
			resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}

	_ = conn.SetDeadline(time.Time{})
	// reader 里可能残留了隧道数据，包一层让它继续可读
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

// bufferedConn 把 bufio 里剩余的数据接在连接前面。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// clientOptions 按代理配置生成 telegram.Options 的 Resolver 部分。
//
// 返回 nil 表示直连（不动 Resolver，让 gotd 用默认拨号）。
func (p ProxyConfig) resolverOptions() (*dcs.PlainOptions, error) {
	dial, err := p.dialFunc()
	if err != nil {
		return nil, err
	}
	if dial == nil {
		return nil, nil
	}
	return &dcs.PlainOptions{Dial: dial}, nil
}

// applyTo 把代理配置写进 telegram.Options。
func (p ProxyConfig) applyTo(opts telegram.Options) (telegram.Options, error) {
	plain, err := p.resolverOptions()
	if err != nil {
		return opts, err
	}
	if plain != nil {
		opts.Resolver = dcs.Plain(*plain)
	}
	return opts, nil
}

// ProbeResult 代理连通性检测结果。
type ProbeResult struct {
	OK      bool          `json:"ok"`
	Message string        `json:"message"`
	Proxy   string        `json:"proxy"`  // 实际使用的代理（空表示直连）
	Target  string        `json:"target"` // 用来检测的目标
	DialMs  int64         `json:"dialMs"` // 建连耗时
	Elapsed time.Duration `json:"-"`
}

// TG 的入口 DC 地址，用来测“能不能到 Telegram”而不只是“代理端口开没开”。
// 149.154.167.51 是 DC2 生产地址，长期没变。
const probeTarget = "149.154.167.51:443"

// Probe 检测经由当前代理能否建立到 Telegram 的 TCP 连接。
//
// 只做到 TCP 握手，不发起 MTProto 握手：能连上就说明网络链路通，
// 账密、session 之类的问题留给后续登录流程报。
// 这样检测结果对“代理配得对不对”是准确的，不会因为别的错误混淆。
func (p ProxyConfig) Probe(ctx context.Context) ProbeResult {
	target := probeTarget
	result := ProbeResult{Proxy: p.URL, Target: target}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dial, err := p.Dial("tcp")
	if err != nil {
		result.Message = "代理配置有误：" + err.Error()
		return result
	}
	start := time.Now()
	conn, err := dial(ctx, target)
	result.DialMs = time.Since(start).Milliseconds()
	if err != nil {
		result.Message = describeDialFailure(err, p.URL)
		return result
	}
	conn.Close()
	result.OK = true
	result.Message = "连接 Telegram 成功"
	return result
}

// describeDialFailure 把底层错误翻译成能直接照做的提示。
func describeDialFailure(err error, proxyURL string) string {
	msg := strings.ToLower(err.Error())
	suffix := ""
	if proxyURL == "" {
		suffix = "（当前为直连，若在国内服务器请先配置代理）"
	}

	switch {
	case strings.Contains(msg, "connection refused"):
		if proxyURL == "" {
			return "连接被拒绝：目标端口不可达" + suffix
		}
		return "代理拒绝连接：确认代理进程在跑、端口对得上" + suffix
	case strings.Contains(msg, "no such host"):
		if strings.Contains(proxyURL, "host.docker.internal") {
			return "解析不到 host.docker.internal：Linux 上需要 compose 的 extra_hosts 配置" + suffix
		}
		return "解析不到代理主机名：检查拼写与 DNS" + suffix
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "timeout"):
		if proxyURL == "" {
			return "连接超时：直连 Telegram 不通" + suffix
		}
		return "连接超时：代理可能没放行 Telegram，或代理本身不可用" + suffix
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return "网络不可达：服务器没有到 Telegram 的出路" + suffix
	case strings.Contains(msg, "proxyconnect"), strings.Contains(msg, "connect"):
		return "代理握手失败：确认协议前缀（socks5:// / http://）与代理类型一致。原始错误：" + err.Error()
	}
	return err.Error()
}
