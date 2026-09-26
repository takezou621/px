// Package controller reconciles Tasks toward their desired end state by
// driving a Provisioner and persisting observed status.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/metrics"
	"github.com/kawai/px/internal/store"
)

// TaskReader gives the controller read access to persisted tasks.
type TaskReader interface {
	GetTask(name string) (*v1alpha1.Task, error)
	ListTasks() ([]*v1alpha1.Task, error)
}

// WorkspaceReader resolves task workspace references against stored
// Workspace resources.
type WorkspaceReader interface {
	GetWorkspace(name string) (*v1alpha1.Workspace, error)
}

// ModelReader resolves task model references against stored Model resources.
type ModelReader interface {
	GetModel(name string) (*v1alpha1.Model, error)
}

// GatewayReader resolves task gateway references against stored Gateway
// resources.
type GatewayReader interface {
	GetGateway(name string) (*v1alpha1.Gateway, error)
}

// SessionReader resolves continueFrom references against captured sessions.
type SessionReader interface {
	GetSession(task string) ([]byte, error)
	// GetDefaultSession resolves a default (task-named) capture only: an
	// explicitly owned row under the same name is somebody else's
	// conversation, not the source task's.
	GetDefaultSession(task string) ([]byte, error)
}

// SessionWriter persists a captured session archive and drops default
// rows when the task record itself goes. The first argument is the capture
// name — the task's name for default captures, or the explicit
// spec.session.name. lastTask records which task wrote the capture, for
// session-continuation spec lookups; explicit marks user-owned rows, which
// outlive their writing task.
type SessionWriter interface {
	SaveSession(name, lastTask string, data []byte, explicit bool) error
	DeleteSession(name string) error
	DeleteDefaultSession(name string) error
}

// TaskWriter lets the controller persist status changes. UpsertTask
// implementations must preserve an already-persisted DeletionTimestamp: the
// controller works on stale snapshots, and a status write must never erase a
// deletion request that arrived while the snapshot was being processed.
type TaskWriter interface {
	UpsertTask(*v1alpha1.Task) error
	MarkTaskDeleted(name string, at time.Time) error
	// MarkTaskPhase is a compare-and-set on expect: the write lands only if
	// the persisted phase still matches what the caller read, so a phase mark
	// racing a reconcile transition fails instead of freezing a task that
	// already finished.
	MarkTaskPhase(name string, expect, phase v1alpha1.TaskPhase, reason string) error
	DeleteTask(name string) error
}

// EventRecorder appends one event to a task's history. Optional: a nil
// Events field disables recording entirely (tests, embedders without a
// store that carries events).
type EventRecorder interface {
	RecordEvent(task string, at time.Time, reason, message string) error
}

type Controller struct {
	store interface {
		TaskReader
		WorkspaceReader
		ModelReader
		GatewayReader
		SessionReader
		TaskWriter
		SessionWriter
	}
	prov Provisioner
	log  *slog.Logger
	Tick time.Duration // reconcile interval
	now  func() time.Time

	// Events records lifecycle transitions as they happen (nil disables).
	// A failed record only logs — an event is an observation, never a gate
	// on the transition it describes.
	Events EventRecorder
	// Metrics accumulates the tick counters the /v1/metrics endpoint
	// renders (nil disables collection).
	Metrics *metrics.Metrics
}

