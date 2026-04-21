//go:build windows

package main

import "errors"

func setRawMode() error {
	return errors.New("raw mode not supported on Windows")
}

func restoreTerminal() {}

func rawLoop(gate *Gate) {
	lineBufferedLoop(gate)
}
