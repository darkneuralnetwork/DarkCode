package server

import (
	"net/http/httptest"
	"testing"
)

func TestPaginate(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	// No limit -> everything, has_more false.
	page, meta := paginate(items, pageParams{})
	if len(page) != 10 || meta["has_more"] != false || meta["total"] != 10 {
		t.Fatalf("no-limit page: got len=%d meta=%v", len(page), meta)
	}

	// limit=3, offset=0 -> first 3, has_more true.
	page, meta = paginate(items, pageParams{limit: 3, offset: 0})
	if len(page) != 3 || page[0] != 0 || page[2] != 2 || meta["has_more"] != true {
		t.Fatalf("first page: got %v meta=%v", page, meta)
	}

	// offset near the end clamps and reports the remainder.
	page, meta = paginate(items, pageParams{limit: 5, offset: 8})
	if len(page) != 2 || page[0] != 8 || meta["has_more"] != false {
		t.Fatalf("tail page: got %v meta=%v", page, meta)
	}

	// offset past the end -> empty page, no panic.
	page, meta = paginate(items, pageParams{limit: 3, offset: 100})
	if len(page) != 0 || meta["has_more"] != false {
		t.Fatalf("out-of-range offset: got %v meta=%v", page, meta)
	}
}

func TestParsePageClampsNegatives(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/memory/episodic?limit=-5&offset=-2", nil)
	p := parsePage(r)
	if p.limit != 0 || p.offset != 0 {
		t.Fatalf("negative params should clamp to 0, got %+v", p)
	}
}
