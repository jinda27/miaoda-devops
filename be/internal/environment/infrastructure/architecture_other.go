//go:build !darwin && !windows

package infrastructure

import "runtime"

func nativeArchitecture() string {
	return runtime.GOARCH
}
