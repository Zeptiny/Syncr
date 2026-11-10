package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNewIDsAreUniqueAndValid(t *testing.T) {
	t.Parallel()
	a, b := NewJobID(), NewJobID()
	if a.IsZero() || b.IsZero() {
		t.Fatal("new ID is zero")
	}
	if a == b {
		t.Fatal("two new IDs are equal")
	}
	parsed, err := ParseJobID(a.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != a {
		t.Errorf("round trip: got %v, want %v", parsed, a)
	}
	if v := a.UUID()[6] >> 4; v != 7 {
		t.Errorf("UUID version = %d, want 7", v)
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		wantErr bool
	}{
		{in: "0195f0a4-3b7c-7d1e-9f00-123456789abc"},
		{in: "0195F0A4-3B7C-7D1E-9F00-123456789ABC"},
		{in: "", wantErr: true},
		{in: "not-a-uuid", wantErr: true},
		{in: "00000000-0000-0000-0000-000000000000", wantErr: true},
		{in: "0195f0a4-3b7c-7d1e-9f00-123456789ab", wantErr: true},
	}
	for _, tt := range tests {
		_, err := ParseWorkerID(tt.in)
		if tt.wantErr {
			if !errors.Is(err, ErrInvalidID) {
				t.Errorf("ParseWorkerID(%q) err = %v, want ErrInvalidID", tt.in, err)
			}
		} else if err != nil {
			t.Errorf("ParseWorkerID(%q) err = %v", tt.in, err)
		}
	}
}

func TestIDText(t *testing.T) {
	t.Parallel()
	type payload struct {
		Job    JobID    `json:"job"`
		Worker WorkerID `json:"worker"`
	}
	in := payload{Job: NewJobID()}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out payload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("JSON round trip: got %+v, want %+v", out, in)
	}
	if !out.Worker.IsZero() {
		t.Error("zero ID did not round-trip as zero")
	}

	var id UserID
	if err := id.UnmarshalText([]byte("garbage")); !errors.Is(err, ErrInvalidID) {
		t.Errorf("UnmarshalText(garbage) = %v, want ErrInvalidID", err)
	}
}

func TestM4IDs(t *testing.T) {
	t.Parallel()
	c, r, d := NewContactID(), NewScheduleRunID(), NewDeliveryID()
	if c.IsZero() || r.IsZero() || d.IsZero() {
		t.Fatal("new ID is zero")
	}
	if got, err := ParseContactID(c.String()); err != nil || got != c {
		t.Errorf("ParseContactID = (%v, %v)", got, err)
	}
	if got, err := ParseScheduleRunID(r.String()); err != nil || got != r {
		t.Errorf("ParseScheduleRunID = (%v, %v)", got, err)
	}
	if got, err := ParseDeliveryID(d.String()); err != nil || got != d {
		t.Errorf("ParseDeliveryID = (%v, %v)", got, err)
	}
	if _, err := ParseDeliveryID("x"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ParseDeliveryID(x) = %v", err)
	}
}

func TestProbeID(t *testing.T) {
	t.Parallel()
	a := NewProbeID()
	if a.IsZero() || a == NewProbeID() {
		t.Fatal("new probe IDs must be non-zero and distinct")
	}
	got, err := ParseProbeID(a.String())
	if err != nil || got != a {
		t.Errorf("ParseProbeID(%s) = (%v, %v)", a, got, err)
	}
	if _, err := ParseProbeID("00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ParseProbeID(nil UUID) = %v, want ErrInvalidID", err)
	}
}
