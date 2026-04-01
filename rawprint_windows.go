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
	enumPrintersW    = winspool.NewProc("EnumPrintersW")
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

// PRINTER_INFO_2W matches the Windows PRINTER_INFO_2W struct layout.
// We only need pPrinterName and Status; the rest are placeholders.
type printerInfo2W struct {
	pServerName        *uint16
	pPrinterName       *uint16
	pShareName         *uint16
	pPortName          *uint16
	pDriverName        *uint16
	pComment           *uint16
	pLocation          *uint16
	pDevMode           uintptr
	pSepFile           *uint16
	pPrintProcessor    *uint16
	pDatatype          *uint16
	pParameters        *uint16
	pSecurityDescriptor uintptr
	Attributes         uint32
	Priority           uint32
	DefaultPriority    uint32
	StartTime          uint32
	UntilTime          uint32
	Status             uint32
	cJobs              uint32
	AveragePPM         uint32
}

const (
	printerEnumLocal      = 0x00000002
	printerEnumConnections = 0x00000004
)

// enumLocalPrinters lists printers using the Windows EnumPrintersW API.
func enumLocalPrinters() ([]PrinterInfo, error) {
	flags := uintptr(printerEnumLocal | printerEnumConnections)
	level := uintptr(2) // PRINTER_INFO_2

	// First call: get required buffer size
	var needed, returned uint32
	enumPrintersW.Call(flags, 0, level, 0, 0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)))

	if needed == 0 {
		return []PrinterInfo{}, nil
	}

	buf := make([]byte, needed)
	ret, _, _ := enumPrintersW.Call(flags, 0, level,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)))

	if ret == 0 {
		return nil, fmt.Errorf("EnumPrintersW failed")
	}

	// Interpret buffer as array of printerInfo2W
	structSize := unsafe.Sizeof(printerInfo2W{})
	printers := make([]PrinterInfo, 0, returned)
	for i := uint32(0); i < returned; i++ {
		info := (*printerInfo2W)(unsafe.Pointer(&buf[uintptr(i)*structSize]))
		name := syscall.UTF16ToString((*[1024]uint16)(unsafe.Pointer(info.pPrinterName))[:])
		// Status == 0 means the printer is ready/idle
		enabled := info.Status == 0
		printers = append(printers, PrinterInfo{Name: name, Enabled: enabled})
	}
	return printers, nil
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
