package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed standard 5-field cron expression, evaluated in UTC:
// minute hour day-of-month month day-of-week. It is a hand-written parser
// on purpose — the dependency axis rules out a cron library for what is
// ~100 lines of field parsing and a Next walk.
type Cron struct {
	minutes [60]bool
	hours   [24]bool
	doms    [32]bool // indexed by day-of-month, 1..31
	months  [13]bool // indexed by time.Month(), 1..12
	dows    [7]bool  // indexed by time.Weekday(), 0=Sunday
	domAll  bool     // dom field is a plain *
	dowAll  bool     // dow field is a plain *
}

// MaxCronLookahead bounds Next's search: four years is the smallest window
// that guarantees every valid expression finds a match (Feb 29 recurs on a
// 4-year cycle). An expression with no match inside it can never fire.
const MaxCronLookahead = 4 * 366 * 24 * time.Hour

// ParseCron parses a 5-field cron expression. Syntax per field: `*`,
// single values, ranges (`a-b`), steps (`*/n`, `a-b/n`, and the widely
// used `a/n` meaning a..max by n), comma lists. dow accepts 0-7 with 7
// normalized to Sunday. No month names, no @macros, no seconds fields.
func ParseCron(expr string) (*Cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("%q: want 5 fields (minute hour dom month dow), got %d", expr, len(fields))
	}
	c := &Cron{}
	if err := c.parseField(fields[0], 0, 59, func(i int) { c.minutes[i] = true }, nil, "minute"); err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	if err := c.parseField(fields[1], 0, 23, func(i int) { c.hours[i] = true }, nil, "hour"); err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	if err := c.parseField(fields[2], 1, 31, func(i int) { c.doms[i] = true }, &c.domAll, "day-of-month"); err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	if err := c.parseField(fields[3], 1, 12, func(i int) { c.months[i] = true }, nil, "month"); err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	if err := c.parseField(fields[4], 0, 7, func(i int) { c.dows[i%7] = true }, &c.dowAll, "day-of-week"); err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	return c, nil
}

// parseField parses one comma-separated cron field into set, recording in
// all whether the field was a plain `*` — only the dom/dow fields use it
// (it decides their OR rule), so other fields pass nil.
func (c *Cron) parseField(s string, lo, hi int, set func(i int), all *bool, name string) error {
	for i, item := range strings.Split(s, ",") {
		if err := c.parseItem(item, lo, hi, set, all, name); err != nil {
			return fmt.Errorf("%s: item %d (%q): %w", name, i, item, err)
		}
	}
	return nil
}

func (c *Cron) parseItem(item string, lo, hi int, set func(i int), all *bool, name string) error {
	step := 1
	hasStep := false
	body := item
	if head, stepStr, has := strings.Cut(item, "/"); has {
		body = head
		hasStep = true
		n, err := strconv.Atoi(stepStr)
		if err != nil || n < 1 {
			return fmt.Errorf("step %q: must be a positive integer", stepStr)
		}
		step = n
		// A step wider than the field range selects only the first value,
		// so clamping cannot change the match set — but it keeps the fire
		// walk's increment from overflowing int on e.g. 1/9223372036854775807,
		// which would wrap negative and index outside the field array.
		if step > hi-lo {
			step = hi - lo + 1
		}
	}
	if body == "" {
		return fmt.Errorf("empty value")
	}
	first, last := lo, hi
	switch {
	case body == "*":
		if all != nil {
			*all = step == 1 // "*" alone is unrestricted; "*/n" still restricts
		}
	case strings.Contains(body, "-"):
		a, b, ok := strings.Cut(body, "-")
		if !ok {
			return fmt.Errorf("range %q: want lo-hi", body)
		}
		var err error
		first, err = cronInt(a, lo, hi)
		if err != nil {
			return err
		}
		last, err = cronInt(b, lo, hi)
		if err != nil {
			return err
		}
		if first > last {
			return fmt.Errorf("range %q: start above end", body)
		}
	default:
		n, err := cronInt(body, lo, hi)
		if err != nil {
			return err
		}
		first, last = n, n
		if hasStep {
			// `5/15` means start at 5 and repeat every 15 up to max, the
			// convention Vixie cron and Kubernetes share. Without a step a
			// single value selects exactly that value.
			last = hi
		}
	}
	for v := first; v <= last; v += step {
		set(v)
	}
	return nil
}

// cronInt parses one cron integer, rejecting what Atoi would silently
// accept ("+5", "05") the way the port validation does.
func cronInt(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi || strconv.Itoa(n) != s {
		return 0, fmt.Errorf("value %q: must be %d-%d", s, lo, hi)
	}
	return n, nil
}

// Next returns the first fire time strictly after t, in UTC, and false if
// the expression matches no time within the 4-year lookahead (an
// expression that can never fire, like "0 0 31 2 *").
func (c *Cron) Next(after time.Time) (time.Time, bool) {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	end := t.Add(MaxCronLookahead)
	for t.Before(end) {
		if !c.months[int(t.Month())] {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !c.hours[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !c.minutes[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// dayMatches applies the POSIX/K8s dom/dow rule: when BOTH fields are
// restricted, a date fires if EITHER matches; one restricted field alone
// decides; neither means every day.
func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.domAll || c.doms[t.Day()]
	dow := c.dowAll || c.dows[int(t.Weekday())]
	switch {
	case !c.domAll && !c.dowAll:
		return dom || dow
	case !c.domAll:
		return dom
	case !c.dowAll:
		return dow
	default:
		return true
	}
}
