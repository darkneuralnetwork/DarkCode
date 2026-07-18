package memory

import (
	"testing"
	"unsafe"
)

// TestDebouncedWriterAtomicAlignment guards the 32-bit-alignment fix: the
// 64-bit atomic counters must sit at an 8-byte-aligned offset, or
// atomic.AddInt64/LoadInt64 panic on 386/arm (the real crash this fixed was
// at memory/writer.go flush() on windows/386). Keeping them first makes the
// offset 0; this test fails loudly if a future edit reorders them.
func TestDebouncedWriterAtomicAlignment(t *testing.T) {
	var w DebouncedWriter
	if off := unsafe.Offsetof(w.writes); off%8 != 0 {
		t.Errorf("DebouncedWriter.writes at offset %d, must be 8-byte aligned for 32-bit atomics", off)
	}
	if off := unsafe.Offsetof(w.errors); off%8 != 0 {
		t.Errorf("DebouncedWriter.errors at offset %d, must be 8-byte aligned for 32-bit atomics", off)
	}
}
