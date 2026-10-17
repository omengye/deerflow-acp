//go:build !windows && !linux && !darwin

package skills

import "os"

func renameNoReplace(root *os.Root, from, to string) error {
	return denied("exclusive skill publication is unsupported on this platform")
}
