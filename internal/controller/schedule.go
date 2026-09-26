package controller

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/store"
)

// reconcileSchedules fires due Schedules and prunes their generated-task
// history. It reuses the task snapshot reconcileAll already fetched, so a
// tick costs one ListTasks no matter how many schedules exist.
func (c *Controller) reconcileSchedules(ctx context.Context, tasks []*v1alpha1.Task) {
	schedules, err := c.store.ListSchedules()
	if err != nil {
		c.log.Error("list schedules", "err", err)
		return
	}
	now := c.now()
	for _, sch := range schedules {
		c.reconcileSchedule(ctx, sch, tasks, now)
	}
}

func (c *Controller) reconcileSchedule(ctx context.Context, sch *v1alpha1.Schedule, tasks []*v1alpha1.Task, now time.Time) {
	expr, err := v1alpha1.ParseCron(sch.Spec.Schedule)
	if err != nil {
		// Apply validates expressions, so a parse failure here means a
		// record written by an older build; skip it rather than fire from
		// a schedule the user can no longer read. The history window is
		// independent of the expression, so it still applies.
		c.log.Warn("schedule parse", "schedule", sch.Metadata.Name, "err", err)
		c.pruneScheduleHistory(ctx, sch, tasks)
		return
	}

	if sch.Spec.Suspend {
		// Held: no fires, but the history window still applies.
		c.pruneScheduleHistory(ctx, sch, tasks)
		return
	}

	// The fire anchor is the creation time until the first fire records
	// itself — never "now": Next(now) is always in the future, so a
	// never-fired schedule anchored there would become due only on a tick
	// landing exactly on a boundary. Same anchor rule as k8s CronJob.
	after := sch.Metadata.CreationTimestamp
	if sch.Status.LastScheduleTime != nil {
		after = *sch.Status.LastScheduleTime
	}
	if after.IsZero() {
		// No anchor at all: a broken record. Keep the history window tidy,
		// but never fire from an unknown anchor.
		c.pruneScheduleHistory(ctx, sch, tasks)
		c.log.Error("schedule has no fire anchor (no creation timestamp, never fired)", "schedule", sch.Metadata.Name)
		return
	}
	next, ok := expr.Next(after)
	if !ok {
		c.log.Warn("schedule matches no fire time within the lookahead", "schedule", sch.Metadata.Name)
		c.pruneScheduleHistory(ctx, sch, tasks)
		return
	}
	if next.After(now) {
		// Not due yet. Pruning still runs so suspend periods and quiet
		// schedules keep their history window tidy.
		c.pruneScheduleHistory(ctx, sch, tasks)
		return
	}

	// Due: compress missed fires into the last one. If the server was down
	// across three nightly fires, one task runs — late-but-once beats three
	// stale repeats.
	fireAt := next
	for {
		more, ok := expr.Next(fireAt)
		if !ok || more.After(now) {
			break
		}
		fireAt = more
	}
	c.fireSchedule(ctx, sch, fireAt)
	c.pruneScheduleHistory(ctx, sch, tasks)
}

// fireSchedule materializes one fire as a task named <schedule>-<unix fire
// time>, then advances the schedule's fire clock. The name encodes the fire
// time, so re-attempting an already-recorded fire is ErrExists rather than a
// duplicate run — the clock still advances in that case so one partial write
// (task persisted, clock not) cannot re-fire every tick.
func (c *Controller) fireSchedule(ctx context.Context, sch *v1alpha1.Schedule, fireAt time.Time) {
	name := v1alpha1.ScheduleTaskName(sch.Metadata.Name, fireAt)
	t := &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: name},
		Spec:       sch.Spec.TaskTemplate,
		Status: v1alpha1.TaskStatus{
			Phase:         v1alpha1.TaskPending,
			Reason:        "scheduled",
			ScheduleOwner: sch.Metadata.Name,
		},
	}
	lastTask := name
	err := c.store.CreateTask(t)
	if errors.Is(err, store.ErrExists) {
		// The fire name is taken: either our own partial write (task
		// persisted, clock not) — recover by advancing the clock — or a
		// foreign task that happened to take the name. Only a task this
		// schedule owns may be recorded as its last fire; a foreign one is
		// left alone and the fire is abandoned.
		prior, getErr := c.store.GetTask(name)
		if getErr != nil || prior.Status.ScheduleOwner != sch.Metadata.Name {
			c.log.Warn("schedule fire name taken by a foreign task; abandoning this fire and advancing the clock", "schedule", sch.Metadata.Name, "task", name)
			lastTask = ""
		} else {
			c.log.Warn("schedule fire already recorded as a task; advancing clock without re-firing", "schedule", sch.Metadata.Name, "task", name)
		}
	} else if err != nil {
		// Leave the clock where it is; the next tick retries this fire.
		c.log.Error("schedule fire", "schedule", sch.Metadata.Name, "err", err)
		return
	}
	if err := c.store.MarkScheduleFired(sch.Metadata.Name, fireAt, lastTask); err != nil {
		c.log.Error("schedule clock", "schedule", sch.Metadata.Name, "err", err)
		return
	}
	c.eventf(name, "Scheduled", "fired by schedule %s for %s", sch.Metadata.Name, fireAt.UTC().Format(time.RFC3339))
	c.log.Info("schedule fired", "schedule", sch.Metadata.Name, "task", name, "fireAt", fireAt.UTC().Format(time.RFC3339))
}

// pruneScheduleHistory destroys the oldest terminal generated tasks beyond
// the schedule's historyLimit. Only terminal tasks with no pending deletion
// count against the limit — an in-flight task stays regardless of age, and
// one already going away is not counted twice. Pruning runs while suspended
// and while quiet too: the window bounds what exists, not what fires.
func (c *Controller) pruneScheduleHistory(ctx context.Context, sch *v1alpha1.Schedule, tasks []*v1alpha1.Task) {
	limit := sch.Spec.HistoryLimit
	if limit == 0 {
		limit = v1alpha1.DefaultScheduleHistory
	}
	var finished []*v1alpha1.Task
	for _, t := range tasks {
		if t.Status.ScheduleOwner != sch.Metadata.Name || t.Status.DeletionTimestamp != nil || !t.Status.Phase.Terminal() {
			continue
		}
		finished = append(finished, t)
	}
	if len(finished) <= limit {
		return
	}
	// Oldest first: the name suffix is the fire time.
	sort.Slice(finished, func(i, j int) bool {
		return scheduleFireTime(finished[i]) < scheduleFireTime(finished[j])
	})
	for _, t := range finished[:len(finished)-limit] {
		if err := c.RequestDestroy(t.Metadata.Name); err != nil {
			c.log.Warn("schedule history prune", "schedule", sch.Metadata.Name, "task", t.Metadata.Name, "err", err)
			continue
		}
		c.log.Info("schedule history pruned", "schedule", sch.Metadata.Name, "task", t.Metadata.Name)
	}
}

// scheduleFireTime recovers the fire time encoded in a generated task's
// name; 0 for names that do not parse, which sorts oldest and prunes first.
func scheduleFireTime(t *v1alpha1.Task) int64 {
	i := strings.LastIndexByte(t.Metadata.Name, '-')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(t.Metadata.Name[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
