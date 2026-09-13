package main

import "os"

// osFindProcess 包一层，便于 pidfile.go 不直接依赖 os 的 FindProcess 语义。
func osFindProcess(pid int) (*os.Process, error) {
	return os.FindProcess(pid)
}
