package attach

import (
	"strings"
)

// ParseRefs scans a prompt for @Type:ref tokens and splits it into the
// remaining query text plus the referenced attachments. This is the syntax
// used by the CLI console (and optionally the GUI): the user types things
// like
//
//	@File:./report.pdf summarize this
//	@Directory:./src review the structure
//	@URL:https://example.com/news what changed
//	@Text:"a literal note" keep this in mind
//	@Image:photo.png describe it
//
// Tokens are case-insensitive on the type and accept the aliases in
// KnownTypes (dir/folder, img, link, note, …). An @ that is not followed by
// a known type and a colon is left untouched in the query. The original
// token is removed from the returned query; surrounding whitespace is
// collapsed so "summarize  @File:x  this" -> "summarize this".
func ParseRefs(input string) (query string, attachments []Attachment) {
	var q strings.Builder
	i := 0
	for i < len(input) {
		ch := input[i]
		if ch != '@' {
			q.WriteByte(ch)
			i++
			continue
		}
		// Try to parse an @Type:ref token starting here.
		att, end, ok := tryParseRef(input, i)
		if !ok {
			// Not a recognised reference — keep the '@' literal.
			q.WriteByte(ch)
			i++
			continue
		}
		attachments = append(attachments, att)
		i = end
	}
	query = collapseSpaces(q.String())
	return query, attachments
}

// tryParseRef attempts to read an @Type:ref token beginning at index `at`.
// On success returns the attachment, the index just past the token, and true.
func tryParseRef(s string, at int) (Attachment, int, bool) {
	// s[at] == '@'
	i := at + 1
	// Read the type name (letters only).
	start := i
	for i < len(s) && isAlpha(s[i]) {
		i++
	}
	if i == start || i >= len(s) || s[i] != ':' {
		return Attachment{}, at, false
	}
	rawType := strings.ToLower(s[start:i])
	canon, ok := KnownTypes[rawType]
	if !ok {
		return Attachment{}, at, false
	}
	t := strings.ToLower(canon)
	// Skip the colon.
	i++
	// Read the value.
	var val strings.Builder
	if i < len(s) && s[i] == '"' {
		// Quoted value: read until the closing quote.
		i++ // skip opening quote
		for i < len(s) && s[i] != '"' {
			val.WriteByte(s[i])
			i++
		}
		if i < len(s) && s[i] == '"' {
			i++ // skip closing quote
		}
	} else {
		// Unquoted value: read until whitespace.
		for i < len(s) && !isSpace(s[i]) {
			val.WriteByte(s[i])
			i++
		}
	}
	a := Attachment{Type: t}
	switch t {
	case TypeURL:
		a.Content = val.String()
		if a.Content == "" {
			a.Path = val.String()
		}
	case TypeText:
		a.Content = val.String()
	default:
		a.Path = val.String()
	}
	if a.Path == "" && a.Content == "" {
		return Attachment{}, at, false
	}
	return a, i, true
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// collapseSpaces turns runs of whitespace into single spaces and trims the
// ends — so removing an @token doesn't leave a double gap.
func collapseSpaces(s string) string {
	var b strings.Builder
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isSpace(c) {
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		b.WriteByte(c)
		prevSpace = false
	}
	return strings.TrimRight(b.String(), " ")
}
