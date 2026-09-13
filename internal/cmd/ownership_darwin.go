package cmd

import "golang.org/x/sys/unix"

func init() {
	readDarwinProcessArgs = func(pid int) ([]byte, error) {
		return unix.SysctlRaw("kern.procargs2", pid)
	}
}
