//go:build linux

package proc

import (
	"bytes"
	"os"
	"strconv"
)

// processAlive reports whether pid is a live process (not a zombie that
// only waits to be reaped).
func processAlive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) S ...": the state follows the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return false
	}
	state := stat[i+2]
	return state != 'Z' && state != 'X'
}
