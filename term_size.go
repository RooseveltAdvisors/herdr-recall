package main

import (
	"os"
	"syscall"
	"unsafe"
)

// termSize measures the pane's own tty. The request number differs by OS:
// Linux TIOCGWINSZ is 0x5413, Darwin is 0x40087468. A wrong number fails the
// ioctl, and the 62 by 12 fallback then draws a small block in the top left
// of a large popup. A failure must not fall back to a wide default: a frame
// wider than the pane wraps and every row becomes unreadable.
func termSize(tty *os.File) (int, int) {
	var ws struct {
		Row, Col, X, Y uint16
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), uintptr(tiocgwinsz), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col < 8 || ws.Row < 4 {
		return 62, 12
	}
	return int(ws.Col), int(ws.Row)
}
