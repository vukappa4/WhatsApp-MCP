//go:build windows

package instance

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createLockedDown creates the profile mutex with an empty protected DACL:
// nobody, this user included, may open it by name.
func createLockedDown(t *testing.T, profile string) func() {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:P")
	if err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(`Global\whatsapp-mcp-` + profile)
	h, err := windows.CreateMutex(&windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, false, name)
	if err != nil {
		t.Fatal(err)
	}
	return func() { windows.CloseHandle(h) }
}
