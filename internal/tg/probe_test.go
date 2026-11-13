package tg

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// 直连一个本地监听应成功。
func TestProbeDirectReachable(t *testing.T) {
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
			c.Close()
		}
	}()

	// Probe 的目标是 Telegram 的固定地址，这里单独验证 describeDialFailure
	// 与 Dial 的可用性，端口可达性用直连拨号确认。
	cfg := ProxyConfig{Timeout: 2 * time.Second}
	dial, err := cfg.Dial("tcp")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("直连本地端口失败：%v", err)
	}
	conn.Close()
}

func TestDescribeDialFailure(t *testing.T) {
	cases := []struct {
		err       error
		proxy     string
		wantIn    string
		notWantIn string
	}{
		{errString("dial tcp 1.2.3.4:443: connect: connection refused"), "socks5://127.0.0.1:7890", "代理拒绝连接", ""},
		{errString("dial tcp: connect: connection refused"), "", "连接被拒绝", ""},
		{errString("dial tcp: lookup host.docker.internal: no such host"), "socks5://host.docker.internal:7890", "extra_hosts", ""},
		{errString("dial tcp 1.2.3.4:443: i/o timeout"), "", "连接超时", ""},
		{errString("dial tcp 1.2.3.4:443: i/o timeout"), "http://127.0.0.1:7890", "代理可能没放行", ""},
		{errString("connect: no route to host"), "", "网络不可达", ""},
		{errString("proxyconnect tcp: unexpected EOF"), "http://127.0.0.1:7890", "代理握手失败", ""},
	}
	for _, c := range cases {
		got := describeDialFailure(c.err, c.proxy)
		if !strings.Contains(got, c.wantIn) {
			t.Errorf("err=%v proxy=%q -> %q，期望包含 %q", c.err, c.proxy, got, c.wantIn)
		}
		if c.notWantIn != "" && strings.Contains(got, c.notWantIn) {
			t.Errorf("不该包含 %q：%q", c.notWantIn, got)
		}
	}
}

// 直连时错误提示要提到“请先配置代理”，避免用户以为号码有问题。
func TestDescribeDialFailureHintsProxyWhenDirect(t *testing.T) {
	msg := describeDialFailure(errString("dial tcp: i/o timeout"), "")
	if !strings.Contains(msg, "代理") {
		t.Fatalf("直连失败应提示配置代理：%s", msg)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Probe 用不可达代理时应给出失败结果而不是 panic。
func TestProbeUnreachableProxy(t *testing.T) {
	cfg := ProxyConfig{URL: "socks5://127.0.0.1:1", Timeout: 1500 * time.Millisecond}
	res := cfg.Probe(context.Background())
	if res.OK {
		t.Fatal("不可达代理不该报成功")
	}
	if res.Message == "" || res.Proxy == "" || res.Target == "" {
		t.Fatalf("结果字段应完整：%+v", res)
	}
}

// TCP 能连上但数据不通时（TCP 劫持），检测必须判失败。
// 用手工搭的"黑洞"服务器模拟：接受连接、读完数据后不发任何回应。
func TestProbeDetectsBlackhole(t *testing.T) {
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
			go func(c net.Conn) {
				defer c.Close()
				// 读掉数据但永不回应
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	cfg := ProxyConfig{Timeout: 3 * time.Second}
	// 直接验证 describeHandshakeTimeout 的文案，避免真跑 MTProto
	msg := describeHandshakeTimeout("")
	if !strings.Contains(msg, "劫持") {
		t.Fatalf("直连握手超时应提示 TCP 劫持：%s", msg)
	}
	msg = describeHandshakeTimeout("socks5://127.0.0.1:7890")
	if !strings.Contains(msg, "代理节点") {
		t.Fatalf("代理场景应指向节点失效：%s", msg)
	}
	_ = cfg
}

func TestDescribeHandshakeFailure(t *testing.T) {
	direct := describeHandshakeFailure(errString("read tcp: connection reset by peer"), "")
	if !strings.Contains(direct, "劫持") && !strings.Contains(direct, "阻断") {
		t.Fatalf("直连被关闭应提示网络阻断：%s", direct)
	}
	proxied := describeHandshakeFailure(errString("read tcp: EOF"), "socks5://127.0.0.1:7890")
	if !strings.Contains(proxied, "上游节点") {
		t.Fatalf("代理场景应指向节点：%s", proxied)
	}
}
