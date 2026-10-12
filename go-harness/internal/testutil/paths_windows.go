package testutil

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// ShortPath requires a real alias so an alias regression never silently tests
// the long spelling twice. Volumes with 8.3 names disabled cannot run it.
func ShortPath(t testing.TB, path string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 32768)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || n >= uint32(len(buf)) {
		t.Fatalf("short path: length=%d err=%v", n, err)
	}
	short := windows.UTF16ToString(buf[:n])
	if strings.EqualFold(short, path) {
		t.Skip("filesystem does not provide an 8.3 alias")
	}
	return short
}
