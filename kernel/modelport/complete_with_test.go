package modelport

// complete_with_test.go — CompleteWith lets a caller supply the client
// directly instead of having Manager route to one. It must still apply the
// same policy (ceiling/temperature), window-fit, and dispatch machinery
// Complete gives routed callers.

import (
	"context"
	"testing"
)

func TestCompleteWithAppliesThePurposePolicy(t *testing.T) {
	c := &fakeClient{reply: "ok"}
	m, err := New(&fakeRouter{client: c}) // router is never consulted by CompleteWith
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.CompleteWith(context.Background(), c, "explicit-model", Ask{Purpose: PurposeCompress, Messages: msgs}); err != nil {
		t.Fatalf("CompleteWith: %v", err)
	}
	if c.got == nil {
		t.Fatal("client never received a request")
	}
	_, wantMaxTok, wantTemp := PolicyFor(PurposeCompress)
	if c.got.MaxTokens == nil || *c.got.MaxTokens != wantMaxTok {
		t.Errorf("MaxTokens = %v, want %d", c.got.MaxTokens, wantMaxTok)
	}
	if c.got.Temperature == nil || *c.got.Temperature != wantTemp {
		t.Errorf("Temperature = %v, want %f", c.got.Temperature, wantTemp)
	}
	if c.got.Model != "explicit-model" {
		t.Errorf("Model = %q, want the caller-supplied model, not one Manager routed to", c.got.Model)
	}
}

func TestCompleteWithNeverConsultsTheRouter(t *testing.T) {
	c := &fakeClient{reply: "ok"}
	r := &fakeRouter{client: c, routeErr: nil}
	m, err := New(r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	other := &fakeClient{reply: "ok"}
	if _, err := m.CompleteWith(context.Background(), other, "m", Ask{Purpose: PurposeExecute, Messages: msgs}); err != nil {
		t.Fatalf("CompleteWith: %v", err)
	}
	if r.gotTier != "" {
		t.Fatalf("router.Route was called (gotTier=%q) — CompleteWith must not route", r.gotTier)
	}
	if other.got == nil {
		t.Fatal("the explicitly-supplied client never received the request")
	}
}

func TestCompleteWithNilClientIsRefused(t *testing.T) {
	m, err := New(&fakeRouter{client: &fakeClient{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.CompleteWith(context.Background(), nil, "m", Ask{Purpose: PurposeExecute, Messages: msgs}); err == nil {
		t.Fatal("expected an error for a nil client")
	}
}

func TestCompleteWithAskOverridesStillWin(t *testing.T) {
	c := &fakeClient{reply: "ok"}
	m, err := New(&fakeRouter{client: c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	temp := 0.2
	if _, err := m.CompleteWith(context.Background(), c, "m", Ask{
		Purpose: PurposePlan, Messages: msgs, MaxTokens: 777, Temperature: &temp,
	}); err != nil {
		t.Fatalf("CompleteWith: %v", err)
	}
	if c.got.MaxTokens == nil || *c.got.MaxTokens != 777 {
		t.Errorf("MaxTokens = %v, want 777 (explicit override)", c.got.MaxTokens)
	}
	if c.got.Temperature == nil || *c.got.Temperature != 0.2 {
		t.Errorf("Temperature = %v, want 0.2 (explicit override)", c.got.Temperature)
	}
}
