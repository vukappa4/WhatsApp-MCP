//go:build !windows && !darwin

package main

import (
	"errors"

	"github.com/vukappa4/whatsapp-mcp/internal/config"
)

const exeName = "whatsapp-mcp"

var errNoService = errors.New("install and uninstall support Windows (Task Scheduler) and macOS (launchd) only: start `whatsapp-mcp run` with your own service manager")

func serviceName(p config.Profile) string { return "the autostart of profile " + string(p) }

func registerService(*config.Config, string, string) (string, error) { return "", errNoService }
func startService(*config.Config) error                              { return errNoService }
func removeService(*config.Config) (bool, error)                     { return false, nil }
func serviceInstalled(config.Profile) bool                           { return false }
func serviceState(config.Profile) string                             { return "not available on this OS" }
func serviceRestartNote(config.Profile) string                       { return "" }
