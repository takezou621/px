// Package controller reconciles Tasks toward their desired end state by
// driving a Provisioner and persisting observed status.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

// TaskReader gives the controller read access to persisted tasks.
type TaskReader interface {
	GetTask(name string) (*v1alpha1.Task, error)
	ListTasks() ([]*v1alpha1.Task, error)
}

// TaskWriter lets the controller persist status changes. UpsertTask
// implementations must preserve an already-persisted DeletionTimestamp: the
// controller works on stale snapshots, and a status write must never erase a
// deletion request that arrived while the snapshot was being processed.
type TaskWriter interface {
	UpsertTask(*v1alpha1.Task) error
	MarkTaskDeleted(name string, at time.Time) error
	DeleteTask(name string) error
}

type Controller struct {
	store interface {
		TaskReader
		TaskWriter
	}
	prov Provisioner
	log  *slog.Logger
	Tick time.Duration // reconcile interval
	now  func() time.Time
}

func New(store interface {
	TaskReader
	TaskWriter
}, prov Provisioner, log *slog.Logger) *Controller {
	return &Controller{
		store: store,
		prov:  prov,
		log:   log,
		Tick:  2 * time.Second,
		now:   time.Now,
	}
}

// ReconcileOnce runs a single reconcile pass over all tasks (for tests).
func (c *Controller) ReconcileOnce(ctx context.Context) { c.reconcileAll(ctx) }

