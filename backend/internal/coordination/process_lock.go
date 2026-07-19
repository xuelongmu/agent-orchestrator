package coordination

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const processLockFile = "ao.db.lock"

var errProcessLocked = errors.New("exclusive database-writer OS lock is already held")

// processLock is the bootstrap fence around opening and migrating ao.db. The
// file is deliberately persistent: ownership lives in the kernel lock, not in
// the presence or contents of the file.
type processLock struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

func acquireProcessLock(dataDir string) (*processLock, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir for database-writer lock: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dataDir, processLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database-writer lock file: %w", err)
	}
	if err := tryLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &processLock{file: file}, nil
}

func (l *processLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}