func New(store interface {
	TaskReader
	WorkspaceReader
	ModelReader
	GatewayReader
	SessionReader
	TaskWriter
	SessionWriter
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
	// Record before marking: the mark makes the task eligible for destroy,
	// and a concurrent reconcile that completes it before eventf runs would
	// leave the event unwritten (the store drops events for vanished tasks).
	c.eventf(name, "Deleting", "delete requested")
	if err := c.store.MarkTaskDeleted(name, c.now()); err != nil {
		return err
	}
	return nil
}

// RequestSuspend persists the Suspending phase; reconcile drives the freeze
// on its next tick — the same async model as RequestDestroy, and the same
// single-statement discipline so it cannot clobber a VMID reconcile persists
// concurrently. expect is the phase the caller read before deciding: if
// reconcile moved the task since (typically to a terminal phase), the mark
// fails with store.ErrPhaseConflict rather than freezing a finished task
// into Suspending. The mark lives in the store, so an in-flight suspend
// survives a px-server restart.
func (c *Controller) RequestSuspend(name string, expect v1alpha1.TaskPhase) error {
	if err := c.store.MarkTaskPhase(name, expect, v1alpha1.TaskSuspending, "suspend requested"); err != nil {
		return err
	}
	c.eventf(name, "SuspendRequested", "suspend requested")
	return nil
}

// RequestResume persists the Resuming phase; reconcile drives the thaw.
// Same compare-and-set discipline as RequestSuspend.
func (c *Controller) RequestResume(name string, expect v1alpha1.TaskPhase) error {
	if err := c.store.MarkTaskPhase(name, expect, v1alpha1.TaskResuming, "resume requested"); err != nil {
		return err
	}
	c.eventf(name, "ResumeRequested", "resume requested")
	return nil
}

// eventf records one lifecycle event for a task. Failure only logs: events
// describe the transitions the controller makes, and must never gate them.
func (c *Controller) eventf(task, reason, format string, args ...any) {
	if c.Events == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if err := c.Events.RecordEvent(task, c.now(), reason, msg); err != nil {
		c.log.Warn("record event", "task", task, "reason", reason, "err", err)
	}
}

func (c *Controller) reconcileAll(ctx context.Context) {
	start := c.now()
	tasks, err := c.store.ListTasks()
	if err != nil {
		c.log.Error("list tasks", "err", err)
		return
	}
	for _, t := range tasks {
		c.reconcile(ctx, t)
	}
	c.Metrics.ObserveTick(c.now().Sub(start))
}

func (c *Controller) reconcile(ctx context.Context, t *v1alpha1.Task) {
	if t.Status.DeletionTimestamp != nil {
		c.destroyTask(ctx, t)
		return
	}

	// Records provisioned before Status.Node existed carry an empty node;
	// repair them from the cluster view before anything node-scoped runs.
	// repairNode either persists the node or fails the task, so this pass
	// runs at most once per record.
	if t.Status.Container != 0 && t.Status.Node == "" {
		c.repairNode(ctx, t)
		return
	}

	// Published ports exist only while the container runs: any other phase
	// tears the forwards down — a frozen backend means hanging connections
	// (suspend), a finished runner serves nothing (terminal). Failure keeps
	// the record's ports and retries next tick, and stops this tick's
	// lifecycle flow too: the delete/TTL paths below remove forwards again
	// on their own, and re-dialing a node that just failed costs another
	// full SSH timeout.
	if t.Status.Container != 0 && t.Status.Node != "" &&
		t.Status.Phase != v1alpha1.TaskRunning && len(t.Status.Ports) > 0 {
		if !c.teardownPorts(ctx, t) {
			return
		}
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
			c.eventf(t.Metadata.Name, "ProvisionFailed", "restart recovery: no container was recorded, task failed without one")
			c.persist(t)
			return
		}
		// A container was cloned, but Create never completed: the boot may
		// have died with the SSH session. Adopt the task only if the runner
		// actually launched; otherwise the container is a partial clone —
		// clean it up rather than spin in a fake Running.
		booted, err := c.prov.Booted(ctx, t.Status.Node, t.Status.Container)
		if err != nil {
			// Transient probe failure; retry on the next tick. A container
			// that is gone or stopped makes pct exec fail with a non-zero
			// code, which lands in the unbooted branch below.
			c.log.Warn("boot probe", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
			return
		}
		if booted {
			// A reused VMID can carry a foreign boot marker; adoption must not
			// run someone else's runner as this task. A container that is not
			// ours (by hostname) is left alone and the task fails.
			owned, oerr := c.prov.Owned(ctx, t.Metadata.Name, t.Status.Node, t.Status.Container)
			if oerr != nil {
				c.log.Error("check ownership before adoption", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", oerr)
				return
			}
			if !owned {
				c.log.Warn("adoption skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", t.Status.Container)
				c.eventf(t.Metadata.Name, "ProvisionFailed", "restart recovery: container %d is not owned by the task, left alone", t.Status.Container)
				t.Status.Container = 0
				t.Status.Phase = v1alpha1.TaskProvisionFail
				t.Status.Reason = "provisioning interrupted by restart: container is not owned by the task, left alone"
				t.Status.EndedAt = nowPtr(c.now)
				c.persist(t)
				return
			}
			t.Status.Phase = v1alpha1.TaskRunning
			now := c.now()
			t.Status.StartedAt = &now
			t.Status.Reason = ""
			c.log.Info("adopted interrupted provision", "task", t.Metadata.Name, "vmid", t.Status.Container)
			c.eventf(t.Metadata.Name, "Running", "adopted container %d on %s after restart", t.Status.Container, t.Status.Node)
			c.persist(t)
			return
		}
		if err := c.prov.DestroyOwned(ctx, t.Metadata.Name, t.Status.Node, t.Status.Container); err != nil {
			if !errors.Is(err, ErrNotOwned) {
				c.log.Error("cleanup interrupted provision", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
				return
			}
			// The VMID no longer names our clone; leave the foreign container
			// alone and fail the task.
			c.log.Warn("cleanup skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
			c.eventf(t.Metadata.Name, "ProvisionFailed", "restart recovery: container %d is not owned by the task, left alone", t.Status.Container)
			t.Status.Container = 0
			t.Status.Phase = v1alpha1.TaskProvisionFail
			t.Status.Reason = "provisioning interrupted by restart: container is not owned by the task, left alone"
			t.Status.EndedAt = nowPtr(c.now)
			c.persist(t)
			return
		}
		c.eventf(t.Metadata.Name, "ProvisionFailed", "restart recovery: container %d never booted a runner, cleaned up", t.Status.Container)
		t.Status.Container = 0
		t.Status.Phase = v1alpha1.TaskProvisionFail
		t.Status.Reason = "provisioning interrupted by restart: container never booted a runner, cleaned up"
		t.Status.EndedAt = nowPtr(c.now)
		c.log.Warn("stale provisioning recovered", "task", t.Metadata.Name, "cleaned", "partial container destroyed")
		c.persist(t)
	case v1alpha1.TaskRunning:
		c.reconcileRunning(ctx, t)
	case v1alpha1.TaskSuspending:
		c.reconcileSuspending(ctx, t)
	case v1alpha1.TaskSuspended:
		c.reconcileSuspended(ctx, t)
	case v1alpha1.TaskResuming:
		c.reconcileResuming(ctx, t)
	case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskProvisionFail:
		// Capture before the TTL gate: the container is the only source of
		// the session, and this is the first tick the task is terminal —
		// capture now and the TTL deadline can never race it.
		c.captureSession(ctx, t)
		c.cleanupAfterTTL(ctx, t)
	}
}

// repairNode fills Status.Node for records persisted before multi-node
// existed: the cluster resource view maps the recorded VMID to the node it
// lives on. One persist and the record looks exactly like a scheduled one.
func (c *Controller) repairNode(ctx context.Context, t *v1alpha1.Task) {
	node, err := c.prov.NodeOf(ctx, t.Status.Container)
	switch {
	case errors.Is(err, ErrGuestGone):
		// The container is gone while the record still claims it exists —
		// fail the task rather than wedge on a node that cannot be
		// determined.
		end := c.now()
		t.Status.EndedAt = &end
		t.Status.Phase = v1alpha1.TaskFailed
		t.Status.Reason = "container vanished from the cluster: " + err.Error()
		c.log.Warn("container vanished", "task", t.Metadata.Name, "vmid", t.Status.Container)
		c.eventf(t.Metadata.Name, "Failed", "container %d vanished from the cluster", t.Status.Container)
		c.persist(t)
	case err != nil:
		// Cluster view unavailable; retry next tick.
		c.log.Warn("repair node", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
	default:
		t.Status.Node = node
		c.log.Info("repaired missing node", "task", t.Metadata.Name, "vmid", t.Status.Container, "node", node)
		c.persist(t)
	}
}

// destroyTask destroys the container and drops the record. Until Destroy
// succeeds the record keeps its DeletionTimestamp, so a failure or a crash
// mid-destroy retries on the next tick.
func (c *Controller) destroyTask(ctx context.Context, t *v1alpha1.Task) {
	vmid := t.Status.Container
	if vmid == 0 {
		if t.Status.Phase == v1alpha1.TaskProvisioning {
			c.log.Warn("deleting task that was still provisioning with no container recorded", "task", t.Metadata.Name)
		}
		// The container never existed, so the session row — whose lifetime
		// is the task record's — can only be absent; dropTaskSession is
		// idempotent, so settle it unconditionally.
		c.dropTaskSession(t)
		if err := c.store.DeleteTask(t.Metadata.Name); err != nil {
			c.log.Error("delete task", "task", t.Metadata.Name, "err", err)
		}
		return
	}

	// Destroy is node-scoped, so the node must be known before anything else.
	// Records from before multi-node (or a crashed write) may lack it.
	if t.Status.Node == "" {
		node, err := c.prov.NodeOf(ctx, vmid)
		switch {
		case errors.Is(err, ErrGuestGone):
			// Nothing on the cluster answers to this VMID: destroy is
			// already complete as far as the guest is concerned.
			c.log.Warn("destroy: container already gone from cluster", "task", t.Metadata.Name, "vmid", vmid)
			t.Status.Container = 0
			c.persist(t)
			c.dropTaskSession(t)
			if err := c.store.DeleteTask(t.Metadata.Name); err != nil {
				c.log.Error("delete task", "task", t.Metadata.Name, "err", err)
			}
			return
		case err != nil:
			// Cluster view unavailable; retry next tick.
			c.log.Warn("destroy: repair node", "task", t.Metadata.Name, "vmid", vmid, "err", err)
			return
		default:
			t.Status.Node = node
			c.log.Info("repaired missing node", "task", t.Metadata.Name, "vmid", vmid, "node", node)
			c.persist(t)
		}
	}
	node := t.Status.Node

	// Kill the port forwards before the container: they are node processes
	// with no tie to the container's lifecycle, so destroying without them
	// would leave listeners pointing at a dead address, and the record —
	// the only thing that remembers the ports — is deleted moments later.
	// A failure here retries the whole destroy on the next tick.
	if len(t.Status.Ports) > 0 {
		if err := c.prov.RemovePorts(ctx, node, hostPortsOf(t.Status.Ports)); err != nil {
			c.log.Error("remove port forwards before destroy", "task", t.Metadata.Name, "err", err)
			return
		}
		t.Status.Ports = nil
		c.persist(t)
	}

	// Guard ownership before thawing too: a recorded VMID another container
	// now holds must not be thawed any more than destroyed. DestroyOwned
	// re-checks, which shrinks (not closes) the window between the two
	// checks — the same residual race the M3 guards accept.
	owned := true
	switch ownedRes, oerr := c.prov.Owned(ctx, t.Metadata.Name, node, vmid); {
	case oerr != nil:
		c.log.Error("destroy: check ownership", "task", t.Metadata.Name, "vmid", vmid, "err", oerr)
		return
	case !ownedRes:
		c.log.Warn("destroy skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", vmid)
		t.Status.Container = 0
		owned = false
		c.persist(t)
	default:
		// A suspended container cannot be stopped or destroyed through pct
		// while frozen: thaw first. A stopped container fails the PID lookup,
		// which is fine — only a frozen one needs the thaw. The thaw also
		// unblocks this capture: a frozen cgroup blocks pct exec, and the
		// capture must settle before the destroy below.
		if err := c.prov.Thaw(ctx, node, vmid); err != nil {
			c.log.Warn("thaw before destroy (ignored unless destroy also fails)", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		}
	}
	if owned && !c.captureSession(ctx, t) {
		// Retry next tick: destroying now would drop the container before
		// its session was settled.
		return
	}

	err := c.prov.DestroyOwned(ctx, t.Metadata.Name, node, vmid)
	switch {
	case errors.Is(err, ErrNotOwned):
		// The VMID names a container that is not ours (PVE nextid is
		// unreserved); it is not px's to destroy, so drop the record
		// rather than retry forever.
		c.log.Warn("destroy skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		t.Status.Container = 0
		c.persist(t)
	case err != nil:
		// Keep the record and retry next tick; deleting it now
		// would orphan the container with nothing left to retry.
		c.log.Error("destroy container", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		return
	default:
		t.Status.Container = 0
		c.persist(t)
	}
	c.dropTaskSession(t)
	if err := c.store.DeleteTask(t.Metadata.Name); err != nil {
		c.log.Error("delete task", "task", t.Metadata.Name, "err", err)
	}
}

// dropTaskSession drops a task's default-named session row, before the task
// record goes — if the order were reversed, a crash in between would leave
// an orphan row that no record ever reconciles again. A row under an
// explicit spec.session.name is user-owned — its lifetime is the user's,
// and other tasks may still restore from it — so it survives the task's
// deletion; the store only clears explicit=0 rows, which also keeps a
// same-named task's lifetime from touching an explicitly named capture.
func (c *Controller) dropTaskSession(t *v1alpha1.Task) {
	if t.Spec.Session != nil && t.Spec.Session.Name != "" {
		return
	}
	if err := c.store.DeleteDefaultSession(t.Metadata.Name); err != nil {
		c.log.Error("delete session", "task", t.Metadata.Name, "err", err)
	}
}

func (c *Controller) provision(ctx context.Context, t *v1alpha1.Task) {
	if ctx.Err() != nil {
		return
	}
	mounts, err := c.resolveWorkspaces(t)
	if err != nil {
		c.failProvision(t, err)
		return
	}
	model, err := c.resolveModel(t)
	if err != nil {
		c.failProvision(t, err)
		return
	}
	gw, err := c.resolveGateway(t)
	if err != nil {
		c.failProvision(t, err)
		return
	}
	session, err := c.resolveSession(t)
	if err != nil {
		c.failProvision(t, err)
		return
	}
	t.Status.Phase = v1alpha1.TaskProvisioning
	t.Status.Reason = "cloning template and starting container"
	c.eventf(t.Metadata.Name, "Provisioning", "cloning template and starting container")
	c.persist(t)

	node, err := c.prov.Schedule(ctx, t.Spec.Image)
	if err != nil {
		c.failProvision(t, fmt.Errorf("schedule: %w", err))
		return
	}
	t.Status.Node = node
	c.eventf(t.Metadata.Name, "Scheduled", "scheduled to node %s", node)

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

	if err := c.prov.Create(ctx, t, node, vmid, mounts, model, gw, session); err != nil {
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
	c.eventf(t.Metadata.Name, "Running", "runner started in container %d on %s", vmid, node)
	c.persist(t)
}

// resolveWorkspaces resolves the task's workspace references against the
// store, before any container work: a reference to a missing Workspace is a
// provision failure, not a container that boots and fails later.
func (c *Controller) resolveWorkspaces(t *v1alpha1.Task) ([]ResolvedWorkspace, error) {
	var mounts []ResolvedWorkspace
	for _, ws := range t.Spec.Workspaces {
		w, err := c.store.GetWorkspace(ws.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace %q: %w", ws.Name, err)
		}
		mounts = append(mounts, ResolvedWorkspace{
			Name:   ws.Name,
			Repo:   w.Spec.Git.Repo,
			Branch: w.Spec.Git.Branch,
		})
	}
	return mounts, nil
}

// resolveModel resolves the task's model reference (at most one) the same
// way — before any container work. A deleted Model only affects tasks
// applied after the deletion; running containers keep their injected env.
func (c *Controller) resolveModel(t *v1alpha1.Task) (*ResolvedModel, error) {
	if t.Spec.Model == "" {
		return nil, nil
	}
	m, err := c.store.GetModel(t.Spec.Model)
	if err != nil {
		return nil, fmt.Errorf("resolve model %q: %w", t.Spec.Model, err)
	}
	return &ResolvedModel{Provider: m.Spec.Provider, APIKey: m.Spec.APIKey, BaseURL: m.Spec.BaseURL}, nil
}

// resolveGateway resolves the task's gateway reference (at most one) like
// resolveModel — before any container work, so an unknown Gateway is a
// provision failure, not a container that boots fully open. A deleted Gateway
// affects tasks applied after the deletion; running containers keep the
// firewall rules set at their provision time.
func (c *Controller) resolveGateway(t *v1alpha1.Task) (*ResolvedGateway, error) {
	if t.Spec.Gateway == "" {
		return nil, nil
	}
	g, err := c.store.GetGateway(t.Spec.Gateway)
	if err != nil {
		return nil, fmt.Errorf("resolve gateway %q: %w", t.Spec.Gateway, err)
	}
	return &ResolvedGateway{Name: g.Metadata.Name, Egress: g.Spec.Egress}, nil
}

// resolveSession fetches the session archive a continueFrom reference asks
// for, before any container work — same pattern as workspaces, model and
// gateway: an unresolvable reference is a provision failure, not a container
// that boots without the session it was told to continue. A bare task name
// must reference a finished task that still holds its capture (a source with
// no captured session has nothing to continue from, a loud failure rather
// than a silently fresh session). The "session:NAME" form reads a named
// capture directly: a capture is a settled state, so no task record is
// consulted and no phase checked.
func (c *Controller) resolveSession(t *v1alpha1.Task) ([]byte, error) {
	if t.Spec.Session == nil || t.Spec.Session.ContinueFrom == "" {
		return nil, nil
	}
	sessionRef, name := v1alpha1.SplitSessionRef(t.Spec.Session.ContinueFrom)
	if sessionRef {
		data, err := c.store.GetSession(name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("session %q has no captured session", name)
			}
			return nil, fmt.Errorf("read session %q: %w", name, err)
		}
		return data, nil
	}
	src, err := c.store.GetTask(name)
	if err != nil {
		return nil, fmt.Errorf("resolve session source %q: %w", name, err)
	}
	switch src.Status.Phase {
	case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed:
	default:
		return nil, fmt.Errorf("session source %q is %s, not finished", name, src.Status.Phase)
	}
	// A bare reference names a task, but the capture lives under the
	// source's capture name — its explicit spec.session.name, or the
	// task name itself (the M8 default). Reading the bare name directly
	// would ProvisionFailed a valid capture whenever the source named
	// its session.
	cap := src.Spec.Session.CaptureName(src.Metadata.Name)
	// A source that never named its session must read a default-lifetime
	// row only: the name may also be held by an explicitly owned row —
	// often the very owner whose presence made this task's own write fail
	// with ErrSessionOwned — and serving it would restore someone else's
	// conversation under the source task's name.
	get := c.store.GetSession
	if src.Spec.Session == nil || src.Spec.Session.Name == "" {
		get = c.store.GetDefaultSession
	}
	data, err := get(cap)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("session source %q has no captured session under %q", name, cap)
		}
		return nil, fmt.Errorf("read session %q: %w", cap, err)
	}
	return data, nil
}

func (c *Controller) failProvision(t *v1alpha1.Task, err error) {
	t.Status.Phase = v1alpha1.TaskProvisionFail
	t.Status.Reason = err.Error()
	t.Status.EndedAt = nowPtr(c.now)
	c.log.Error("provision task", "task", t.Metadata.Name, "err", err)
	c.eventf(t.Metadata.Name, "ProvisionFailed", "%s", err.Error())
	c.Metrics.IncProvisionFailures()
	c.persist(t)
}

func (c *Controller) poll(ctx context.Context, t *v1alpha1.Task) {
	if t.Status.Container == 0 {
		return
	}
	code, err := c.prov.Exit(ctx, t.Status.Node, t.Status.Container)
	if err != nil {
		// The container may have died without writing an exit file (OOM,
		// node reboot); distinguish that from a transient probe failure.
		if running, rerr := c.prov.Running(ctx, t.Status.Node, t.Status.Container); rerr == nil && !running {
			end := c.now()
			t.Status.EndedAt = &end
			t.Status.Phase = v1alpha1.TaskFailed
			t.Status.Reason = "container not running and no exit file: " + err.Error()
			c.log.Warn("container died", "task", t.Metadata.Name, "vmid", t.Status.Container)
			c.eventf(t.Metadata.Name, "ContainerDied", "container not running and no exit file: %s", err)
			c.persist(t)
			// Settle the capture in the same tick the terminal phase lands:
			// a px run --continue issued the moment the phase is visible
			// would otherwise race the next tick's terminal capture and
			// resolve an empty session.
			c.captureSession(ctx, t)
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
	c.eventf(t.Metadata.Name, string(t.Status.Phase), "runner exited %d", *code)
	c.persist(t)
	// Same-tick capture: see the container-died path above. A failed
	// capture retries on the following terminal tick, unchanged.
	c.captureSession(ctx, t)
}

// reconcileRunning polls the runner, but first adopts any container found
// frozen. A container frozen out-of-band (pct on the node, a host reboot
// restoring a frozen state) would hang every pct exec — including poll's —
// until it thaws, so the freeze is claimed as a suspend before probing.
func (c *Controller) reconcileRunning(ctx context.Context, t *v1alpha1.Task) {
	frozen, err := c.prov.Frozen(ctx, t.Status.Node, t.Status.Container)
	if err == nil && frozen {
		t.Status.Phase = v1alpha1.TaskSuspended
		t.Status.Reason = "adopted container found frozen"
		c.log.Info("adopted frozen container as suspended", "task", t.Metadata.Name, "vmid", t.Status.Container)
		c.eventf(t.Metadata.Name, "Suspended", "adopted container found frozen")
		c.persist(t)
		return
	}
	if err != nil {
		// Probe failures (SSH blip, container stopped) fall through to poll,
		// which decides whether the container died.
		c.log.Warn("frozen probe", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
	}
	c.poll(ctx, t)
	// poll may have just settled the task terminal; published ports follow
	// the phase out on the next tick's teardown. Only a still-Running task
	// gets its forwards converged.
	if t.Status.Phase == v1alpha1.TaskRunning {
		c.reconcilePorts(ctx, t)
	}
}

// deadDuringSuspend settles a suspend-phase task whose container has
// stopped or vanished: the freeze probe fails for a dead guest as surely as
// for an SSH blip, and without this check the phase would retry forever and
// resume would never land (the PID the thaw path needs is gone). Same rule
// as poll's dead-container path: Failed, with EndedAt set. Returns true when
// it settled the task; callers should then not touch it further this tick.
func (c *Controller) deadDuringSuspend(ctx context.Context, t *v1alpha1.Task, during string) bool {
	running, err := c.prov.Running(ctx, t.Status.Node, t.Status.Container)
	if err != nil || running {
		return false
	}
	end := c.now()
	t.Status.EndedAt = &end
	t.Status.Phase = v1alpha1.TaskFailed
	t.Status.Reason = "container not running while " + during
	c.log.Warn("container died", "task", t.Metadata.Name, "vmid", t.Status.Container, "during", during)
	c.eventf(t.Metadata.Name, "ContainerDied", "container not running while %s", during)
	c.persist(t)
	return true
}

// reconcileSuspending drives Suspending → Suspended: freeze, then confirm on
// the next tick. Each step is idempotent, so a crash between them resumes
// correctly from the persisted phase.
func (c *Controller) reconcileSuspending(ctx context.Context, t *v1alpha1.Task) {
	frozen, err := c.prov.Frozen(ctx, t.Status.Node, t.Status.Container)
	if err != nil {
		if !c.deadDuringSuspend(ctx, t, "suspending") {
			c.log.Warn("suspend probe", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		}
		return
	}
	if !frozen {
		if err := c.prov.Freeze(ctx, t.Status.Node, t.Status.Container); err != nil {
			c.log.Error("freeze", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
			return
		}
		// Verify on the next tick rather than trusting the write.
		return
	}
	t.Status.Phase = v1alpha1.TaskSuspended
	t.Status.Reason = "container frozen"
	c.log.Info("task suspended", "task", t.Metadata.Name, "vmid", t.Status.Container)
	c.eventf(t.Metadata.Name, "Suspended", "container frozen")
	c.persist(t)
}

// reconcileSuspended keeps watching a suspended container: an external thaw
// (pct, host restart) would silently un-suspend the task, so re-freeze. It
// deliberately does not poll the runner: a frozen runner cannot report an
// exit anyway, and an exit-file check would have to spawn into the frozen
// cgroup and block until thaw.
func (c *Controller) reconcileSuspended(ctx context.Context, t *v1alpha1.Task) {
	frozen, err := c.prov.Frozen(ctx, t.Status.Node, t.Status.Container)
	if err != nil {
		if !c.deadDuringSuspend(ctx, t, "suspended") {
			c.log.Warn("suspended probe", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		}
		return
	}
	if !frozen {
		c.log.Warn("suspended container thawed externally; re-freezing", "task", t.Metadata.Name, "vmid", t.Status.Container)
		if err := c.prov.Freeze(ctx, t.Status.Node, t.Status.Container); err != nil {
			c.log.Error("re-freeze", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		}
	}
}

// reconcileResuming drives Resuming → Running: thaw, then confirm on the
// next tick. The runner was frozen mid-flight, so the resume returns the
// task to Running regardless of how long it was suspended.
func (c *Controller) reconcileResuming(ctx context.Context, t *v1alpha1.Task) {
	frozen, err := c.prov.Frozen(ctx, t.Status.Node, t.Status.Container)
	if err != nil {
		if !c.deadDuringSuspend(ctx, t, "resuming") {
			c.log.Warn("resume probe", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		}
		return
	}
	if frozen {
		if err := c.prov.Thaw(ctx, t.Status.Node, t.Status.Container); err != nil {
			c.log.Error("thaw", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
			return
		}
		// Verify on the next tick rather than trusting the write.
		return
	}
	t.Status.Phase = v1alpha1.TaskRunning
	t.Status.Reason = "container thawed"
	c.log.Info("task resumed", "task", t.Metadata.Name, "vmid", t.Status.Container)
	c.eventf(t.Metadata.Name, "Resumed", "container thawed")
	c.persist(t)
}

// captureSession settles a task's session capture before its container may
// be destroyed: on success either the archive is in the store or the task
// provably had nothing to capture, and the status flag marks it settled. It
// returns false when the capture must be retried next tick — destroy paths
// refuse to proceed then, the same rule RemovePorts follows for forwards.
// The flag is checked first, so every path after the first costs nothing.
func (c *Controller) captureSession(ctx context.Context, t *v1alpha1.Task) bool {
	if t.Status.SessionSaved {
		return true
	}
	if t.Status.Container == 0 || t.Status.Node == "" {
		// No container was ever created (ProvisionFailed, or a provision
		// crash before the VMID persisted): nothing to capture, and waiting
		// cannot change that — settle so the destroy paths can proceed.
		t.Status.SessionSaved = true
		c.persist(t)
		return true
	}
	// The destroy paths' ownership gate applies here too: a VMID that
	// outlived its record may name a stranger's container, and archiving
	// its HOME into this task's session row would leak across tasks.
	owned, err := c.prov.Owned(ctx, t.Metadata.Name, t.Status.Node, t.Status.Container)
	if err != nil {
		c.log.Error("capture session ownership check", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		return false
	}
	if !owned {
		c.log.Warn("capture session skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", t.Status.Container)
		t.Status.SessionSaved = true
		c.eventf(t.Metadata.Name, "SessionSaved", "capture skipped: container not owned by the task")
		c.persist(t)
		return true
	}
	// A container that can no longer answer exec — it died or was stopped
	// since the terminal phase landed — settles the capture too: there is
	// nothing left to exec into, and waiting cannot bring the session back.
	// Without this check the destroy paths would spin forever on a capture
	// that can no longer succeed.
	running, err := c.prov.Running(ctx, t.Status.Node, t.Status.Container)
	if err != nil {
		c.log.Error("capture session running check", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		return false
	}
	if !running {
		c.log.Warn("capture session skipped: container no longer running", "task", t.Metadata.Name, "vmid", t.Status.Container)
		t.Status.SessionSaved = true
		c.eventf(t.Metadata.Name, "SessionSaved", "capture skipped: container no longer running")
		c.persist(t)
		return true
	}
	data, err := c.prov.CaptureSession(ctx, t.Status.Node, t.Status.Container, t.Spec.Runner.User)
	tooLarge := false
	if errors.Is(err, ErrSessionTooLarge) {
		// Settled, not retryable: the archive can never fit the cap, so
		// waiting only pins the container. The log and the zero byte count
		// record the loss.
		c.log.Error("capture session over the size cap, storing nothing", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		data = nil
		tooLarge = true
	} else if err != nil {
		c.log.Error("capture session", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		return false
	}
	if len(data) > 0 {
		cap := t.Spec.Session.CaptureName(t.Metadata.Name)
		// An explicitly named capture is user-owned: its row must outlive
		// the writing task, so the store records it as explicit.
		explicit := t.Spec.Session != nil && t.Spec.Session.Name != ""
		if err := c.store.SaveSession(cap, t.Metadata.Name, data, explicit); err != nil {
			if errors.Is(err, store.ErrSessionOwned) {
				// Settled, not retryable: the name is held by a user-owned
				// capture this default write must not touch. Retrying would
				// pin the container forever, so the capture is dropped and
				// the event keeps the loss visible.
				c.log.Error("capture dropped for an explicitly owned session name", "task", t.Metadata.Name, "session", cap, "err", err)
				t.Status.SessionSaved = true
				c.persist(t)
				c.eventf(t.Metadata.Name, "SessionSaved", "capture dropped: session %s is owned by an explicitly named capture (%d bytes discarded)", cap, len(data))
				return true
			}
			c.log.Error("save session", "task", t.Metadata.Name, "session", cap, "err", err)
			return false
		}
	}
	t.Status.SessionSaved = true
	t.Status.SessionBytes = len(data)
	c.persist(t)
	switch {
	case tooLarge:
		c.eventf(t.Metadata.Name, "SessionSaved", "capture skipped: session exceeds the %d byte cap", v1alpha1.MaxSessionBytes)
	case len(data) > 0:
		c.eventf(t.Metadata.Name, "SessionSaved", "captured %d bytes into session %s", len(data), t.Spec.Session.CaptureName(t.Metadata.Name))
	default:
		c.eventf(t.Metadata.Name, "SessionSaved", "nothing to capture")
	}
	return true
}

// cleanupAfterTTL destroys a terminal task's container once its TTL has
// passed, leaving the finished record in place.
func (c *Controller) cleanupAfterTTL(ctx context.Context, t *v1alpha1.Task) {
	// Same rule as destroyTask: no container may be destroyed while its
	// forwards still listen — the terminal teardown ran on the previous
	// tick's central pass, but if it failed, this is the last gate before
	// the record loses the port list.
	if len(t.Status.Ports) > 0 {
		if err := c.prov.RemovePorts(ctx, t.Status.Node, hostPortsOf(t.Status.Ports)); err != nil {
			c.log.Error("remove port forwards before ttl destroy", "task", t.Metadata.Name, "err", err)
			return
		}
		t.Status.Ports = nil
		c.persist(t)
	}
	ttl := t.Spec.TTLSecondsAfterFinished
	if ttl <= 0 || t.Status.Container == 0 || t.Status.EndedAt == nil {
		return
	}
	deadline := t.Status.EndedAt.Add(time.Duration(ttl) * time.Second)
	if c.now().Before(deadline) {
		return
	}
	vmid := t.Status.Container
	// Last gate before the container goes: if the terminal-tick capture is
	// still unsettled (a failed SSH, a px-server crash between ticks), the
	// destroy waits for the next tick rather than dropping the session.
	if !c.captureSession(ctx, t) {
		return
	}
	err := c.prov.DestroyOwned(ctx, t.Metadata.Name, t.Status.Node, vmid)
	switch {
	case errors.Is(err, ErrNotOwned):
		c.log.Warn("ttl destroy skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		t.Status.Container = 0
		t.Status.Reason += " (container not owned, left alone after TTL)"
		c.persist(t)
		return
	case err != nil:
		c.log.Error("ttl destroy", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		return
	}
	t.Status.Container = 0
	t.Status.Reason += " (container cleaned up after TTL)"
	c.eventf(t.Metadata.Name, "TTLDeleted", "container %d cleaned up after TTL", vmid)
	c.persist(t)
}

// reconcilePorts converges a Running task's published ports to spec.ports:
// every entry needs a resolved hostPort (explicit, already assigned, or
// freshly allocated) and a node-side socat matching it. The resolved
// mapping is persisted before the node is touched — a crash between the
// two leaves a record that still names its ports, so a px-server restart
// rebuilds the forwards from the store alone.
func (c *Controller) reconcilePorts(ctx context.Context, t *v1alpha1.Task) {
	if len(t.Spec.Ports) == 0 {
		if len(t.Status.Ports) > 0 {
			c.teardownPorts(ctx, t)
		}
		return
	}
	resolved := make(map[string]int, len(t.Status.Ports))
	for _, st := range t.Status.Ports {
		resolved[st.Name] = st.HostPort
	}
	want := make([]v1alpha1.PortStatus, 0, len(t.Spec.Ports))
	var fwds []PortForward
	var claimed map[int]string
	for _, sp := range t.Spec.Ports {
		hp := sp.HostPort
		if hp == 0 {
			hp = resolved[sp.Name]
		}
		if hp == 0 {
			if claimed == nil {
				tasks, err := c.store.ListTasks()
				if err != nil {
					c.log.Error("port allocation: list tasks", "task", t.Metadata.Name, "err", err)
					return
				}
				claimed = v1alpha1.ClaimedHostPorts(tasks, t.Metadata.Name)
				// A later explicit entry in this same spec also claims its
				// hostPort: the walk below has not reached it yet, and an
				// auto assignment must not land on it.
				for _, other := range t.Spec.Ports {
					if other.HostPort != 0 {
						claimed[other.HostPort] = t.Metadata.Name
					}
				}
			}
			hp = allocateHostPort(claimed, fwds)
			if hp == 0 {
				c.log.Error("port allocation: px host port range exhausted", "task", t.Metadata.Name)
				return
			}
		}
		want = append(want, v1alpha1.PortStatus{Name: sp.Name, Port: sp.Port, HostPort: hp})
		fwds = append(fwds, PortForward{HostPort: hp, Port: sp.Port})
	}
	// Entries the new mapping no longer names would keep their listeners
	// alive on the node — sweep them before publishing, and bail on
	// failure so the next tick retries with the record unchanged.
	kept := make(map[int]bool, len(want))
	for _, w := range want {
		kept[w.HostPort] = true
	}
	var stale []int
	for _, st := range t.Status.Ports {
		if !kept[st.HostPort] {
			stale = append(stale, st.HostPort)
		}
	}
	if len(stale) > 0 {
		if err := c.prov.RemovePorts(ctx, t.Status.Node, stale); err != nil {
			c.log.Warn("remove stale port forwards", "task", t.Metadata.Name, "hostPorts", stale, "err", err)
			return
		}
	}
	if !portsEqual(want, t.Status.Ports) {
		t.Status.Ports = want
		c.log.Info("ports published", "task", t.Metadata.Name, "vmid", t.Status.Container, "ports", hostPortsOf(t.Status.Ports))
		// Persist before the node is touched: the record must name its
		// ports even if this process dies mid-convergence.
		c.persist(t)
	}
	res, err := c.prov.EnsurePorts(ctx, t.Status.Node, t.Status.Container, fwds)
	if err != nil {
		c.log.Warn("port forwards", "task", t.Metadata.Name, "vmid", t.Status.Container, "err", err)
		return
	}
	for _, hp := range res.Failed {
		c.log.Warn("port forward failed to start", "task", t.Metadata.Name, "hostPort", hp, "ctIP", res.CTIP)
	}
}

// portsEqual compares two resolved port mappings entry by entry. Entries
// follow spec order on both sides — status lists are only ever written
// from the spec walk.
func portsEqual(a, b []v1alpha1.PortStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// teardownPorts removes a task's node-side forwards and drops the record's
// port list. A failure keeps the list so the next tick retries — the
// delete/TTL paths gate on it being empty before destroying anything, and
// the reconcile loop halts a task's lifecycle flow until it returns true.
func (c *Controller) teardownPorts(ctx context.Context, t *v1alpha1.Task) bool {
	if len(t.Status.Ports) == 0 {
		return true
	}
	if err := c.prov.RemovePorts(ctx, t.Status.Node, hostPortsOf(t.Status.Ports)); err != nil {
		c.log.Error("remove port forwards", "task", t.Metadata.Name, "err", err)
		return false
	}
	t.Status.Ports = nil
	c.persist(t)
	return true
}

// allocateHostPort returns the lowest host port in the px range that no
// other task claims and this task's own working set does not use yet, or
// 0 when the range is exhausted. The reconciler is serial, so the claimed
// snapshot cannot race another allocation.
func allocateHostPort(claimed map[int]string, fwds []PortForward) int {
	taken := make(map[int]bool, len(fwds))
	for _, f := range fwds {
		taken[f.HostPort] = true
	}
	for p := v1alpha1.HostPortMin; p <= v1alpha1.HostPortMax; p++ {
		if claimed[p] == "" && !taken[p] {
			return p
		}
	}
	return 0
}

func hostPortsOf(ps []v1alpha1.PortStatus) []int {
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.HostPort)
	}
	return out
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
