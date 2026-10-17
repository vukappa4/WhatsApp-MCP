// Package manifest links the Windows application manifest of whatsapp-mcp
// (rsrc_windows_*.syso, built from whatsapp-mcp.manifest): a console program
// whose console allocation policy is "detached", so a start without a
// console (Task Scheduler) creates no window.
//
// The main package imports it only in cgo-free builds, which is how the
// daemon is built (CGO_ENABLED=0). A cgo build (go test -race) links with
// the C toolchain, which adds its own default manifest: two manifests do not
// merge, so such builds go without this one.
package manifest

//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -manifest whatsapp-mcp.manifest -o rsrc_windows_amd64.syso
//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch arm64 -manifest whatsapp-mcp.manifest -o rsrc_windows_arm64.syso
