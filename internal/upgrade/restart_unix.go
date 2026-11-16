//go:build !windows

package upgrade

import (
	"fmt"
	"os"
	"syscall"
)

const posixWOK = 0x2

// checkDirWritable probes write permission on dir using access(2) without creating or deleting files.
func checkDirWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", dir)
	}
	if err := syscall.Access(dir, posixWOK); err != nil {
		return fmt.Errorf("当前运行用户对目录 %s 无写入权限: %w", dir, err)
	}
	return nil
}

// RestartProcess replaces the current Unix process image in-place with launchPath
// (preserving the same PID under systemd, launchd, Docker, or a terminal).
func RestartProcess(launchPath string, argv []string, envv []string) error {
	if len(argv) == 0 {
		argv = []string{launchPath}
	} else {
		copied := make([]string, len(argv))
		copy(copied, argv)
		copied[0] = launchPath
		argv = copied
	}
	return syscall.Exec(launchPath, argv, envv)
}
