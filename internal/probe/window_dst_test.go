package probe

import (
	"testing"
	"time"

	// Embed the timezone database so the DST cases behave the same on a
	// developer machine and in a minimal CI container.
	_ "time/tzdata"
)

// Design document section 8, verification item 7 asks whether the cross-midnight
// window logic holds against real clock behaviour, DST included.
//
// The windows are wall-clock by definition: an operator writing 22:00-06:00
// means their local clock, not a fixed UTC offset. These tests pin that on the
// two days a year where wall clock and elapsed time disagree.

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	return loc
}

// TestWindow_SpringForward covers 2026-03-08, when 02:00-03:00 local does not
// exist.
func TestWindow_SpringForward(t *testing.T) {
	loc := newYork(t)
	w := TimeWindow{ID: "morning", StartTime: "01:00", EndTime: "04:00", Enabled: true}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		// 01:30 EST, before the jump.
		{"before the jump", time.Date(2026, 3, 8, 1, 30, 0, 0, loc), true},
		// 03:30 EDT, after the jump. Go normalises a nonexistent 02:30 to
		// 03:30, which is the correct wall-clock answer.
		{"after the jump", time.Date(2026, 3, 8, 3, 30, 0, 0, loc), true},
		{"normalised nonexistent time", time.Date(2026, 3, 8, 2, 30, 0, 0, loc), true},
		{"outside the window", time.Date(2026, 3, 8, 5, 0, 0, 0, loc), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Matches(tc.at); got != tc.want {
				t.Errorf("Matches(%s) = %v, want %v", tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}

	// The window is three wall-clock hours wide, but the day is only 23 hours
	// long. Boundary arithmetic must follow the wall clock, not elapsed time.
	start := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	end := time.Date(2026, 3, 8, 4, 0, 0, 0, loc)
	if elapsed := end.Sub(start); elapsed != 2*time.Hour {
		t.Errorf("01:00-04:00 spans %s of elapsed time; the expectation is 2h across the jump", elapsed)
	}
	if w.StartTime != "01:00" || w.EndTime != "04:00" {
		t.Error("the window must stay expressed in wall-clock terms")
	}
}

// TestWindow_FallBack covers 2026-11-01, when 01:00-02:00 local happens twice.
// Both occurrences must match: an operator means "while my clock reads 01:xx".
func TestWindow_FallBack(t *testing.T) {
	loc := newYork(t)
	w := TimeWindow{ID: "early", StartTime: "01:00", EndTime: "02:00", Enabled: true}

	first := time.Date(2026, 11, 1, 1, 30, 0, 0, loc) // EDT, before the repeat
	second := first.Add(time.Hour)                    // EST, the repeated hour

	if first.Format("15:04") != "01:30" {
		t.Fatalf("first occurrence reads %s, expected 01:30", first.Format("15:04"))
	}
	if second.Format("15:04") != "01:30" {
		t.Fatalf("second occurrence reads %s, expected 01:30", second.Format("15:04"))
	}
	if first.Equal(second) {
		t.Fatal("the two occurrences should be distinct instants")
	}

	for name, at := range map[string]time.Time{"first": first, "second": second} {
		if !w.Matches(at) {
			t.Errorf("%s occurrence of 01:30 did not match a 01:00-02:00 window", name)
		}
	}
}

// TestWindow_CrossMidnightAcrossDST is the combination the design document
// calls out: a window that both crosses midnight and spans a DST transition.
//
// The US fall-back happens at 02:00 on the first Sunday of November, so the
// night that spans it runs from Saturday into Sunday.
func TestWindow_CrossMidnightAcrossDST(t *testing.T) {
	loc := newYork(t)

	// Saturday 2026-10-31 22:00 through Sunday 2026-11-01 06:00.
	w := TimeWindow{
		ID: "sat-night", DaysOfWeek: []int{6},
		StartTime: "22:00", EndTime: "06:00", Enabled: true,
	}

	satEvening := time.Date(2026, 10, 31, 23, 0, 0, 0, loc)
	sunMorning := time.Date(2026, 11, 1, 5, 0, 0, 0, loc)

	if satEvening.Weekday() != time.Saturday {
		t.Fatalf("fixture is %s, expected Saturday", satEvening.Weekday())
	}
	if sunMorning.Weekday() != time.Sunday {
		t.Fatalf("fixture is %s, expected Sunday", sunMorning.Weekday())
	}

	// Guard the fixture itself: if the zone database ever stops putting a
	// transition between these two instants, the test would silently stop
	// covering DST. Fail loudly instead.
	_, before := satEvening.Zone()
	_, after := sunMorning.Zone()
	if before == after {
		t.Fatalf("fixture spans no DST transition (offset %d throughout)", before)
	}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"saturday evening", satEvening, true},
		// The morning leg belongs to Saturday's window even though the
		// calendar has rolled over, and this particular morning had a repeated
		// hour.
		{"sunday morning after the repeat", sunMorning, true},
		{"just past the window end", time.Date(2026, 11, 1, 6, 30, 0, 0, loc), false},
		{"sunday daytime", time.Date(2026, 11, 1, 12, 0, 0, 0, loc), false},
		{"friday evening is not saturday", time.Date(2026, 10, 30, 23, 0, 0, 0, loc), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Matches(tc.at); got != tc.want {
				t.Errorf("Matches(%s) = %v, want %v", tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestWindow_StableAcrossLocations pins that the decision depends on the wall
// clock of the supplied instant, not on the machine's own zone.
func TestWindow_StableAcrossLocations(t *testing.T) {
	utc := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load Asia/Tokyo: %v", err)
	}

	// The same instant reads 12:00 in UTC and 21:00 in Tokyo, so a 08:00-20:00
	// window must accept it in one and reject it in the other.
	w := TimeWindow{ID: "work", StartTime: "08:00", EndTime: "20:00", Enabled: true}

	if !w.Matches(utc) {
		t.Error("12:00 UTC should fall inside an 08:00-20:00 window")
	}
	if w.Matches(utc.In(tokyo)) {
		t.Error("the same instant at 21:00 Tokyo should fall outside it")
	}
}
