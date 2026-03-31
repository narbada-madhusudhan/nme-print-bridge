package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"time"
)

// ─── Data Helpers ──────────────────────────────────────────────────────────

func decodeData(b64, raw string) ([]byte, error) {
	if b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 data: %w", err)
		}
		return data, nil
	}
	return []byte(raw), nil
}

func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address: %s", ip)
	}
	return nil
}

// tcpSend dials a network printer and writes raw data.
func tcpSend(ip string, port int, data []byte) error {
	if port == 0 {
		port = DefaultPrinterPort
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, time.Duration(NetworkDialTimeout)*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(time.Duration(NetworkWriteTimeout) * time.Second))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", addr, err)
	}
	return nil
}
