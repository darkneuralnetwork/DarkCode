package server

import (
	"net/http"
	"strconv"
)

// Pagination for the list endpoints whose payload grows with usage
// (episodic/semantic/procedural memory, knowledge, audit). Opt-in and
// backward-compatible: with no query params the full list is returned, so
// existing callers are unaffected. Clients that want a page pass ?limit= and
// optional ?offset=.

type pageParams struct {
	limit  int // 0 = no limit (return everything from offset on)
	offset int
}

func parsePage(r *http.Request) pageParams {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit < 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}
	return pageParams{limit: limit, offset: offset}
}

// paginate slices items by the page params and returns the page plus metadata
// (total, offset, limit, count, has_more) for the JSON response.
func paginate[T any](items []T, p pageParams) ([]T, map[string]interface{}) {
	total := len(items)
	start := p.offset
	if start > total {
		start = total
	}
	end := total
	if p.limit > 0 && start+p.limit < end {
		end = start + p.limit
	}
	page := items[start:end]
	meta := map[string]interface{}{
		"total":    total,
		"offset":   start,
		"limit":    p.limit,
		"count":    len(page),
		"has_more": end < total,
	}
	return page, meta
}

// writePage writes a paginated list response: the items under listKey plus the
// pagination metadata fields merged in at the top level.
func writePage[T any](w http.ResponseWriter, listKey string, items []T, p pageParams) {
	page, meta := paginate(items, p)
	meta[listKey] = page
	writeJSON(w, http.StatusOK, meta)
}
