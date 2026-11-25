//go:build !windows

package main

// On Linux and macOS AMS runs from a terminal or a service: no window to
// own, and no browser to open.
func ownConsole() bool       { return false }
func setConsoleTitle(string) {}
func openBrowser(string)     {}
