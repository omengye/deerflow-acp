package skills

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var reopenEditFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// Replacements must not inherit a broader staging-directory DACL than the
// original credential-bearing file. Fail closed if it cannot be preserved.
func preserveEditSecurity(root *os.Root, name string, replacement *os.File) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read source DACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return setSkillFileDACL(replacement, dacl)
}

// Snapshot objects can contain credentials from installed support files. Their
// Windows ACL must not inherit broad read access from the portable parent.
func secureSnapshotFile(file *os.File) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return setSkillFileDACL(file, dacl)
}

func setSkillFileDACL(replacement *os.File, dacl *windows.ACL) error {
	// os.Root.OpenFile requests GENERIC_WRITE, not WRITE_DAC. Reopen by
	// handle, never by an absolute path that could follow a replaced parent.
	handle, _, callErr := reopenEditFile.Call(replacement.Fd(), windows.WRITE_DAC|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, 0)
	if handle == ^uintptr(0) {
		return fmt.Errorf("reopen staged DACL: %w", callErr)
	}
	defer windows.CloseHandle(windows.Handle(handle))
	if err := windows.SetSecurityInfo(windows.Handle(handle), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("preserve staged DACL: %w", err)
	}
	return nil
}
