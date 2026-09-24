// Package controller reconciles Tasks toward their desired end state by
// driving a Provisioner and persisting observed status.
package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

// TaskReader gives the controller read access to persisted tasks.
type TaskReader interface {
	ListTasks() ([]*v1alpha1.Task, error)
}

// TaskWriter lets the controller persist status changes.
type TaskWriter interface {
	UpsertTask(*v1alpha1.Task) error
	DeleteTask(name string) error
}

// Deleter signals that a task was deleted while running and its container
// should be destroyed (wired by the server on DELETE requests).
type Deleter interface {
	DeleteTaskAndDestroy(ctx context.Context, name string) error
}

type Controller struct {
	store interface {
		TaskReader
		TaskWriter
	}
	prov   Provisioner
	log    *slog.Logger
	Tick   time.Duration // reconcile interval
	now    func() time.Time

	mu      sync.Mutex
	destroy map[string]int // task name -> vmid pending destroy
}

func New(store interface {
	TaskReader
	TaskWriter
}, prov Provisioner, log *slog.Logger) *Controller {
	return &Controller{
		store:   store,
		prov:    prov,
		log:     log,
		Tick:    2 * time.Second,
		now:     time.Now,
		destroy: map[string]int{},
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

// RequestDestroy asks the controller to destroy a task's container on the
// next reconcile (used by the API server on DELETE).
func (c *Controller) RequestDestroy(name string, vmid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.destroy[name] = vmid
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
	// Explicit delete: destroy the container and drop the record.
	c.mu.Lock()
	vmid, pending := c.destroy[t.Metadata.Name]
	if pending {
		delete(c.destroy, t.Metadata.Name)
	}
	c.mu.Unlock()
	if pending {
		if vmid != 0 {
			if err := c.prov.Destroy(ctx, vmid); err != nil {
				c.log.Error("destroy container", "task", t.Metadata.Name, "vmid", vmid, "err", err)
			}
		}
		if err := c.store.DeleteTask(t.Metadata.Name); err != nil {
			c.log.Error("delete task", "task", t.Metadata.Name, "err", err)
		}
		return
	}

	switch t.Status.Phase {
	case "", v1alpha1.TaskPending:
		c.provision(ctx, t)
	case v1alpha1.TaskProvisioning, v1alpha1.TaskRunning:
		c.poll(ctx, t)
	case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskProvisionFail:
		c.cleanupAfterTTL(ctx, t)
	}
}

func (c *Controller) provision(ctx context.Context, t *v1alpha1.Task) {
	if ctx.Err() != nil {
		return
	}
	t.Status.Phase = v1alpha1.TaskProvisioning
	t.Status.Reason = "cloning template and starting container"
	c.persist(t)

	vmid, err := c.prov.Create(ctx, t)
	if err != nil {
		t.Status.Phase = v1alpha1.TaskProvisionFail
		t.Status.Reason = err.Error()
		t.Status.EndedAt = nowPtr(c.now)
		c.log.Error("provision task", "task", t.Metadata.Name, "err", err)
		c.persist(t)
		return
	}
	t.Status.Container = vmid
	t.Status.Phase = v1alpha1.TaskRunning
	now := c.now()
	t.Status.StartedAt = &now
	t.Status.Reason = ""
	c.log.Info("task running", "task", t.Metadata.Name, "vmid", vmid)
	c.persist(t)
}

func (c *Controller) poll(ctx context.Context, t *v1alpha1.Task) {
	if t.Status.Container == 0 {
		return
	}
	code, err := c.prov.Exit(ctx, t.Status.Container)
	if err != nil {
		c.log.Warn("poll exit", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		return
	}
	if code == nil {
		if t.Status.Phase != v1alpha1.TaskRunning {
			t.Status.Phase = v1alpha1.TaskRunning
			c.persist(t)
		}
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
