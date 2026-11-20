//go:build windows

// Package ownerfile keeps a file that holds a credential readable by its
// owner only: mode 0600 on Unix, a protected DACL on Windows, where the mode
// bits mean nothing (#206).
package ownerfile

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// RestrictToUser replaces path's DACL with one protected entry granting the
// current user full access, so neither inherited nor other users' entries
// can read the token.
func RestrictToUser(path string) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(u.User.Sid),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// OnlyUser reports whether path's DACL is protected (no inherited entries)
// and holds exactly one allow entry, the current user's.
func OnlyUser(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		return false, err
	}
	if ctrl&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return false, err
	}
	if acl.AceCount != 1 {
		return false, nil
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err = windows.GetAce(acl, 0, &ace); err != nil {
		return false, err
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	return ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), u.User.Sid), nil
}
