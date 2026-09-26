package controller

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

func addSchedule(t *testing.T, m *memStore, sch *v1alpha1.Schedule) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedules[sch.Metadata.Name] = sch
}

func testSchedule(name, expr string) *v1alpha1.Schedule {
	return &v1alpha1.Schedule{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindSchedule,
		Metadata:   v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.ScheduleSpec{
			Schedule: expr,
			TaskTemplate: v1alpha1.TaskSpec{
				Image:  "tmpl",
				Runner: v1alpha1.RunnerSpec{Command: []string{"true"}},
			},
		},
	}
}

func getSchedule(t *testing.T, m *memStore, name string) *v1alpha1.Schedule {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	sch, ok := m.schedules[name]
	if !ok {
		t.Fatalf("schedule %s gone", name)
	}
	return sch
}

func scheduleController(m *memStore, now time.Time) *Controller {
	ctl := New(m, &fakeProv{exits: map[int]int{}}, slog.New(slog.DiscardHandler))
	ctl.now = func() time.Time { return now }
	return ctl
}

func firedTask(schedule string, fireAt time.Time, phase v1alpha1.TaskPhase) *v1alpha1.Task {
	return &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: v1alpha1.ScheduleTaskName(schedule, fireAt)},
		Spec: v1alpha1.TaskSpec{
			Image:  "tmpl",
			Runner: v1alpha1.RunnerSpec{Command: []string{"true"}},
		},
		Status: v1alpha1.TaskStatus{Phase: phase, ScheduleOwner: schedule},
	}
}

func TestScheduleFiresWhenDue(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 17, 0, time.UTC)
	sch := testSchedule("everymin", "* * * * *")
	last := now.Truncate(time.Minute).Add(-time.Minute) // 10:29: 10:30 is due
	sch.Status.LastScheduleTime = &last
	addSchedule(t, m, sch)
	ctl := scheduleController(m, now)

	runOnce(ctl)

	sch = getSchedule(t, m, "everymin")
	if sch.Status.LastScheduleTime == nil {
		t.Fatal("fire clock not advanced")
	}
	wantFire := now.Truncate(time.Minute)
	if !sch.Status.LastScheduleTime.Equal(wantFire) {
		t.Fatalf("clock = %s, want %s", sch.Status.LastScheduleTime.UTC(), wantFire)
	}
	if sch.Status.LastTask != "everymin-"+strconv.FormatInt(wantFire.Unix(), 10) {
		t.Fatalf("lastTask = %q", sch.Status.LastTask)
	}
	task := get(t, m, sch.Status.LastTask)
	if task.Status.Phase != v1alpha1.TaskPending || task.Status.Reason != "scheduled" {
		t.Fatalf("generated task status = %s/%s, want Pending/scheduled", task.Status.Phase, task.Status.Reason)
	}
	if task.Status.ScheduleOwner != "everymin" {
		t.Fatalf("scheduleOwner = %q", task.Status.ScheduleOwner)
	}
	if task.Spec.Image != "tmpl" || len(task.Spec.Runner.Command) != 1 {
		t.Fatalf("template not copied: %+v", task.Spec)
	}
}

func TestScheduleWaitsWhenNotDue(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("yearly", "0 0 1 1 *")
	sch.Metadata.CreationTimestamp = now.Add(-48 * time.Hour)
	addSchedule(t, m, sch)
	ctl := scheduleController(m, now)

	runOnce(ctl)

	if sch.Status.LastScheduleTime != nil {
		t.Fatal("clock advanced on a not-due schedule")
	}
	if len(m.tasks) != 0 {
		t.Fatalf("tasks created: %d", len(m.tasks))
	}
}

// The real-world first-fire path: apply lands between boundaries, so the
// anchor is mid-minute and the first due tick is well past the boundary.
// Anchoring on "now" instead would keep Next(now) strictly ahead forever.
func TestScheduleFirstFireAfterCreation(t *testing.T) {
	m := newMemStore()
	created := time.Date(2026, 9, 26, 10, 29, 30, 0, time.UTC)
	sch := testSchedule("first", "* * * * *")
	sch.Metadata.CreationTimestamp = created
	addSchedule(t, m, sch)
	ctl := scheduleController(m, created.Add(15*time.Second)) // 10:29:45, not due

	runOnce(ctl)
	if sch.Status.LastScheduleTime != nil {
		t.Fatal("clock advanced before the first boundary")
	}
	if len(m.tasks) != 0 {
		t.Fatalf("fired %d tasks before the first boundary", len(m.tasks))
	}

	ctl = scheduleController(m, created.Add(time.Minute+17*time.Second)) // 10:30:47, past the boundary
	runOnce(ctl)

	want := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	if sch.Status.LastScheduleTime == nil || !sch.Status.LastScheduleTime.Equal(want) {
		t.Fatalf("clock = %v, want %s", sch.Status.LastScheduleTime, want)
	}
	if len(m.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(m.tasks))
	}
}

