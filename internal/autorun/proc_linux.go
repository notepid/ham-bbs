package autorun

import "syscall"

// setDeathSignal stops the program if HamBBS dies without stopping it.
func setDeathSignal(a *syscall.SysProcAttr) {
	a.Pdeathsig = syscall.SIGTERM
}
