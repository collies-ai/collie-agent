//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// stdinIsTerminal reports whether the process's stdin is a terminal. It asks the terminal driver for its settings
// (tcgetattr): character devices that are not terminals, such as /dev/null, fail it.
func stdinIsTerminal() bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
