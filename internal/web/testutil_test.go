package web

import (
	"os"
	"time"
)

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func sleepShort() { time.Sleep(50 * time.Millisecond) }
