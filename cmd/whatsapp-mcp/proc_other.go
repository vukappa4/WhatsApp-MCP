//go:build unix && !darwin && !linux

package main

import "errors"

func processImage(int) (string, error) { return "", errors.ErrUnsupported }
