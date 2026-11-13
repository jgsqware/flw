package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		tag, cur string
		want     bool
	}{
		{"v0.2.0", "v0.1.9", true},
		{"v0.2.0", "0.2.0", false},
		{"v1.0.0", "v0.99.99", true},
		{"v0.1.0", "v0.2.0", false},
		{"v0.2.0", "dev", false}, // dev builds are not nagged
		{"v0.2.0", "v0.0.0-20261113-abcdef", false},
	}
	for _, c := range cases {
		if got := newer(c.tag, c.cur); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.tag, c.cur, got, c.want)
		}
	}
}

func TestExpectedSum(t *testing.T) {
	sums := []byte("abc  flw_linux_amd64.tar.gz\ndef  flw_darwin_arm64.tar.gz\n")
	if got := expectedSum(sums, "flw_darwin_arm64.tar.gz"); got != "def" {
		t.Fatalf("got %q", got)
	}
	if got := expectedSum(sums, "flw_windows_amd64.tar.gz"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestBinaryFromTarGz(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"README.md": "doc", "flw": "BIN"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	b, err := binaryFromTarGz(buf.Bytes())
	if err != nil || string(b) != "BIN" {
		t.Fatalf("got %q, %v", b, err)
	}
}
