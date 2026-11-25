//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// ownConsole is true when ams.exe has its console window to itself: it was
// double-clicked, not started from a terminal.
func ownConsole() bool {
	proc := kernel32.NewProc("GetConsoleProcessList")
	if proc.Find() != nil {
		return false
	}
	var ids [4]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}

func setConsoleTitle(title string) {
	proc := kernel32.NewProc("SetConsoleTitleW")
	if proc.Find() != nil {
		return
	}
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	proc.Call(uintptr(unsafe.Pointer(p)))
}

func openBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
