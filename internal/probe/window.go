package probe

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TimeWindow is one scheduled probing period.
//
// DaysOfWeek uses 0=Sunday..6=Saturday; an empty list means every day
// (design doc 3.4).
type TimeWindow struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	DaysOfWeek []int  `json:"daysOfWeek"`
	StartTime  string `json:"startTime"` // "HH:MM"
	EndTime    string `json:"endTime"`   // "HH:MM"
	Enabled    bool   `json:"enabled"`
	SortOrder  int    `json:"sortOrder"`
}

// WindowStore persists window configuration. Implemented by
// storage.WindowStore.
type WindowStore interface {
	ListWindows(ctx context.Context) ([]TimeWindow, error)
	UpsertWindow(ctx context.Context, w TimeWindow) error
	DeleteWindow(ctx context.Context, id string) error
}

// Validate checks a window's shape.
func (w TimeWindow) Validate() error {
	if strings.TrimSpace(w.ID) == "" {
		return fmt.Errorf("probe: time window id is required")
	}
	if _, _, err := parseClock(w.StartTime); err != nil {
		return fmt.Errorf("probe: window %s start: %w", w.ID, err)
	}
	if _, _, err := parseClock(w.EndTime); err != nil {
		return fmt.Errorf("probe: window %s end: %w", w.ID, err)
	}
	for _, d := range w.DaysOfWeek {
		if d < 0 || d > 6 {
			return fmt.Errorf("probe: window %s has invalid day %d (want 0-6)", w.ID, d)
		}
	}
	return nil
}

// parseClock parses a strictly zero-padded "HH:MM" clock value.
//
// The padding is required, not cosmetic: containsTime compares these strings
// lexicographically, so "8:00" would sort after "10:00" and silently invert
// every window boundary.
func parseClock(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, fmt.Errorf("expected zero-padded HH:MM, got %q", s)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("expected hour 00-23, got %q", s)
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("expected minute 00-59, got %q", s)
	}
	return hour, minute, nil
}

// spansMidnight reports whether the window wraps past 00:00.
func (w TimeWindow) spansMidnight() bool { return w.StartTime > w.EndTime }

// containsTime reports whether the clock portion of `at` falls in the window,
// given that the day has already been matched.
func (w TimeWindow) containsTime(at time.Time) bool {
	now := at.Format("15:04")
	if !w.spansMidnight() {
		return w.StartTime <= now && now <= w.EndTime
	}
	// Cross-midnight: 22:00-06:00 covers [22:00, 24:00) and [00:00, 06:00].
	return now >= w.StartTime || now <= w.EndTime
}

// matchesDay reports whether `at`'s date is a day the window applies to.
//
// For a cross-midnight window the morning leg belongs to the *previous* day's
// window, so 06:30 on Tuesday is inside a Mon 22:00-06:00 window (design doc
// 3.4).
func (w TimeWindow) matchesDay(at time.Time) bool {
	if len(w.DaysOfWeek) == 0 {
		return true
	}
	if containsDay(w.DaysOfWeek, int(at.Weekday())) {
		return true
	}
	if w.spansMidnight() && at.Format("15:04") <= w.EndTime {
		prev := (int(at.Weekday()) + 6) % 7
		return containsDay(w.DaysOfWeek, prev)
	}
	return false
}

func containsDay(days []int, day int) bool {
	for _, d := range days {
		if d == day {
			return true
		}
	}
	return false
}

// Matches reports whether `at` falls inside this window, considering both the
// day set and the time-of-day range.
func (w TimeWindow) Matches(at time.Time) bool {
	if !w.Enabled {
		return false
	}
	return w.matchesDay(at) && w.containsTime(at)
}

// ShouldProbeNow reports whether any enabled window admits `at`.
//
// With no enabled windows configured, probing is unrestricted -- this keeps a
// fresh install working before anyone configures a schedule (design doc 3.4).
func ShouldProbeNow(windows []TimeWindow, at time.Time) bool {
	anyEnabled := false
	for _, w := range windows {
		if !w.Enabled {
			continue
		}
		anyEnabled = true
		if w.Matches(at) {
			return true
		}
	}
	return !anyEnabled
}

// WindowManager owns the window list and answers the scheduler's
// "may we probe right now?" question.
type WindowManager struct {
	store WindowStore

	mu      sync.RWMutex
	windows []TimeWindow
}

// NewWindowManager builds an empty manager. Call Load to populate it.
func NewWindowManager(store WindowStore) *WindowManager {
	return &WindowManager{store: store}
}

// Load restores windows from the store.
func (m *WindowManager) Load(ctx context.Context) error {
	windows, err := m.store.ListWindows(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.windows = windows
	m.mu.Unlock()
	return nil
}

// All returns the configured windows.
func (m *WindowManager) All() []TimeWindow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TimeWindow, len(m.windows))
	copy(out, m.windows)
	return out
}

// ShouldProbeNow evaluates the current configuration against `at`.
func (m *WindowManager) ShouldProbeNow(at time.Time) bool {
	return ShouldProbeNow(m.All(), at)
}

// Upsert validates and persists a window, then refreshes the in-memory list.
func (m *WindowManager) Upsert(ctx context.Context, w TimeWindow) error {
	if err := w.Validate(); err != nil {
		return err
	}
	if err := m.store.UpsertWindow(ctx, w); err != nil {
		return err
	}
	return m.reload(ctx)
}

// Delete removes a window.
func (m *WindowManager) Delete(ctx context.Context, id string) error {
	if err := m.store.DeleteWindow(ctx, id); err != nil {
		return err
	}
	return m.reload(ctx)
}

// ReplaceAll swaps the whole window list, used by the bulk save action.
func (m *WindowManager) ReplaceAll(ctx context.Context, windows []TimeWindow) error {
	for _, w := range windows {
		if err := w.Validate(); err != nil {
			return err
		}
	}
	existing := m.All()
	keep := make(map[string]bool, len(windows))
	for _, w := range windows {
		keep[w.ID] = true
		if err := m.store.UpsertWindow(ctx, w); err != nil {
			return err
		}
	}
	for _, old := range existing {
		if keep[old.ID] {
			continue
		}
		if err := m.store.DeleteWindow(ctx, old.ID); err != nil {
			return err
		}
	}
	return m.reload(ctx)
}

func (m *WindowManager) reload(ctx context.Context) error { return m.Load(ctx) }

// SortWindows orders windows by SortOrder then start time, for display.
func SortWindows(windows []TimeWindow) []TimeWindow {
	out := make([]TimeWindow, len(windows))
	copy(out, windows)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].StartTime < out[j].StartTime
	})
	return out
}
