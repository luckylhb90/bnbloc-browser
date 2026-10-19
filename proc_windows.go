//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// detach starts cmd in its own process group, so the browser outlives the
// tool process that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// lockFile takes an exclusive lock by creating path, waiting while another
// process holds it.
func lockFile(path string) (func(), error) {
	for {
		unlock, ok, err := tryLockFile(path)
		if err != nil || ok {
			return unlock, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tryLockFile creates path exclusively; ok false when it exists.
func tryLockFile(path string) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { f.Close(); _ = os.Remove(path) }, true, nil
}

// processAlive reports whether process pid is still running.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if syscall.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}
