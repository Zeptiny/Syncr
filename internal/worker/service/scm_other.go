//go:build !windows

package service

import "fmt"

// serviceCommandLine reads a Windows Service's command line; there is no
// Service Control Manager here.
func serviceCommandLine(string) (string, error) {
	return "", fmt.Errorf("%w: there is no Windows Service Control Manager on this system", ErrUnsupported)
}
