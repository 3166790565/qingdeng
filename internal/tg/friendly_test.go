package tg

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

func TestFriendlyServerReject(t *testing.T) {
	err := errors.New("rpc error: code = 400 desc = PHONE_NUMBER_INVALID")
	msg := friendly(err)
	if !strings.Contains(msg, "Telegram 不接受这个号码") {
		t.Fatalf("应说明是 TG 侧拒绝：%s", msg)
	}
	if !strings.Contains(msg, "+6598004075") {
		t.Fatalf("应给出正确示例：%s", msg)
	}
	if strings.Contains(msg, "格式不对") {
		t.Fatalf("不该再说本地格式不对：%s", msg)
	}
}

func TestFriendlyNetworkFailure(t *testing.T) {
	cases := []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		errors.New("dial tcp 149.154.167.51:443: i/o timeout"),
		os.ErrDeadlineExceeded,
		errors.New("proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: connection refused"),
	}
	for _, err := range cases {
		msg := friendly(err)
		if !strings.Contains(msg, "不是号码问题") {
			t.Errorf("%v -> %s，应说明与号码无关", err, msg)
		}
	}
}

func TestFriendlyTimeout(t *testing.T) {
	msg := friendly(errors.New("TIMEOUT"))
	if !strings.Contains(msg, "不是号码问题") {
		t.Fatalf("超时不该归因到号码：%s", msg)
	}
}

func TestIsDialError(t *testing.T) {
	if !isDialError(errors.New("dial tcp: connection refused")) {
		t.Fatal("应识别为建连失败")
	}
	if isDialError(errors.New("PHONE_NUMBER_INVALID")) {
		t.Fatal("TG 业务错误不该判为建连失败")
	}
	if isDialError(nil) {
		t.Fatal("nil 不该判为建连失败")
	}
}
