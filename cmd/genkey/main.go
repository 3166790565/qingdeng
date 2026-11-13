// Command genkey 生成一串足够长的随机密钥，用来填 .env 里的 SECRET_KEY。
//
//	go run ./cmd/genkey
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		fmt.Fprintln(os.Stderr, "生成失败：", err)
		os.Exit(1)
	}
	fmt.Println(strings.TrimRight(base64.URLEncoding.EncodeToString(buf), "="))
}
