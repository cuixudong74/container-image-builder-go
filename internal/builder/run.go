package builder

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// RunInRootfs executes a command inside rootfs via chroot.
// It requires root privileges (CAP_SYS_CHROOT).
func RunInRootfs(rootfs string, args []string, env []string, workdir string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("RUN requires root privileges (chroot); current euid is %d", os.Geteuid())
	}
	if len(args) == 0 {
		return fmt.Errorf("RUN requires a command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = env
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: rootfs}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command %q in chroot %q failed: %w", args[0], rootfs, err)
	}
	return nil
}
