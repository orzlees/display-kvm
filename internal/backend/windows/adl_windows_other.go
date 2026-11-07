//go:build windows && !amd64

package windows

import "runtime"

func openPlatformADL() (adlSession, error) {
	return nil, requireADLArchitecture(runtime.GOARCH)
}
