package nativeproto

import "testing"

func TestDecodeReadDirResponseRejectsOversizedCount(t *testing.T) {
	var enc encoder
	enc.u32(0)     // DirTTLMS
	enc.u64(0)     // NextCookie
	enc.bool(true) // EOF
	enc.u32(10000) // count far larger than the remaining frame

	if _, err := DecodeReadDirResponse(enc.Bytes()); err == nil {
		t.Fatal("expected error for count exceeding frame size")
	}
}

func TestDecodeReadDirResponseEmpty(t *testing.T) {
	var enc encoder
	enc.u32(5)
	enc.u64(0)
	enc.bool(true)
	enc.u32(0)

	resp, err := DecodeReadDirResponse(enc.Bytes())
	if err != nil {
		t.Fatalf("empty readdir response: %v", err)
	}
	if !resp.EOF || len(resp.Entries) != 0 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
