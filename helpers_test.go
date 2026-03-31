package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestDecodeData_Base64(t *testing.T) {
	original := []byte{0x1B, 0x40, 0x48, 0x65, 0x6C, 0x6C, 0x6F} // ESC @ Hello
	encoded := base64.StdEncoding.EncodeToString(original)

	result, err := decodeData(encoded, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(original) {
		t.Errorf("got %v, want %v", result, original)
	}
}

func TestDecodeData_Raw(t *testing.T) {
	result, err := decodeData("", "Hello printer")
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "Hello printer" {
		t.Errorf("got %q, want %q", result, "Hello printer")
	}
}

func TestDecodeData_Base64Priority(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("from base64"))
	result, err := decodeData(b64, "from raw")
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "from base64" {
		t.Errorf("base64 should take priority, got %q", result)
	}
}

func TestDecodeData_InvalidBase64(t *testing.T) {
	_, err := decodeData("not-valid-base64!!!", "")
	if err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestDecodeData_EmptyBoth(t *testing.T) {
	result, err := decodeData("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestValidateIP_Valid(t *testing.T) {
	valid := []string{"192.168.1.1", "10.0.0.1", "127.0.0.1", "::1", "fe80::1"}
	for _, ip := range valid {
		if err := validateIP(ip); err != nil {
			t.Errorf("validateIP(%q) should be valid, got: %v", ip, err)
		}
	}
}

func TestValidateIP_Invalid(t *testing.T) {
	invalid := []string{"", "not-an-ip", "192.168.1.999", "hostname.local", "192.168.1"}
	for _, ip := range invalid {
		if err := validateIP(ip); err == nil {
			t.Errorf("validateIP(%q) should fail", ip)
		}
	}
}

func TestTcpSend_Success(t *testing.T) {
	// Start a mock TCP server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	received := make(chan []byte, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		received <- data
	}()

	testData := []byte("\x1B@Hello Printer\n\n\n\n\n\n\x1DV\x00")
	err = tcpSend("127.0.0.1", port, testData)
	if err != nil {
		t.Fatalf("tcpSend failed: %v", err)
	}

	select {
	case data := <-received:
		if string(data) != string(testData) {
			t.Errorf("received %q, want %q", data, testData)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for data")
	}
}

func TestTcpSend_DefaultPort(t *testing.T) {
	// Port 0 should default to DefaultPrinterPort (9100) — will fail to connect but tests the path
	err := tcpSend("127.0.0.1", 0, []byte("test"))
	if err == nil {
		t.Error("expected connection error to port 9100")
	}
}

func TestTcpSend_ConnectionRefused(t *testing.T) {
	// Use a port that's definitely not listening
	err := tcpSend("127.0.0.1", 59999, []byte("test"))
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestTcpSend_SimulatePrint(t *testing.T) {
	// Simulate a full ESC/POS print job through TCP
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	received := make(chan []byte, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		received <- data
	}()

	// Generate ESC/POS from structured content (like a real KOT)
	content := []byte(`{
		"header": "KOT #42",
		"subheader": "Table 7 - Dine In",
		"lines": [
			{"text": "2x Butter Chicken", "bold": true},
			{"text": "1x Naan"},
			{"separator": true},
			{"columns": ["Total Items", "3"]}
		],
		"footer": "Kitchen Copy"
	}`)
	escposData := contentToEscPos(content)

	err = tcpSend("127.0.0.1", port, escposData)
	if err != nil {
		t.Fatalf("tcpSend failed: %v", err)
	}

	select {
	case data := <-received:
		s := string(data)
		// Verify the ESC/POS data arrived intact
		if !containsBytes(data, []byte(escInit)) {
			t.Error("missing ESC @ init command")
		}
		if !containsStr(s, "KOT #42") {
			t.Error("missing header")
		}
		if !containsStr(s, "Butter Chicken") {
			t.Error("missing menu item")
		}
		if !containsBytes(data, []byte(escBoldOn)) {
			t.Error("missing bold command")
		}
		if !containsBytes(data, []byte(escCut)) {
			t.Error("missing paper cut command")
		}
		t.Logf("Simulated print: %d bytes sent to mock printer", len(data))
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestTcpSend_MultiplePrints(t *testing.T) {
	// Simulate printing to multiple "printers" concurrently
	printers := make([]net.Listener, 3)
	for i := range printers {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		printers[i] = l
	}

	// Accept connections on all printers
	results := make([]chan []byte, 3)
	for i, l := range printers {
		results[i] = make(chan []byte, 1)
		go func(listener net.Listener, ch chan []byte) {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			data, _ := io.ReadAll(conn)
			ch <- data
		}(l, results[i])
	}

	// Send different content to each printer
	for i, l := range printers {
		port := l.Addr().(*net.TCPAddr).Port
		data := []byte(fmt.Sprintf("\x1B@Print job %d\n\n\n\n\n\n\x1DV\x00", i+1))
		if err := tcpSend("127.0.0.1", port, data); err != nil {
			t.Fatalf("printer %d: %v", i, err)
		}
	}

	for i, ch := range results {
		select {
		case data := <-ch:
			expected := fmt.Sprintf("Print job %d", i+1)
			if !containsStr(string(data), expected) {
				t.Errorf("printer %d: expected %q in data", i, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("printer %d: timeout", i)
		}
	}
}

func containsBytes(haystack, needle []byte) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		string(haystack) != "" && contains(string(haystack), string(needle))
}

func containsStr(s, substr string) bool {
	return contains(s, substr)
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
