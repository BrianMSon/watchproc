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

// Special key constants
const (
	KeyUp    byte = 0x80
	KeyDown  byte = 0x81
	KeyLeft  byte = 0x82
	KeyRight byte = 0x83
)

// Windows INPUT_RECORD structures for ReadConsoleInput
type keyEventRecord struct {
	bKeyDown         int32
	wRepeatCount     uint16
	wVirtualKeyCode  uint16
	wVirtualScanCode uint16
	uChar            uint16 // UnicodeChar
	dwControlKeyState uint32
}

type inputRecord struct {
	eventType uint16
	_         uint16 // padding
	event     [16]byte
}

var procReadConsoleInput = kernel32.NewProc("ReadConsoleInputW")

const (
	_KEY_EVENT = 0x0001
	_VK_UP     = 0x26
	_VK_DOWN   = 0x28
	_VK_LEFT   = 0x25
	_VK_RIGHT  = 0x27
	_VK_ESCAPE = 0x1B
	_VK_RETURN = 0x0D
)

func readKey() (byte, bool) {
	var rec inputRecord
	var numRead uint32

	for {
		r, _, _ := procReadConsoleInput.Call(
			uintptr(stdinHandle),
			uintptr(unsafe.Pointer(&rec)),
			1,
			uintptr(unsafe.Pointer(&numRead)),
		)
		if r == 0 || numRead == 0 {
			return 0, false
		}

		// KEY_EVENT only, key down only
		if rec.eventType != _KEY_EVENT {
			continue
		}
		keyEvent := (*keyEventRecord)(unsafe.Pointer(&rec.event[0]))
		if keyEvent.bKeyDown == 0 {
			continue
		}

		// Virtual key code check
		switch keyEvent.wVirtualKeyCode {
		case _VK_UP:
			return KeyUp, true
		case _VK_DOWN:
			return KeyDown, true
		case _VK_LEFT:
			return KeyLeft, true
		case _VK_RIGHT:
			return KeyRight, true
		case _VK_ESCAPE:
			return 27, true
		case _VK_RETURN:
			return 13, true
		}

		// Character key
		ch := byte(keyEvent.uChar)
		if ch > 0 {
			return ch, true
		}
	}
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
