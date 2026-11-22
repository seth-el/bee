//go:build !linux

package main

import "os"

func terminalFile(_ *os.File) bool { return false }
