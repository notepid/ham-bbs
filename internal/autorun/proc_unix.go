//go:build !windows

package autorun

import (
	"os"
	"os/exec"
	"syscall"
)

// setup puts the program in its own process group, so Ctrl-C in the
// terminal reaches only HamBBS, which then stops the program itself.
func setup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setDeathSignal(cmd.SysProcAttr)
}

func attach(*os.Process) {}

func interrupt(p *os.Process) error {
	return p.Signal(os.Interrupt)
}
