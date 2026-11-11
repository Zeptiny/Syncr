//go:build windows

package service

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceCommandLine returns the command line (ImagePath) the Service
// Control Manager starts the service name with. It asks only for the rights
// to connect and to read the configuration, which every user has.
func serviceCommandLine(name string) (string, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", fmt.Errorf("service name %q: %w", name, err)
	}
	h, err := windows.OpenService(scm, namePtr, windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "", fmt.Errorf("%s: %w", name, ErrNotInstalled)
	}
	if err != nil {
		return "", fmt.Errorf("open the service %s: %w", name, err)
	}
	svc := &mgr.Service{Name: name, Handle: h}
	defer func() { _ = svc.Close() }()
	cfg, err := svc.Config()
	if err != nil {
		return "", fmt.Errorf("read the configuration of the service %s: %w", name, err)
	}
	return cfg.BinaryPathName, nil
}
