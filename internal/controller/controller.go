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

type Controller struct {
	store interface {
		TaskReader
		WorkspaceReader
		ModelReader
		GatewayReader
		TaskWriter
	}
	prov Provisioner
	log  *slog.Logger
	Tick time.Duration // reconcile interval
	now  func() time.Time
}

func New(store interface {
	TaskReader
	WorkspaceReader
	ModelReader
	GatewayReader
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

// RequestSuspend persists the Suspending phase; reconcile drives the freeze
// on its next tick — the same async model as RequestDestroy, and the same
// single-statement discipline so it cannot clobber a VMID reconcile persists
// concurrently. expect is the phase the caller read before deciding: if
// reconcile moved the task since (typically to a terminal phase), the mark
// fails with store.ErrPhaseConflict rather than freezing a finished task
// into Suspending. The mark lives in the store, so an in-flight suspend
// survives a px-server restart.
func (c *Controller) RequestSuspend(name string, expect v1alpha1.TaskPhase) error {
	return c.store.MarkTaskPhase(name, expect, v1alpha1.TaskSuspending, "suspend requested")
}

// RequestResume persists the Resuming phase; reconcile drives the thaw.
// Same compare-and-set discipline as RequestSuspend.
func (c *Controller) RequestResume(name string, expect v1alpha1.TaskPhase) error {
	return c.store.MarkTaskPhase(name, expect, v1alpha1.TaskResuming, "resume requested")
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
			t.Status.Container = 0
			t.Status.Phase = v1alpha1.TaskProvisionFail
			t.Status.Reason = "provisioning interrupted by restart: container is not owned by the task, left alone"
			t.Status.EndedAt = nowPtr(c.now)
			c.persist(t)
			return
		}
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
	switch owned, oerr := c.prov.Owned(ctx, t.Metadata.Name, node, vmid); {
	case oerr != nil:
		c.log.Error("destroy: check ownership", "task", t.Metadata.Name, "vmid", vmid, "err", oerr)
		return
	case !owned:
		c.log.Warn("destroy skipped: container is not owned by the task", "task", t.Metadata.Name, "vmid", vmid)
		t.Status.Container = 0
		c.persist(t)
	default:
		// A suspended container cannot be stopped or destroyed through pct
		// while frozen: thaw first. A stopped container fails the PID lookup,
		// which is fine — only a frozen one needs the thaw.
		if err := c.prov.Thaw(ctx, node, vmid); err != nil {
			c.log.Warn("thaw before destroy (ignored unless destroy also fails)", "task", t.Metadata.Name, "vmid", vmid, "err", err)
		}
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
	if err := c.store.DeleteTask(t.Metadata.Name); err != nil {
		c.log.Error("delete task", "task", t.Metadata.Name, "err", err)
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
	t.Status.Phase = v1alpha1.TaskProvisioning
	t.Status.Reason = "cloning template and starting container"
	c.persist(t)

	node, err := c.prov.Schedule(ctx, t.Spec.Image)
	if err != nil {
		c.failProvision(t, fmt.Errorf("schedule: %w", err))
		return
	}
	t.Status.Node = node

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

	if err := c.prov.Create(ctx, t, node, vmid, mounts, model, gw); err != nil {
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
	c.persist(t)
}

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
