//go:build windows

package designcontract

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

var windowsCleanupValidatedHook func(*os.Root, string) error

func cleanupOwnedProjectionStages(root *os.Root, ownershipTarget string) error {
	entries, err := readRootEntries(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isProjectionStage(name) {
			continue
		}
		validated, identity, content, err := openValidatedProjectionFile(root, name)
		if err != nil || !strings.HasPrefix(string(content), projectionOwnershipMarker(ownershipTarget)) {
			if validated != nil {
				_ = validated.Close()
			}
			continue
		}
		_ = validated.Close()
		locked, handle, err := openLockedWindowsProjectionFile(root, name, windows.GENERIC_READ|windows.DELETE, identity)
		if err != nil {
			return fmt.Errorf("lock owned projection stage for cleanup: %w", err)
		}
		lockedContent, readErr := io.ReadAll(io.LimitReader(locked, MaxCanonicalBytes+64*1024))
		if readErr != nil || !strings.HasPrefix(string(lockedContent), projectionOwnershipMarker(ownershipTarget)) {
			_ = locked.Close()
			return errors.New("owned projection stage changed before handle-bound cleanup")
		}
		if windowsCleanupValidatedHook != nil {
			if err := windowsCleanupValidatedHook(root, name); err != nil {
				_ = locked.Close()
				return err
			}
		}
		deleteOnClose := byte(1)
		if err := windowsProjectionAPI.setFileInformationByHandle(handle, windows.FileDispositionInfo, &deleteOnClose, 1); err != nil {
			_ = locked.Close()
			return fmt.Errorf("mark owned projection stage for handle-bound cleanup: %w", err)
		}
		if err := locked.Close(); err != nil {
			return fmt.Errorf("close handle-bound projection stage cleanup: %w", err)
		}
	}
	return syncProjectionContainer(root)
}
