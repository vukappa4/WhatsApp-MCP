//go:build windows

package useracl

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/sys/windows"
)

// Restrict gives path a protected DACL that grants full control to the
// current user only, inherited by files when dir is true.
func Restrict(path string, dir bool) error {
	sid, err := CurrentUserSID()
	if err != nil {
		return err
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;%s;FA;;;%s)", inherit, sid))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// CurrentUserSID is the SID of the user running this process.
func CurrentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

// fileDACL returns the SDDL of path's DACL.
func fileDACL(path string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}

// canonicalSID turns an SDDL SID token, which may be an alias like LA for
// the built-in Administrator, into its S-1-... form.
func canonicalSID(token string) (string, error) {
	sid, err := windows.StringToSid(token)
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

// aceRe matches an allow ACE in SDDL and captures its SID token.
var aceRe = regexp.MustCompile(`\(A;[^;]*;[^;]*;[^;]*;[^;]*;([^)]+)\)`)

// Check returns an error unless path has a protected DACL with a single
// ACE, granting the current user.
func Check(path string) error {
	sid, err := CurrentUserSID()
	if err != nil {
		return err
	}
	sddl, err := fileDACL(path)
	if err != nil {
		return err
	}
	aces := aceRe.FindAllStringSubmatch(sddl, -1)
	if !strings.HasPrefix(sddl, "D:P") || strings.Count(sddl, "(") != 1 || len(aces) != 1 {
		return fmt.Errorf("%s: DACL %s, want one ACE for %s", path, sddl, sid)
	}
	// SDDL writes some SIDs as aliases, like LA when the user is the built-in Administrator.
	if got, err := canonicalSID(aces[0][1]); err != nil || got != sid {
		return fmt.Errorf("%s: DACL %s grants %s (%v), want %s", path, sddl, got, err, sid)
	}
	return nil
}
