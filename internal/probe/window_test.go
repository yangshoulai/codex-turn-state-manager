package probe

import (
	"context"
	"testing"
	"time"
)

// at builds a time on a known week. 2026-09-14 is a Monday.
func at(t *testing.T, day time.Weekday, clock string) time.Time {
	t.Helper()
	hour, minute, err := parseClock(clock)
	if err != nil {
		t.Fatalf("bad clock %q: %v", clock, err)
	}
	// 2026-09-13 is a Sunday, so index by weekday directly.
	base := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	return base.AddDate(0, 0, int(day)).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

func TestShouldProbeNow_NoWindowsMeansAlways(t *testing.T) {
	if !ShouldProbeNow(nil, at(t, time.Monday, "03:00")) {
		t.Error("no windows configured should permit probing")
	}
	if !ShouldProbeNow([]TimeWindow{}, at(t, time.Sunday, "23:59")) {
		t.Error("empty window list should permit probing")
	}
}

func TestShouldProbeNow_AllWindowsDisabledMeansAlways(t *testing.T) {
	windows := []TimeWindow{{
		ID: "w1", StartTime: "08:00", EndTime: "20:00", Enabled: false,
	}}
	if !ShouldProbeNow(windows, at(t, time.Monday, "03:00")) {
		t.Error("a disabled window is not a restriction; probing should be permitted")
	}
}

func TestWindow_SameDay(t *testing.T) {
	// Weekdays 08:00-20:00.
	w := TimeWindow{
		ID: "work", DaysOfWeek: []int{1, 2, 3, 4, 5},
		StartTime: "08:00", EndTime: "20:00", Enabled: true,
	}

	cases := []struct {
		name string
		day  time.Weekday
		at   string
		want bool
	}{
		{"inside on a weekday", time.Monday, "10:00", true},
		{"inside on friday", time.Friday, "19:59", true},
		{"start boundary is inclusive", time.Monday, "08:00", true},
		{"end boundary is inclusive", time.Monday, "20:00", true},
		{"just before start", time.Monday, "07:59", false},
		{"just after end", time.Monday, "20:01", false},
		{"weekend excluded", time.Saturday, "10:00", false},
		{"sunday excluded", time.Sunday, "10:00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Matches(at(t, tc.day, tc.at)); got != tc.want {
				t.Errorf("Matches(%s %s) = %v, want %v", tc.day, tc.at, got, tc.want)
			}
		})
	}
}

func TestWindow_EmptyDaysMeansEveryDay(t *testing.T) {
	w := TimeWindow{ID: "always", StartTime: "08:00", EndTime: "20:00", Enabled: true}
	for day := time.Sunday; day <= time.Saturday; day++ {
		if !w.Matches(at(t, day, "12:00")) {
			t.Errorf("%s 12:00 should match an every-day window", day)
		}
	}
}

// TestWindow_CrossMidnight covers the most bug-prone rule in the design: a
// window such as 22:00-06:00 spans two calendar days, and the morning leg
// belongs to the previous day's window.
func TestWindow_CrossMidnight(t *testing.T) {
	t.Run("every day", func(t *testing.T) {
		w := TimeWindow{ID: "night", StartTime: "22:00", EndTime: "06:00", Enabled: true}
		cases := []struct {
			day  time.Weekday
			at   string
			want bool
		}{
			{time.Monday, "22:00", true},  // start inclusive
			{time.Monday, "23:59", true},  // evening leg
			{time.Tuesday, "00:00", true}, // just past midnight
			{time.Tuesday, "05:59", true}, // early morning leg
			{time.Tuesday, "06:00", true}, // end inclusive
			{time.Tuesday, "06:01", false},
			{time.Tuesday, "21:59", false},
			{time.Tuesday, "12:00", false},
		}
		for _, tc := range cases {
			if got := w.Matches(at(t, tc.day, tc.at)); got != tc.want {
				t.Errorf("Matches(%s %s) = %v, want %v", tc.day, tc.at, got, tc.want)
			}
		}
	})

	t.Run("monday only", func(t *testing.T) {
		// Monday 22:00 through Tuesday 06:00.
		w := TimeWindow{
			ID: "mon-night", DaysOfWeek: []int{1},
			StartTime: "22:00", EndTime: "06:00", Enabled: true,
		}
		cases := []struct {
			name string
			day  time.Weekday
			at   string
			want bool
		}{
			{"monday evening", time.Monday, "23:00", true},
			{"tuesday early morning belongs to monday's window", time.Tuesday, "05:00", true},
			{"tuesday just after the window ends", time.Tuesday, "06:30", false},
			{"monday before the window opens", time.Monday, "21:00", false},
			{"sunday evening is not monday", time.Sunday, "23:00", false},
			{"wednesday morning is not monday", time.Wednesday, "05:00", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := w.Matches(at(t, tc.day, tc.at)); got != tc.want {
					t.Errorf("Matches(%s %s) = %v, want %v", tc.day, tc.at, got, tc.want)
				}
			})
		}
	})
}