func TestScheduleMissedFiresCompressToOne(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("hourly", "0 * * * *")
	last := now.Add(-3 * time.Hour) // 07:30: missed 08:00, 09:00, 10:00
	sch.Status.LastScheduleTime = &last
	addSchedule(t, m, sch)
	ctl := scheduleController(m, now)

	runOnce(ctl)

	wantFire := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if sch.Status.LastScheduleTime == nil || !sch.Status.LastScheduleTime.Equal(wantFire) {
		t.Fatalf("clock = %s, want %s", sch.Status.LastScheduleTime.UTC(), wantFire)
	}
	if len(m.tasks) != 1 {
		t.Fatalf("tasks created = %d, want 1", len(m.tasks))
	}
	if _, ok := m.tasks["hourly-"+strconv.FormatInt(wantFire.Unix(), 10)]; !ok {
		t.Fatalf("expected task for the newest missed fire %s", wantFire)
	}
}

func TestScheduleSuspendHoldsFire(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("held", "* * * * *")
	sch.Spec.Suspend = true
	addSchedule(t, m, sch)
	ctl := scheduleController(m, now)

	runOnce(ctl)

	if sch.Status.LastScheduleTime != nil {
		t.Fatal("suspended schedule advanced its clock")
	}
	if len(m.tasks) != 0 {
		t.Fatalf("suspended schedule fired %d tasks", len(m.tasks))
	}
}

func TestScheduleResumeFiresOnce(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("held", "* * * * *")
	last := now.Add(-time.Minute) // resume fires the 10:30 boundary
	sch.Status.LastScheduleTime = &last
	sch.Spec.Suspend = true
	addSchedule(t, m, sch)
	ctl := scheduleController(m, now)
	runOnce(ctl) // suspended: nothing

	sch.Spec.Suspend = false
	runOnce(ctl)

	if len(m.tasks) != 1 {
		t.Fatalf("tasks after resume = %d, want 1", len(m.tasks))
	}
	runOnce(ctl) // same minute: clock already at it, no second fire
	if len(m.tasks) != 1 {
		t.Fatalf("tasks after re-tick = %d, want 1", len(m.tasks))
	}
}

func TestSchedulePrunesHistory(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("win", "0 0 1 1 *") // next fire Jan 1: prune-only tick
	sch.Metadata.CreationTimestamp = now.Add(-time.Hour)
	sch.Spec.HistoryLimit = 2
	addSchedule(t, m, sch)
	for i := 5; i >= 1; i-- {
		fireAt := now.Add(-time.Duration(i) * time.Hour)
		if err := m.UpsertTask(firedTask("win", fireAt, v1alpha1.TaskSucceeded)); err != nil {
			t.Fatal(err)
		}
	}
	ctl := scheduleController(m, now)

	runOnce(ctl)

	var kept, pruned int
	m.mu.Lock()
	for name, task := range m.tasks {
		if task.Status.DeletionTimestamp != nil {
			pruned++
		} else {
			kept++
		}
		_ = name
	}
	m.mu.Unlock()
	if pruned != 3 || kept != 2 {
		t.Fatalf("pruned = %d kept = %d, want 3/2", pruned, kept)
	}
}

