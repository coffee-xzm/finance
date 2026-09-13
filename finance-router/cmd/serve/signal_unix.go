//go:build unix

package main

import (
	"syscall"
)

// syscallSignal0 用 signal 0 探测进程是否存在（不实际发信号）。
func syscallSignal0(pid int) error {
	p, err := osFindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}
