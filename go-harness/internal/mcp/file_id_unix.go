//go:build !windows

package mcp

import (
	"fmt"
	"os"
	"syscall"
)

func programFileID(_ string, file os.FileInfo) (string, error) {
	id, ok := file.Sys().(*syscall.Stat_t)
	if !ok {
		return "", os.ErrInvalid
	}
	return fmt.Sprintf("%x-%x", id.Dev, id.Ino), nil
}
