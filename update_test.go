package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchExpectedChecksum(t *testing.T) {
	manifest := "deadbeef01234567890123456789012345678901234567890123456789abcd  print-bridge-linux-amd64\n" +
		"ABCDEF01234567890123456789012345678901234567890123456789012345 *print-bridge-mac-arm64\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(manifest))
	}))
	defer srv.Close()

	tests := []struct {
		name      string
		assetName string
		wantHash  string
		wantErr   bool
	}{
		{"exact match", "print-bridge-linux-amd64", "deadbeef01234567890123456789012345678901234567890123456789abcd", false},
		{"binary-mode '*' prefix stripped, hash lowercased", "print-bridge-mac-arm64", "abcdef01234567890123456789012345678901234567890123456789012345", false},
		{"no entry for asset", "print-bridge-windows-amd64.exe", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fetchExpectedChecksum(srv.URL, tt.assetName)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got hash %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantHash {
				t.Errorf("hash = %q, want %q", got, tt.wantHash)
			}
		})
	}
}

func TestFetchExpectedChecksum_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchExpectedChecksum(srv.URL, "print-bridge-linux-amd64"); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

// TestPerformUpdate_FailsClosedWithoutChecksums verifies the update is
// refused entirely (never touches the filesystem or network for the
// binary) when the release has no SHA256SUMS asset to verify against.
func TestPerformUpdate_FailsClosedWithoutChecksums(t *testing.T) {
	info := &UpdateInfo{
		DownloadURL:   "https://github.com/narbada-madhusudhan/nme-print-bridge/releases/download/v9.9.9/print-bridge-linux-amd64",
		AssetName:     "print-bridge-linux-amd64",
		LatestVersion: "v9.9.9",
		ChecksumsURL:  "", // no manifest published
	}

	err := performUpdate(info)
	if err == nil {
		t.Fatal("expected performUpdate to fail closed when ChecksumsURL is empty")
	}
	if !strings.Contains(err.Error(), ChecksumsAssetName) {
		t.Errorf("error should explain the missing %s asset, got: %v", ChecksumsAssetName, err)
	}
}

// TestPerformUpdate_FailsClosedOnUnverifiableChecksums verifies the update
// is refused when the checksums manifest can't be fetched (e.g. network
// error, 404) — it must not fall back to applying an unverified binary.
func TestPerformUpdate_FailsClosedOnUnverifiableChecksums(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	info := &UpdateInfo{
		DownloadURL:   "https://github.com/narbada-madhusudhan/nme-print-bridge/releases/download/v9.9.9/print-bridge-linux-amd64",
		AssetName:     "print-bridge-linux-amd64",
		LatestVersion: "v9.9.9",
		ChecksumsURL:  srv.URL,
	}

	if err := performUpdate(info); err == nil {
		t.Fatal("expected performUpdate to fail closed when checksums manifest is unfetchable")
	}
}

// TestPerformUpdate_RejectsChecksumMismatch is the core security assertion:
// a downloaded binary whose sha256 doesn't match the SHA256SUMS manifest
// must be rejected and never swapped in. Only exercises the mismatch path
// (safe: performUpdate removes the temp file and returns before any
// os.Rename of the real executable). Does NOT test the matching-hash path,
// since that would replace the test binary itself.
func TestPerformUpdate_RejectsChecksumMismatch(t *testing.T) {
	origPrefixes := TrustedDownloadPrefixes
	defer func() { TrustedDownloadPrefixes = origPrefixes }()

	const assetName = "print-bridge-linux-amd64"
	mux := http.NewServeMux()
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is definitely not the real binary"))
	})
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  " + assetName + "\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	TrustedDownloadPrefixes = []string{srv.URL}

	info := &UpdateInfo{
		DownloadURL:   srv.URL + "/binary",
		AssetName:     assetName,
		LatestVersion: "v9.9.9",
		ChecksumsURL:  srv.URL + "/SHA256SUMS",
	}

	err := performUpdate(info)
	if err == nil {
		t.Fatal("expected performUpdate to reject a checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected a checksum mismatch error, got: %v", err)
	}
}

func TestPerformUpdate_RejectsUntrustedDownloadURL(t *testing.T) {
	info := &UpdateInfo{
		DownloadURL:  "https://evil.example.com/print-bridge",
		AssetName:    "print-bridge-linux-amd64",
		ChecksumsURL: "https://github.com/narbada-madhusudhan/nme-print-bridge/releases/download/v9.9.9/SHA256SUMS",
	}

	if err := performUpdate(info); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("expected untrusted download URL error, got: %v", err)
	}
}
