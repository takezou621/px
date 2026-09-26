package v1alpha1

import (
	"testing"
	"time"
)

func TestParseCronSingleValuesSelectExactlyOneSlot(t *testing.T) {
	// Regression: "0" in the minute field once expanded to every minute,
	// silently turning hourly schedules into every-minute ones.
	c, err := ParseCron("0 12 15 6 *")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := c.Next(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Next: no match")
	}
	// dom restricted alone (dow is *): June 15 12:00 every year.
	want := time.Date(2027, 6, 15, 12, 0, 0, 0, time.UTC)
	if !n.Equal(want) {
		t.Fatalf("Next = %s, want %s", n, want)
	}
	n2, ok := c.Next(n)
	if !ok || !n2.Equal(want.AddDate(1, 0, 0)) {
		t.Fatalf("Next again = %s, want exactly one year later", n2)
	}
}

func TestParseCronFieldSets(t *testing.T) {
	for _, tt := range []struct {
		expr string
		fire time.Time
	}{
		{expr: "*/15 * * * *", fire: time.Date(2026, 1, 1, 3, 45, 0, 0, time.UTC)},
		{expr: "5/15 * * * *", fire: time.Date(2026, 1, 1, 0, 50, 0, 0, time.UTC)},
		{expr: "1-10/3 * * * *", fire: time.Date(2026, 1, 1, 0, 7, 0, 0, time.UTC)},
		{expr: "0 9-17 * * *", fire: time.Date(2026, 1, 1, 17, 0, 0, 0, time.UTC)},
		{expr: "30 8 1 * *", fire: time.Date(2026, 5, 1, 8, 30, 0, 0, time.UTC)},
		{expr: "0 0 1 1 *", fire: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{expr: "59 23 31 12 *", fire: time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)},
	} {
		c, err := ParseCron(tt.expr)
		if err != nil {
			t.Fatalf("%q: %v", tt.expr, err)
		}
		got, ok := c.Next(tt.fire.Add(-time.Minute))
		if !ok || !got.Equal(tt.fire) {
			t.Fatalf("%q: Next(%s) = %s (ok=%v), want %s", tt.expr, tt.fire.Add(-time.Minute), got, ok, tt.fire)
		}
	}
}

func TestParseCronDomDowOrRule(t *testing.T) {
	// Both fields restricted: POSIX/K8s OR — any 13th OR any Friday fires.
	// Jan 1 2026 is a Thursday, so the first match is Fri Jan 2.
	c, err := ParseCron("0 0 13 * 5")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Next: no match")
	}
	if want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC); !n.Equal(want) {
		t.Fatalf("Next = %s, want %s (first Friday)", n, want)
	}
	// Only dom restricted: every 13th fires regardless of weekday.
	c2, _ := ParseCron("0 0 13 * *")
	n2, _ := c2.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC); !n2.Equal(want) {
		t.Fatalf("dom-only Next = %s, want %s", n2, want)
	}
	// Only dow restricted: every Friday.
	c3, _ := ParseCron("0 0 * * 5")
	n3, _ := c3.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) // Thu
	if want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC); !n3.Equal(want) {
		t.Fatalf("dow-only Next = %s, want %s", n3, want)
	}
}

func TestParseCronDowSevenIsSunday(t *testing.T) {
	c7, err := ParseCron("0 0 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	c0, err := ParseCron("0 0 * * 0")
	if err != nil {
		t.Fatal(err)
	}
	n7, _ := c7.Next(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)) // a Saturday
	n0, _ := c0.Next(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC); !n7.Equal(want) || !n0.Equal(want) {
		t.Fatalf("dow 7 = %s, dow 0 = %s, want %s", n7, n0, want)
	}
}

func TestParseCronNeverFires(t *testing.T) {
	c, err := ParseCron("0 0 31 2 *") // Feb 31 does not exist
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Next(time.Now()); ok {
		t.Fatal("Feb 31 must match no time within the lookahead")
	}
}

// A step wider than its field range selects only the first value. Before
// the clamp, a step near MaxInt64 overflowed the fire walk's increment,
// wrapped negative, and indexed outside the field array — a crafted apply
// could panic the server.
func TestParseCronHugeStepSelectsFirstValueOnly(t *testing.T) {
	base := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	c, err := ParseCron("1/9223372036854775807 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := c.Next(base)
	if !ok || !n.Equal(time.Date(2026, 9, 26, 11, 1, 0, 0, time.UTC)) {
		t.Fatalf("Next = %s, want 11:01 (minute 1 only)", n)
	}
	wide, err := ParseCron("*/9223372036854775807 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	n2, ok := wide.Next(base)
	if !ok || n2.Minute() != 0 {
		t.Fatalf("Next = %s, want minute 0 only", n2)
	}
}

func TestParseCronRejects(t *testing.T) {
	for _, expr := range []string{
		"banana",
		"0 0 * *",     // too few fields
		"0 0 * * * *", // too many
		"",            // empty
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // dom starts at 1
		"* * 32 * *",  // dom out of range
		"* * * 13 *",  // month out of range
		"* * * * 8",   // dow out of range (0-7)
		"-5 * * * *",  // signed
		"+5 * * * *",  // signed
		"05 * * * *",  // leading zero
		"1-0 * * * *", // range start above end
		"1--3 * * * *",
		"*/0 * * * *",  // zero step
		"5/x * * * *",  // non-numeric step
		"5/ * * * *",   // empty step
		"/5 * * * *",   // empty body
		"1,2, * * * *", // trailing comma item
		"1,,2 * * * *", // empty list item
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) accepted; want error", expr)
		}
	}
}

func TestNextUTCAndStrictlyAfter(t *testing.T) {
	c, _ := ParseCron("30 * * * *")
	// Exactly on a fire time: Next is the next one, never the same minute.
	n, ok := c.Next(time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC))
	if !ok || !n.Equal(time.Date(2026, 9, 26, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("Next = %s, want 11:30", n)
	}
	// Local-time input is normalized to UTC.
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	local := time.Date(2026, 9, 26, 3, 0, 0, 0, losAngeles) // 10:00 UTC
	n2, _ := c.Next(local)
	if !n2.Equal(time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("Next(local) = %s, want 10:30 UTC", n2)
	}
}
