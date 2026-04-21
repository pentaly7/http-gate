//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type termios struct {
	Iflag, Oflag, Cflag, Lflag uint32
	Cc                         [20]byte
	Ispeed, Ospeed             uint32
}

var originalTermios termios

func setRawMode() error {
	var t termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		os.Stdin.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}
	originalTermios = t
	t.Lflag &^= syscall.ECHO | syscall.ICANON
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		os.Stdin.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}
	return nil
}

func restoreTerminal() {
	syscall.Syscall(syscall.SYS_IOCTL,
		os.Stdin.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&originalTermios)))
}

func rawLoop(gate *Gate) {
	defer restoreTerminal()
	buf := make([]byte, 1)
	for {
		os.Stdin.Read(buf)
		switch buf[0] {
		case 13, 10:
			n := gate.ReleaseAll()
			if n == 0 {
				fmt.Println("[GATE] Nothing held — send some requests first.")
			}
		case 'q', 'Q', 3:
			restoreTerminal()
			fmt.Println("\nGoodbye.")
			os.Exit(0)
		default:
			fmt.Printf("[GATE] %d request(s) held. Press ENTER to release all.\n", gate.QueueLen())
		}
	}
}
