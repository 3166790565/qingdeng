package tg

import (
	"strings"
	"testing"
)

func TestPhoneNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"+6598004075", "+6598004075"},
		{"6598004075", "+6598004075"},
		{"+65 9800 4075", "+6598004075"},
		{"+65-9800-4075", "+6598004075"},
		{"(+65) 9800 4075", "+6598004075"},
		{"+1 (415) 555-2671", "+14155552671"},
		{"+44 20 7946 0958", "+442079460958"},
		{" 13800138000 ", "+13800138000"},
	}
	for _, c := range cases {
		got, err := normalizePhone(c.in)
		if err != nil {
			t.Errorf("%q: 意外错误 %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPhoneRejectsBad(t *testing.T) {
	bad := []string{"", "abc", "+", "12", "+6598004075123456789", "６５９８"}
	for _, in := range bad {
		if _, err := normalizePhone(in); err == nil {
			t.Errorf("%q 应被拒绝", in)
		}
	}
}

func TestPhoneErrorMentionsCountryCode(t *testing.T) {
	_, err := normalizePhone("abc")
	if err == nil || !strings.Contains(err.Error(), "国家区号") {
		t.Fatalf("错误应提示国家区号：%v", err)
	}
}
