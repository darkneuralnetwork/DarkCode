package plugin

import (
	"testing"
	"unsafe"
)

// TestHostAtomicAlignment guards the 32-bit-alignment fix for Host.nextID
// (atomic.AddInt64). See memory/writer.go for the class of bug — a misaligned
// 64-bit atomic panics on 386/arm.
func TestHostAtomicAlignment(t *testing.T) {
	var h Host
	if off := unsafe.Offsetof(h.nextID); off%8 != 0 {
		t.Errorf("Host.nextID at offset %d, must be 8-byte aligned", off)
	}
}
