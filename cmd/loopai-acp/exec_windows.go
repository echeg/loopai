//go:build windows

package main

// platformLaunch starts loopai as a child with inherited stdio: Windows cannot replace a process.
var platformLaunch = launchInherited
