package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	winspool         = syscall.NewLazyDLL("winspool.drv")
	openPrinterW     = winspool.NewProc("OpenPrinterW")
	startDocPrinterW = winspool.NewProc("StartDocPrinterW")
	startPagePrinter = winspool.NewProc("StartPagePrinter")
	writePrinter     = winspool.NewProc("WritePrinter")
	endPagePrinter   = winspool.NewProc("EndPagePrinter")
	endDocPrinter    = winspool.NewProc("EndDocPrinter")
	closePrinter     = winspool.NewProc("ClosePrinter")
)

// DOC_INFO_1W matches the Windows DOC_INFO_1W struct layout.
type docInfo1W struct {
	pDocName    *uint16
	pOutputFile *uint16
	pDatatype   *uint16
}

// sendRawToPrinter sends raw bytes directly to a Windows printer via winspool.drv.
func sendRawToPrinter(printerName string, data []byte) error {
	pName, err := syscall.UTF16PtrFromString(printerName)
	if err != nil {
		return fmt.Errorf("invalid printer name: %w", err)
	}

	var hPrinter uintptr
	ret, _, _ := openPrinterW.Call(uintptr(unsafe.Pointer(pName)), uintptr(unsafe.Pointer(&hPrinter)), 0)
	if ret == 0 {
		return fmt.Errorf("OpenPrinter failed for %q", printerName)
	}
	defer closePrinter.Call(hPrinter)

	pDocName, _ := syscall.UTF16PtrFromString("NME Print")
	pDatatype, _ := syscall.UTF16PtrFromString("RAW")
	docInfo := docInfo1W{
		pDocName:  pDocName,
		pDatatype: pDatatype,
	}

	ret, _, _ = startDocPrinterW.Call(hPrinter, 1, uintptr(unsafe.Pointer(&docInfo)))
	if ret == 0 {
		return fmt.Errorf("StartDocPrinter failed for %q", printerName)
	}
	defer endDocPrinter.Call(hPrinter)

	ret, _, _ = startPagePrinter.Call(hPrinter)
	if ret == 0 {
		return fmt.Errorf("StartPagePrinter failed for %q", printerName)
	}
	defer endPagePrinter.Call(hPrinter)

	var written uint32
	ret, _, _ = writePrinter.Call(
		hPrinter,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
	)
	if ret == 0 {
		return fmt.Errorf("WritePrinter failed for %q", printerName)
	}
	if int(written) != len(data) {
		return fmt.Errorf("WritePrinter: wrote %d of %d bytes", written, len(data))
	}
	return nil
}

// canOpenPrinter checks if a printer is registered with the Windows print spooler.
func canOpenPrinter(printerName string) bool {
	pName, err := syscall.UTF16PtrFromString(printerName)
	if err != nil {
		return false
	}
	var hPrinter uintptr
	ret, _, _ := openPrinterW.Call(uintptr(unsafe.Pointer(pName)), uintptr(unsafe.Pointer(&hPrinter)), 0)
	if ret == 0 {
		return false
	}
	closePrinter.Call(hPrinter)
	return true
}
