//go:build linux

package main

import (
	"os"
	"strconv"
)

// processImage is the full path of a running process's executable.
func processImage(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}
