//go:build unix

package ptyregistry

import "os"

func atomicReplace(src, dst string) error { return os.Rename(src, dst) }
