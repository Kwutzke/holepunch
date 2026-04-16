package cmd

import "syscall"

// sysProcAttr detaches the daemon process so it survives the parent exiting.
var sysProcAttr = syscall.SysProcAttr{
	Setsid: true,
}