func TestShouldProbeNow_MultipleWindows(t *testing.T) {
	windows := []TimeWindow{
		{ID: "morning", StartTime: "08:00", EndTime: "12:00", Enabled: true},
		{ID: "afternoon", StartTime: "14:00", EndTime: "20:00", Enabled: true},
	}
	cases := []struct {
		at   string
		want bool
	}{
		{"08:00", true},
		{"11:59", true},
		{"12:30", false}, // the lunch gap
		{"13:59", false},
		{"14:00", true},
		{"20:00", true},
		{"20:01", false},
		{"07:00", false},
	}
	for _, tc := range cases {
		if got := ShouldProbeNow(windows, at(t, time.Wednesday, tc.at)); got != tc.want {
			t.Errorf("ShouldProbeNow(%s) = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestShouldProbeNow_DisabledWindowIsIgnored(t *testing.T) {
	windows := []TimeWindow{
		{ID: "off", StartTime: "08:00", EndTime: "20:00", Enabled: false},
		{ID: "on", StartTime: "22:00", EndTime: "23:00", Enabled: true},
	}
	if ShouldProbeNow(windows, at(t, time.Monday, "12:00")) {
		t.Error("a disabled window must not admit a probe")
	}
	if !ShouldProbeNow(windows, at(t, time.Monday, "22:30")) {
		t.Error("an enabled window should admit a probe")
	}
}

func TestWindow_Validate(t *testing.T) {
	valid := TimeWindow{ID: "a", StartTime: "08:00", EndTime: "20:00", DaysOfWeek: []int{0, 6}}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid window rejected: %v", err)
	}

	bad := []TimeWindow{
		{ID: "", StartTime: "08:00", EndTime: "20:00"},
		{ID: "a", StartTime: "8:00", EndTime: "20:00"},
		{ID: "a", StartTime: "24:00", EndTime: "20:00"},
		{ID: "a", StartTime: "08:00", EndTime: "20:70"},
		{ID: "a", StartTime: "08:00", EndTime: "20:00", DaysOfWeek: []int{7}},
	}
	for i, w := range bad {
		if err := w.Validate(); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, w)
		}
	}
}

func TestWindowManager_ShouldProbeNow(t *testing.T) {
	store := newFakeWindowStore()
	m := NewWindowManager(store)
	if err := m.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !m.ShouldProbeNow(at(t, time.Monday, "03:00")) {
		t.Error("no windows loaded should permit probing")
	}

	if err := m.Upsert(t.Context(), TimeWindow{
		ID: "work", Label: "work", StartTime: "08:00", EndTime: "20:00", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if m.ShouldProbeNow(at(t, time.Monday, "03:00")) {
		t.Error("outside the configured window probing should be blocked")
	}
	if !m.ShouldProbeNow(at(t, time.Monday, "09:00")) {
		t.Error("inside the configured window probing should be allowed")
	}
}

type fakeWindowStore struct{ windows []TimeWindow }

func newFakeWindowStore() *fakeWindowStore { return &fakeWindowStore{} }

func (s *fakeWindowStore) ListWindows(context.Context) ([]TimeWindow, error) {
	out := make([]TimeWindow, len(s.windows))
	copy(out, s.windows)
	return out, nil
}

func (s *fakeWindowStore) UpsertWindow(_ context.Context, w TimeWindow) error {
	for i, existing := range s.windows {
		if existing.ID == w.ID {
			s.windows[i] = w
			return nil
		}
	}
	s.windows = append(s.windows, w)
	return nil
}

func (s *fakeWindowStore) DeleteWindow(_ context.Context, id string) error {
	for i, existing := range s.windows {
		if existing.ID == id {
			s.windows = append(s.windows[:i], s.windows[i+1:]...)
			return nil
		}
	}
	return nil
}
