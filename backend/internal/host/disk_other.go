//go:build !linux

package host

import (
	"errors"
	"runtime"
)

// diskUsage is only implemented on Linux, where the dashboard runs. This stub
// lets the package build and its parser tests run on a Windows or macOS dev
// machine.
func diskUsage(string) (Disk, error) {
	return Disk{}, errors.New("disk usage not supported on " + runtime.GOOS)
}
