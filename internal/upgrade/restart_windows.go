//go:build windows

package upgrade

import (
	"fmt"
	"os"
	"os/exec"
)

// checkDirWritable verifies that dir exists and is not marked read-only on Windows.
func checkDirWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", dir)
	}
	if info.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("目录 %s 为只读状态", dir)
	}
	return nil
}

// RestartProcess spawns a replacement instance on Windows after listeners and SQLite handles
// have been closed, allowing the caller to exit cleanly.
func RestartProcess(launchPath string, argv []string, envv []string) error {
	var passArgs []string
	if len(argv) > 1 {
		passArgs = argv[1:]
	}
	cmd := exec.Command(launchPath, passArgs...)
	cmd.Env = envv
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Start()
}
