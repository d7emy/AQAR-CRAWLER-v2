//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

type keyEventRecord struct {
	EventType         uint16
	_                 uint16
	BKeyDown          int32
	WRepeatCount      uint16
	WVirtualKeyCode   uint16
	WVirtualScanCode  uint16
	UChar             uint16
	DwControlKeyState uint32
}

// dismissBatchCtrlCPrompt injects "Y\r" into the console input buffer when
// launched from a Windows .bat script (AQAR_FROM_BAT=1) so cmd.exe's
// "Terminate batch job (Y/N)?" prompt is answered automatically on Ctrl+C.
func dismissBatchCtrlCPrompt() {
	if os.Getenv("AQAR_FROM_BAT") != "1" {
		return
	}
	conIn, err := syscall.UTF16PtrFromString("CONIN$")
	if err != nil {
		return
	}
	h, err := syscall.CreateFile(
		conIn,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)

	const keyEvent = 0x0001
	records := [4]keyEventRecord{
		{EventType: keyEvent, BKeyDown: 1, WRepeatCount: 1, WVirtualKeyCode: 'Y', UChar: 'Y'},
		{EventType: keyEvent, BKeyDown: 0, WRepeatCount: 1, WVirtualKeyCode: 'Y', UChar: 'Y'},
		{EventType: keyEvent, BKeyDown: 1, WRepeatCount: 1, WVirtualKeyCode: 0x0D, UChar: '\r'},
		{EventType: keyEvent, BKeyDown: 0, WRepeatCount: 1, WVirtualKeyCode: 0x0D, UChar: '\r'},
	}
	var written uint32
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	writeConsoleInputW := kernel32.NewProc("WriteConsoleInputW")
	_, _, _ = writeConsoleInputW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&records[0])),
		uintptr(len(records)),
		uintptr(unsafe.Pointer(&written)),
	)
}
