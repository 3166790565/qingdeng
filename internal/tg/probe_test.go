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