// Run blocks, reconciling every task on each tick until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	tick := time.NewTicker(c.Tick)
	defer tick.Stop()
	for {
		c.reconcileAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// RequestDestroy persists a deletion request on the task record; the next
// reconcile destroys the container and drops the record. It updates only the
// deletion timestamp in a single statement — never a full status overwrite —
// so it cannot clobber a VMID that reconcile persists concurrently. The mark
// lives in the store, so an in-flight delete survives a px-server restart.
func (c *Controller) RequestDestroy(name string) error {
	return c.store.MarkTaskDeleted(name, c.now())
}

func (c *Controller) reconcileAll(ctx context.Context) {
	tasks, err := c.store.ListTasks()
	if err != nil {
		c.log.Error("list tasks", "err", err)
		return
	}
	for _, t := range tasks {
		c.reconcile(ctx, t)
	}
}

func (c *Controller) reconcile(ctx context.Context, t *v1alpha1.Task) {
	if t.Status.DeletionTimestamp != nil {
		c.destroyTask(ctx, t)
		return
	}

	switch t.Status.Phase {
	case "", v1alpha1.TaskPending:
		c.provision(ctx, t)
	case v1alpha1.TaskProvisioning:
		// The reconciler is serial, so a Provisioning task in the store is
		// always a provision interrupted by a px-server restart.
		if t.Status.Container == 0 {
			// The restart hit before the VMID was persisted; no container exists.
			c.log.Warn("stale provisioning recovered", "task", t.Metadata.Name)
			t.Status.Phase = v1alpha1.TaskProvisionFail
			t.Status.Reason = "provisioning interrupted by restart"
			t.Status.EndedAt = nowPtr(c.now)
			c.persist(t)
			return
		}
		// A container was cloned, but Create never completed: the boot may
		// have died with the SSH session. Adopt the task only if the runner
		// actually launched; otherwise the container is a partial clone —
		// clean it up rather than spin in a fake Running.
		booted, err := c.prov.Booted(ctx, t.Status.Container)
		if err != nil {
			// Transient probe failure; retry on the next tick. A container
			// that is gone or stopped makes pct exec fail with a non-zero
			// code, which lands in the unbooted branch below.
			c.log.Warn("boot probe", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
			return
		}
		if booted {
			t.Status.Phase = v1alpha1.TaskRunning
			now := c.now()
			t.Status.StartedAt = &now
			t.Status.Reason = ""
			c.log.Info("adopted interrupted provision", "task", t.Metadata.Name, "vmid", t.Status.Container)
			c.persist(t)
			return
		}
		if err := c.prov.Destroy(ctx, t.Status.Container); err != nil {
			c.log.Error("cleanup interrupted provision", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
			return
		}
		t.Status.Container = 0
		t.Status.Phase = v1alpha1.TaskProvisionFail
		t.Status.Reason = "provisioning interrupted by restart: container never booted a runner, cleaned up"
		t.Status.EndedAt = nowPtr(c.now)
		c.log.Warn("stale provisioning recovered", "task", t.Metadata.Name, "cleaned", "partial container destroyed")
		c.persist(t)
	case v1alpha1.TaskRunning:
		c.poll(ctx, t)
	case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskProvisionFail:
		c.cleanupAfterTTL(ctx, t)
	}
}

// destroyTask destroys the container and drops the record. Until Destroy
// succeeds the record keeps its DeletionTimestamp, so a failure or a crash
// mid-destroy retries on the next tick.
func (c *Controller) destroyTask(ctx context.Context, t *v1alpha1.Task) {
	if vmid := t.Status.Container; vmid != 0 {
		if err := c.prov.Destroy(ctx, vmid); err != nil {
			// Keep the record and retry next tick; deleting it now
			// would orphan the container with nothing left to retry.
			c.log.Error("destroy container", "task", t.Metadata.Name, "vmid", vmid, "err", err)
			return
		}
		t.Status.Container = 0
		c.persist(t)
	} else if t.Status.Phase == v1alpha1.TaskProvisioning {
		c.log.Warn("deleting task that was still provisioning with no container recorded", "task", t.Metadata.Name)
	}
	if err := c.store.DeleteTask(t.Metadata.Name); err != nil {
		c.log.Error("delete task", "task", t.Metadata.Name, "err", err)
	}
}

func (c *Controller) provision(ctx context.Context, t *v1alpha1.Task) {
	if ctx.Err() != nil {
		return
	}
	t.Status.Phase = v1alpha1.TaskProvisioning
	t.Status.Reason = "cloning template and starting container"
	c.persist(t)

	vmid, err := c.prov.Allocate(ctx)
	if err != nil {
		c.failProvision(t, fmt.Errorf("allocate: %w", err))
		return
	}
	// Persist the VMID before any node-side work: if the process dies
	// mid-Create the record still names the container, so delete and TTL can
	// destroy it after a restart.
	t.Status.Container = vmid
	if err := c.store.UpsertTask(t); err != nil {
		// The persisted VMID is the crash-safety anchor for everything
		// node-side; if it cannot be recorded, do not create the container.
		// Clear it before failing: the retry write inside failProvision may
		// succeed, and must not then persist a VMID for a container that was
		// never created.
		t.Status.Container = 0
		c.failProvision(t, fmt.Errorf("persist vmid: %w", err))
		return
	}

	if err := c.prov.Create(ctx, t, vmid); err != nil {
		// Create cleans up its own partial work, so the VMID no longer names
		// a container of ours. Clear it: a later destroy must never target an
		// id that Create may have lost to another owner (PVE's nextid is a
		// suggestion, not a reservation).
		t.Status.Container = 0
		c.failProvision(t, err)
		return
	}
	t.Status.Phase = v1alpha1.TaskRunning
	now := c.now()
	t.Status.StartedAt = &now
	t.Status.Reason = ""
	c.log.Info("task running", "task", t.Metadata.Name, "vmid", vmid)
	c.persist(t)
}

func (c *Controller) failProvision(t *v1alpha1.Task, err error) {
	t.Status.Phase = v1alpha1.TaskProvisionFail
	t.Status.Reason = err.Error()
	t.Status.EndedAt = nowPtr(c.now)
	c.log.Error("provision task", "task", t.Metadata.Name, "err", err)
	c.persist(t)
}

func (c *Controller) poll(ctx context.Context, t *v1alpha1.Task) {
	if t.Status.Container == 0 {
		return
	}
	code, err := c.prov.Exit(ctx, t.Status.Container)
	if err != nil {
		// The container may have died without writing an exit file (OOM,
		// node reboot); distinguish that from a transient probe failure.
		if running, rerr := c.prov.Running(ctx, t.Status.Container); rerr == nil && !running {
			end := c.now()
			t.Status.EndedAt = &end
			t.Status.Phase = v1alpha1.TaskFailed
			t.Status.Reason = "container not running and no exit file: " + err.Error()
			c.log.Warn("container died", "task", t.Metadata.Name, "vmid", t.Status.Container)
			c.persist(t)
			return
		}
		c.log.Warn("poll exit", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		return
	}
	if code == nil {
		return
	}
	end := c.now()
	t.Status.EndedAt = &end
	t.Status.ExitCode = *code
	if *code == 0 {
		t.Status.Phase = v1alpha1.TaskSucceeded
		t.Status.Reason = "runner exited 0"
	} else {
		t.Status.Phase = v1alpha1.TaskFailed
		t.Status.Reason = "runner exited non-zero"
	}
	c.log.Info("task finished", "task", t.Metadata.Name, "exit", *code, "phase", t.Status.Phase)
	c.persist(t)
}

func (c *Controller) cleanupAfterTTL(ctx context.Context, t *v1alpha1.Task) {
	ttl := t.Spec.TTLSecondsAfterFinished
	if ttl <= 0 || t.Status.Container == 0 || t.Status.EndedAt == nil {
		return
	}
	deadline := t.Status.EndedAt.Add(time.Duration(ttl) * time.Second)
	if c.now().Before(deadline) {
		return
	}
	vmid := t.Status.Container
	if err := c.prov.Destroy(ctx, vmid); err != nil {
		c.log.Error("ttl destroy", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		return
	}
	t.Status.Container = 0
	t.Status.Reason += " (container cleaned up after TTL)"
	c.persist(t)
}

func (c *Controller) persist(t *v1alpha1.Task) {
	if err := c.store.UpsertTask(t); err != nil {
		c.log.Error("persist task", "task", t.Metadata.Name, "err", err)
	}
}

func nowPtr(now func() time.Time) *time.Time {
	t := now()
	return &t
}
