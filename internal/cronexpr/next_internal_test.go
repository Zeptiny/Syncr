package cronexpr

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// tzif builds version 1 TZif data: a zone at offsets[0] that changes to
// offsets[i+1] at transitions[i].
func tzif(t *testing.T, transitions []time.Time, offsets []int32) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("TZif")
	b.Write(make([]byte, 16)) // version 1 and reserved
	be := func(v any) {
		if err := binary.Write(&b, binary.BigEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	chars := []byte("ZZZ\x00")
	// isutcnt, isstdcnt, leapcnt, timecnt, typecnt, charcnt.
	for _, n := range []int{0, 0, 0, len(transitions), len(offsets), len(chars)} {
		be(uint32(n))
	}
	for _, tr := range transitions {
		be(int32(tr.Unix()))
	}
	for i := range transitions {
		be(uint8(i + 1))
	}
	for _, off := range offsets {
		be(off)
		be(uint8(0)) // isdst
		be(uint8(0)) // designation index
	}
	b.Write(chars)
	return b.Bytes()
}

// TestResolveTwoTransitionsInOneWindow covers resolve's fallback: a zone
// that springs forward and falls back within three hours, so both ends of
// the search window have the same offset.
func TestResolveTwoTransitionsInOneWindow(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC) // 12:00 -> 13:00
	t2 := time.Date(2030, 1, 1, 15, 0, 0, 0, time.UTC) // 16:00 -> 15:00
	loc, err := time.LoadLocationFromTZData("Test/Weird", tzif(t, []time.Time{t1, t2}, []int32{0, 3600, 0}))
	if err != nil {
		t.Fatal(err)
	}
	e := &Expr{loc: loc}
	naive := func(h, m int) time.Time { return time.Date(2030, 1, 1, h, m, 0, 0, time.UTC) }
	tests := []struct {
		wall time.Time
		want time.Time
	}{
		{naive(10, 0), naive(10, 0)},   // before both
		{naive(12, 30), t1},            // in the gap: at the transition
		{naive(13, 0), t1},             // the first wall time after the gap
		{naive(14, 0), naive(13, 0)},   // +1 between the transitions
		{naive(15, 30), naive(14, 30)}, // repeated: the first pass
		{naive(16, 0), naive(16, 0)},   // after both
		{naive(15, 0).Add(-time.Minute), naive(13, 59)},
	}
	for _, tt := range tests {
		if got := e.resolve(tt.wall); !got.Equal(tt.want) {
			t.Errorf("resolve(%v) = %v, want %v", tt.wall, got.UTC(), tt.want)
		}
	}
}

func TestBitScans(t *testing.T) {
	t.Parallel()
	set := uint64(1)<<5 | 1<<17 | 1<<59 | starBit
	for _, tt := range []struct{ from, maxN, want int }{{0, 59, 5}, {5, 59, 5}, {6, 59, 17}, {18, 59, 59}, {18, 58, -1}, {60, 63, -1}} {
		if got := nextBit(set, tt.from, tt.maxN); got != tt.want {
			t.Errorf("nextBit(%d, %d) = %d, want %d", tt.from, tt.maxN, got, tt.want)
		}
	}
	for _, tt := range []struct{ from, want int }{{59, 59}, {58, 17}, {17, 17}, {16, 5}, {4, -1}, {0, -1}} {
		if got := prevBit(set, tt.from); got != tt.want {
			t.Errorf("prevBit(%d) = %d, want %d", tt.from, got, tt.want)
		}
	}
}
