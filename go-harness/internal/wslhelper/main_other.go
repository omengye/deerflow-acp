//go:build !linux

package wslhelper

import (
	"fmt"
	"os"
)

func Main([]string) int { fmt.Fprintln(os.Stderr, "WSL helper requires Linux"); return 2 }
