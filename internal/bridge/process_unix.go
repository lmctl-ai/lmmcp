//go:build !windows

package bridge

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func prepareProcess(cmd *exec.Cmd) { cmd.SysProcAttr = sysProcAttr() }
func stopProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

type unixPolledReader struct {
	ctx context.Context
	fd  int
}

func (r *unixPolledReader) Read(p []byte) (int, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := syscall.Read(r.fd, p)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func openPolledReader(ctx context.Context, in io.Reader) (io.Reader, func()) {
	f, ok := in.(*os.File)
	if !ok {
		return in, func() {}
	}
	dupFD, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return in, func() {}
	}
	if err := syscall.SetNonblock(dupFD, true); err != nil {
		_ = syscall.Close(dupFD)
		return in, func() {}
	}
	return &unixPolledReader{ctx: ctx, fd: dupFD}, func() {
		_ = syscall.SetNonblock(dupFD, false)
		_ = syscall.Close(dupFD)
	}
}
