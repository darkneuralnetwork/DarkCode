package tools

import (
	"testing"
	"unsafe"
)

// TestMCPClientAtomicAlignment guards the 32-bit-alignment fix for the MCP
// clients' atomically-incremented request-id counters (see memory/writer.go
// for the class of bug). They must be 8-byte aligned or atomic.AddInt64
// panics on 386/arm.
func TestMCPClientAtomicAlignment(t *testing.T) {
	var s StdioMCPClient
	if off := unsafe.Offsetof(s.nextID); off%8 != 0 {
		t.Errorf("StdioMCPClient.nextID at offset %d, must be 8-byte aligned", off)
	}
	var h HTTPMCPClient
	if off := unsafe.Offsetof(h.nextID); off%8 != 0 {
		t.Errorf("HTTPMCPClient.nextID at offset %d, must be 8-byte aligned", off)
	}
}
