//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

type consoleScreenBufferInfo struct {
	dwSize              [2]int16
	dwCursorPosition    [2]int16
	wAttributes         uint16
	srWindow            [4]int16
	dwMaximumWindowSize [2]int16
}

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	originalStdoutMode             uint32
	stdoutHandle                   syscall.Handle
	stdinHandle                    syscall.Handle
	originalStdinMode              uint32
)

const (
	_ENABLE_LINE_INPUT = 0x0002
	_ENABLE_ECHO_INPUT = 0x0004
)

const ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004

func setupConsole() {
	stdoutHandle, _ = syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	procGetConsoleMode.Call(uintptr(stdoutHandle), uintptr(unsafe.Pointer(&originalStdoutMode)))
	procSetConsoleMode.Call(uintptr(stdoutHandle), uintptr(originalStdoutMode|ENABLE_VIRTUAL_TERMINAL_PROCESSING))
}

func restoreConsole() {
	procSetConsoleMode.Call(uintptr(stdoutHandle), uintptr(originalStdoutMode))
}

func clearScreen() {
	cmd := exec.Command("cmd", "/c", "cls")
	cmd.Stdout = os.Stdout
	cmd.Run()
}

func moveCursor(row, col int) {
	fmt.Printf("\033[%d;%dH", row, col)
}

func clearToEnd() {
	fmt.Print("\033[J")
}

func clearLine() {
	fmt.Print("\033[K")
}

func enableRawInput() {
	stdinHandle, _ = syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	r, _, _ := procGetConsoleMode.Call(uintptr(stdinHandle), uintptr(unsafe.Pointer(&originalStdinMode)))
	if r == 0 {
		return
	}
	newMode := originalStdinMode &^ (_ENABLE_LINE_INPUT | _ENABLE_ECHO_INPUT)
	procSetConsoleMode.Call(uintptr(stdinHandle), uintptr(newMode))
}

func disableRawInput() {
	if stdinHandle != 0 {
		procSetConsoleMode.Call(uintptr(stdinHandle), uintptr(originalStdinMode))
	}
}

func readKey() (byte, bool) {
	buf := make([]byte, 1)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return 0, false
	}
	// Windows: special keys (arrows, F1-F12) start with 0x00 or 0xE0
	if buf[0] == 0x00 || buf[0] == 0xE0 {
		os.Stdin.Read(buf) // read and discard scan code
		return 0, false
	}
	return buf[0], true
}

// getTerminalSize 터미널 크기 가져오기
func getTerminalSize() (width, height int) {
	var info consoleScreenBufferInfo
	ret, _, _ := procGetConsoleScreenBufferInfo.Call(
		uintptr(stdoutHandle),
		uintptr(unsafe.Pointer(&info)),
	)
	if ret == 0 {
		return 80, 24
	}
	width = int(info.srWindow[2]-info.srWindow[0]) + 1
	height = int(info.srWindow[3]-info.srWindow[1]) + 1
	return width, height
}
