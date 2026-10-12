package localhost

import (
	"path/filepath"
	"testing"
	"unsafe"

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
		// SDDL renders the built-in Administrator SID as LA on CI runners.
		// Compare the actual ACEs, not that account-dependent string spelling.
		var hasUser, hasSystem bool
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			const fileAllAccess = 0x1f01ff // FILE_ALL_ACCESS, the SDDL FA grant.
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != fileAllAccess {
				t.Fatalf("unexpected access entry on %s: %+v", name, ace)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			hasUser = hasUser || sid.Equals(user.User.Sid)
			hasSystem = hasSystem || sid.IsWellKnown(windows.WinLocalSystemSid)
		}
		if !hasUser || !hasSystem {
			t.Fatalf("missing user/SYSTEM access: %s", sd.String())
		}
	}
}
