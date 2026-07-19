//go:build !linux && !darwin && !windows

package designcontract

import (
	"errors"
	"os"
)

func publishProjectionFile(_, _ *os.Root, _ *os.File, _ os.FileInfo, _, _ string, _ os.FileInfo, _ func() error) error {
	return errors.New("atomic design contract projection publication is unavailable on this platform; canonical SQLite state remains available")
}

func publishProjectionDirectory(_ *os.Root, _, _ string, _ os.FileInfo) error {
	return errors.New("atomic design contract staging directory publication is unavailable on this platform")
}
