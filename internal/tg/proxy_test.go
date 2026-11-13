package tg

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyDisabled(t *testing.T) {
	var cfg ProxyConfig
	if cfg.Enabled() {
		t.Fatal("空配置不该启用代理")
	}
	dial, err := cfg.dialFunc()
	if err != nil || dial != nil {
		t.Fatalf("空配置应返回 nil dial，得到 %v, %v", dial, err)
	}
}

func TestProxyBadScheme(t *testing.T) {
	cfg := ProxyConfig{URL: "ftp://127.0.0.1:7890"}
	if _, err := cfg.dialFunc(); err == nil {
		t.Fatal("不支持的协议应报错")
	}
}

func TestProxyMissingHost(t *testing.T) {
	cfg := ProxyConfig{URL: "socks5://"}
	if _, err := cfg.dialFunc(); err == nil {
		t.Fatal("缺主机应报错")
	}
}

// TestHTTPProxyConnect 起一个真代理，验证 CONNECT 隧道能打通并原样转发数据。
func TestHTTPProxyConnect(t *testing.T) {
	// 目标“服务器”：收到什么就回什么
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	var connects atomic.Int32
	var sawAuth atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "need CONNECT", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		if auth := r.Header.Get("Proxy-Authorization"); auth != "" {
			sawAuth.Store(auth)
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		defer upstream.Close()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer clientConn.Close()

		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go io.Copy(upstream, clientConn)
		io.Copy(clientConn, upstream)
	}))
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	cfg := ProxyConfig{URL: proxyURL.String(), Timeout: 5 * time.Second}
	dial, err := cfg.dialFunc()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", target.Addr().String())
	if err != nil {
		t.Fatalf("经代理建连失败：%v", err)
	}
	defer conn.Close()

	payload := []byte("hello-mtproto")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("隧道数据不对：%q", got)
	}
	if connects.Load() != 1 {
		t.Fatalf("应只 CONNECT 一次，实际 %d", connects.Load())
	}
}

// 代理带账号密码时要发 Proxy-Authorization: Basic。
func TestHTTPProxyAuthHeader(t *testing.T) {
	var got atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Proxy-Authorization"))
		http.Error(w, "deny", http.StatusForbidden)
	}))
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	proxyURL.User = url.UserPassword("user", "pass")
	cfg := ProxyConfig{URL: proxyURL.String(), Timeout: 3 * time.Second}
	dial, err := cfg.dialFunc()
	if err != nil {
		t.Fatal(err)
	}
	// 代理拒绝是预期结果，这里只关心请求头
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("代理拒绝时应报错")
	}
	if auth, _ := got.Load().(string); !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("缺少 Basic 认证头：%q", auth)
	}
}

// 代理拒绝 CONNECT 时，错误里要带状态码，方便排障。
func TestHTTPProxyRejectsConnect(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer proxySrv.Close()

	cfg := ProxyConfig{URL: proxySrv.URL, Timeout: 3 * time.Second}
	dial, err := cfg.dialFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = dial(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("错误里应包含状态码，得到：%v", err)
	}
}

// SOCKS5 至少能构造出 dialer（不做真实握手，避免依赖外部服务）。
func TestSocks5DialerBuilds(t *testing.T) {
	cfg := ProxyConfig{URL: "socks5://127.0.0.1:7890", Timeout: time.Second}
	dial, err := cfg.dialFunc()
	if err != nil || dial == nil {
		t.Fatalf("SOCKS5 dialer 构造失败：%v", err)
	}
	cfg.URL = "socks5://user:pass@127.0.0.1:7890"
	if _, err := cfg.dialFunc(); err != nil {
		t.Fatalf("带认证的 SOCKS5 构造失败：%v", err)
	}
}

// bufferedConn 要优先吐出 bufio 里已缓存的字节。
func TestBufferedConn(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		server.Write([]byte("leftover"))
	}()

	reader := bufio.NewReader(client)
	// 先读一点触发缓冲，模拟 CONNECT 响应后残留数据
	buf := make([]byte, 4)
	io.ReadFull(reader, buf)

	conn := &bufferedConn{Conn: client, reader: reader}
	rest := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(conn, rest); err != nil {
		t.Fatal(err)
	}
	if string(buf)+string(rest) != "leftover" {
		t.Fatalf("拼接结果不对：%q%q", buf, rest)
	}
}

// Clash 的 Mixed 端口常被粘贴成裸 host:port，不该因为少了协议前缀就失败。
func TestBareHostPortDefaultsToHTTP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1:7890", "host.docker.internal:7890"} {
		cfg := ProxyConfig{URL: raw, Timeout: time.Second}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s 应被接受，得到：%v", raw, err)
		}
		dial, err := cfg.dialFunc()
		if err != nil || dial == nil {
			t.Errorf("%s 应能构造 dialer，得到 %v, %v", raw, dial, err)
		}
	}
}

// 缺端口要给出能看懂的提示，而不是让 url.Parse 的默认端口蒙混过去。
func TestProxyMissingPort(t *testing.T) {
	cfg := ProxyConfig{URL: "socks5://127.0.0.1"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "端口") {
		t.Fatalf("应提示缺端口，得到：%v", err)
	}
}

// 用 HTTP 客户端去连一个 SOCKS5-only 端口时，错误里要给出换协议的提示。
func TestHTTPDialHintOnWrongProtocol(t *testing.T) {
	// 起一个只说 SOCKS5 的监听：收到 HTTP CONNECT 会读不出合法请求行
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// 回一段不是 HTTP 的字节，模拟协议不匹配
			c.Write([]byte("\x05\x00"))
			c.Close()
		}
	}()

	cfg := ProxyConfig{URL: "http://" + ln.Addr().String(), Timeout: 2 * time.Second}
	dial, err := cfg.dialFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = dial(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("协议不匹配时应报错")
	}
	if !strings.Contains(err.Error(), "socks5://") {
		t.Fatalf("错误里应提示换用 socks5://，得到：%v", err)
	}
}
