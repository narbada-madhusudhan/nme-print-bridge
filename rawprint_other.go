//go:build !windows

package main

import "fmt"

func sendRawToPrinter(printerName string, data []byte) error {
	return fmt.Errorf("sendRawToPrinter is only supported on Windows")
}

func canOpenPrinter(printerName string) bool {
	return false
}
