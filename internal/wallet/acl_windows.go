//go:build windows

package wallet

import (
	"golang.org/x/sys/windows"
)

// hardenFileACL restricts path's Windows NTFS DACL to the current user SID only
// (protected DACL, no inheritance from parent directory), closing F-33 on Windows hosts.
func hardenFileACL(path string) error {
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return err
	}
	ea := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	dacl, err := windows.ACLFromEntries(ea, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}
