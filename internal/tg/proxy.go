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
	parsed, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil {
		return nil, fmt.Errorf("代理地址解析失败：%w", err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("代理地址缺少主机与端口：%s", p.URL)
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
			return dialViaHTTPProxy(ctx, parsed, network, addr, timeout)
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
