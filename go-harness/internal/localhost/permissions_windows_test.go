package localhost

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEndpointProtectedDACL(t *testing.T) {
	dir := t.TempDir()
	_, _ = startHost(t, Config{RuntimeDir: dir})
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dir, filepath.Join(dir, EndpointFilename), filepath.Join(dir, "daemon.lock")} {
		sd, err := windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("inheritable DACL on %s: %v", name, err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 2 {
			t.Fatalf("unexpected ACL on %s: %v", name, err)
		}
		if s := sd.String(); !strings.Contains(s, user.User.Sid.String()) || !strings.Contains(s, ";;;SY)") {
			t.Fatalf("missing user/SYSTEM access: %s", s)
		}
	}
}
