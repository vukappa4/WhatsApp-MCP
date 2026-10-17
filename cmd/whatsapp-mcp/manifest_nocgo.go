//go:build windows && !cgo

package main

// The daemon's manifest (detached console) in the cgo-free production build.
import _ "github.com/vukappa4/whatsapp-mcp/cmd/whatsapp-mcp/manifest"