func TestSchedulePruneKeepsNonTerminalAndDeletionPending(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("win", "0 0 1 1 *")
	sch.Metadata.CreationTimestamp = now.Add(-time.Hour)
	sch.Spec.HistoryLimit = 1
	addSchedule(t, m, sch)
	// A running task older than the limit, and a Succeeded one already being
	// destroyed: neither counts against the window.
	old := now.Add(-24 * time.Hour)
	if err := m.UpsertTask(firedTask("win", old, v1alpha1.TaskRunning)); err != nil {
		t.Fatal(err)
	}
	dying := now.Add(-2 * time.Hour)
	dyingTask := firedTask("win", dying, v1alpha1.TaskSucceeded)
	if err := m.UpsertTask(dyingTask); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkTaskDeleted(dyingTask.Metadata.Name, now); err != nil {
		t.Fatal(err)
	}
	// One finished task within the limit.
	recent := now.Add(-time.Hour)
	if err := m.UpsertTask(firedTask("win", recent, v1alpha1.TaskFailed)); err != nil {
		t.Fatal(err)
	}
	ctl := scheduleController(m, now)

	runOnce(ctl)

	// The pruning pass must not have targeted the running task or the
	// finished one inside the window. (The deletion-pending task is consumed
	// by the normal destroy path in the same tick, so it is gone regardless.)
	for _, name := range []string{
		v1alpha1.ScheduleTaskName("win", old),
		v1alpha1.ScheduleTaskName("win", recent),
	} {
		m.mu.Lock()
		_, ok := m.tasks[name]
		m.mu.Unlock()
		if !ok {
			t.Fatalf("task %s was deleted", name)
		}
	}
}

func TestScheduleFireNameTakenAdvancesClock(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("clash", "* * * * *")
	last := now.Add(-time.Minute)
	sch.Status.LastScheduleTime = &last
	addSchedule(t, m, sch)
	wantFire := now.Truncate(time.Minute)
	taken := firedTask("clash", wantFire, v1alpha1.TaskSucceeded)
	if err := m.UpsertTask(taken); err != nil {
		t.Fatal(err)
	}
	ctl := scheduleController(m, now)

	runOnce(ctl)

	if sch.Status.LastScheduleTime == nil || !sch.Status.LastScheduleTime.Equal(wantFire) {
		t.Fatalf("clock = %v, want %s", sch.Status.LastScheduleTime, wantFire)
	}
	if len(m.tasks) != 1 {
		t.Fatalf("tasks = %d, want the pre-existing one only", len(m.tasks))
	}
}

func TestScheduleFireNameTakenByForeignTaskAdvancesClock(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("clash", "* * * * *")
	last := now.Add(-time.Minute)
	sch.Status.LastScheduleTime = &last
	addSchedule(t, m, sch)
	wantFire := now.Truncate(time.Minute)
	taken := firedTask("other-schedule", wantFire, v1alpha1.TaskSucceeded)
	// Squat the fire name clash-<fireAt> while carrying a foreign owner —
	// firedTask derives the name from the owner, so overwrite it.
	taken.Metadata.Name = v1alpha1.ScheduleTaskName("clash", wantFire)
	if err := m.UpsertTask(taken); err != nil {
		t.Fatal(err)
	}
	ctl := scheduleController(m, now)

	runOnce(ctl)

	if sch.Status.LastScheduleTime == nil || !sch.Status.LastScheduleTime.Equal(wantFire) {
		t.Fatalf("clock = %v, want %s", sch.Status.LastScheduleTime, wantFire)
	}
	if sch.Status.LastTask != "" {
		t.Fatalf("lastTask = %q, want empty: a task this schedule does not own must not be recorded as its fire", sch.Status.LastTask)
	}
	if len(m.tasks) != 1 {
		t.Fatalf("tasks = %d, want the foreign one only", len(m.tasks))
	}
}

func TestScheduleStoreErrorRetainsClock(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	sch := testSchedule("flaky", "* * * * *")
	last := now.Add(-time.Minute)
	sch.Status.LastScheduleTime = &last
	addSchedule(t, m, sch)
	m.failCreates = 1
	ctl := scheduleController(m, now)

	runOnce(ctl)
	if sch.Status.LastScheduleTime == nil || !sch.Status.LastScheduleTime.Equal(last) {
		t.Fatalf("clock = %v, want it held at %s despite the fire write failing", sch.Status.LastScheduleTime, last)
	}

	runOnce(ctl)
	if len(m.tasks) != 1 {
		t.Fatalf("tasks after retry = %d, want 1", len(m.tasks))
	}
}

func TestScheduleBadAndNeverFiringExpressions(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)
	broken := testSchedule("broken", "banana")
	dead := testSchedule("dead", "0 0 31 2 *")
	addSchedule(t, m, broken)
	addSchedule(t, m, dead)
	ctl := scheduleController(m, now)

	runOnce(ctl) // must not panic, must not fire

	if len(m.tasks) != 0 {
		t.Fatalf("tasks = %d, want 0", len(m.tasks))
	}
	if broken.Status.LastScheduleTime != nil || dead.Status.LastScheduleTime != nil {
		t.Fatal("clock advanced on an unusable schedule")
	}
}
