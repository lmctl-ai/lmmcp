package bridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func prepareProcess(cmd *exec.Cmd) {}
func stopProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		program := filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe")
		_ = exec.Command(program, "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		_ = cmd.Process.Kill()
	}
}
