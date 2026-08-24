package permission

import (
	"testing"
	"unsafe"
)

// TestServerApproverAtomicAlignment guards the 32-bit-alignment fix for
// ServerApprover.counter (atomic.AddUint64). See memory/writer.go for the
// class of bug — a misaligned 64-bit atomic panics on 386/arm.
func TestServerApproverAtomicAlignment(t *testing.T) {
	var s ServerApprover
	if off := unsafe.Offsetof(s.counter); off%8 != 0 {
		t.Errorf("ServerApprover.counter at offset %d, must be 8-byte aligned", off)
	}
}
