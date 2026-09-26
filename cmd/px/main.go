// px is the kubectl-style CLI for the px control plane.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

var (
	serverURL = "http://127.0.0.1:7420"
	apiToken  string
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&serverURL, "server", envOr("PX_SERVER", "http://127.0.0.1:7420"), "px-server URL")
	fs.StringVar(&apiToken, "token", envOr("PX_TOKEN", ""), "API bearer token (env PX_TOKEN; px-server enables auth with -token-file)")
	var err error
	switch cmd {
	case "apply":
		err = cmdApply(fs, args)
	case "run":
		err = cmdRun(fs, args)
	case "get":
		err = cmdGet(fs, args)
	case "describe":
		err = cmdDescribe(fs, args)
	case "logs":
		err = cmdLogs(fs, args)
	case "exec":
		err = cmdExec(fs, args)
	case "watch":
		err = cmdWatch(fs, args)
	case "delete":
		err = cmdDelete(fs, args)
	case "suspend":
		err = cmdSuspendResume(fs, args, "suspend")
	case "resume":
		err = cmdSuspendResume(fs, args, "resume")
	case "version":
		fmt.Println("px v0.0.1 (px.io/v1alpha1)")
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdApply(fs *flag.FlagSet, args []string) error {
	file := fs.String("f", "", "manifest file ('-' for stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("usage: px apply -f <file|->")
	}
	var body []byte
	var err error
	if *file == "-" {
		body, err = io.ReadAll(os.Stdin)
	} else {
		body, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	var out struct {
		Results []string `json:"results"`
		Error   string   `json:"error"`
	}
	if err := doJSON(http.MethodPost, "/v1/apply", body, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return fmt.Errorf("%s", out.Error)
	}
	for _, r := range out.Results {
		fmt.Println(r)
	}
	return nil
}

// wsList collects repeated -workspace NAME[=GOAL] flags.
type wsList []string

func (w *wsList) String() string { return strings.Join(*w, ",") }
func (w *wsList) Set(v string) error {
	*w = append(*w, v)
	return nil
}

// defaultAgentCommand drives the baked-in Claude Code CLI against the
// runner's GOAL env. The sandbox is the isolation boundary (LXC + optional
// Gateway egress allowlist), so the CLI's own permission prompts are
// skipped — there is no human inside the container to answer them.
// IS_SANDBOX=1 is the CLI's declared escape hatch for exactly that
// arrangement: runners exec as root, and without it the CLI refuses
// --dangerously-skip-permissions under root/sudo outright.
var defaultAgentCommand = []string{"sh", "-c", `IS_SANDBOX=1 claude --dangerously-skip-permissions -p "$GOAL"`}

// continueAgentCommand is defaultAgentCommand's variant for a continued
// session: --continue resumes the most recent conversation in the restored
// ~/.claude/projects, and the goal arrives as the next user turn.
var continueAgentCommand = []string{"sh", "-c", `IS_SANDBOX=1 claude --dangerously-skip-permissions --continue -p "$GOAL"`}

// splitRunArgs splits args at the first bare "--": everything before it
// goes to flag parsing, everything after is the explicit runner command.
// The flag package stops at the first non-flag arg, so flags must precede
// the goal.
func splitRunArgs(args []string) (flags, cmd []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// cmdRun is client-side sugar over apply + logs + get task: it builds a
// Task manifest from flags, applies it, and (unless -no-wait) follows the
// runner's log to a terminal phase, exiting with the task's exit code.
// Ctrl-C detaches — the task keeps running server-side.
func cmdRun(fs *flag.FlagSet, args []string) error {
	model := fs.String("model", "", "Model resource name (LLM credentials for the runner)")
	var wss wsList
	fs.Var(&wss, "workspace", "Workspace reference NAME[=GOAL], repeatable")
	gateway := fs.String("gateway", "", "Gateway resource name (egress allowlist)")
	image := fs.String("image", "px-agent-debian12", "LXC template to clone")
	name := fs.String("name", "", "task name (default agent-<epoch>-<rand>)")
	ttl := fs.Int("ttl", 0, "TTLSecondsAfterFinished (0 = keep until deleted)")
	cores := fs.Int("cores", 0, "CPU cores (0 = template default)")
	memory := fs.Int("memory", 0, "memory MB (0 = template default)")
	noWait := fs.Bool("no-wait", false, "return immediately after apply")
	continueFrom := fs.String("continue", "", "continue the finished task NAME's agent session, copying its spec")
	flags, cmd := splitRunArgs(args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: px run GOAL [-model NAME] [-workspace NAME[=GOAL]]... [-gateway NAME] [-continue TASK] [-- COMMAND...]")
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected arguments after the goal: %q (quote the goal; flags must precede it)", rest[1:])
	}
	goal := rest[0]

	// -continue copies these from the source spec, so re-specifying one is
	// a contradiction rather than an override. Detection uses flag.Visit —
	// only flags the user actually set — so "-cores 0" is a rejection too,
	// not a silent no-op the copy would clobber. Name and TTL stay local by
	// design (a new task may well want a different TTL).
	var src *v1alpha1.Task
	if *continueFrom != "" {
		copied := map[string]bool{"image": true, "model": true, "workspace": true, "gateway": true, "cores": true, "memory": true}
		overridden := make([]string, 0, 7)
		fs.Visit(func(f *flag.Flag) {
			if copied[f.Name] {
				overridden = append(overridden, "-"+f.Name)
			}
		})
		if len(cmd) > 0 {
			overridden = append(overridden, "-- COMMAND")
		}
		if len(overridden) > 0 {
			return fmt.Errorf("-continue already copies image, model, gateway, workspaces and resources from task %s; drop %s",
				*continueFrom, strings.Join(overridden, ", "))
		}
		if err := doJSON(http.MethodGet, "/v1/tasks/"+*continueFrom, nil, &src); err != nil {
			return fmt.Errorf("read task %s to continue: %w", *continueFrom, err)
		}
	}

	if len(cmd) == 0 {
		cmd = defaultAgentCommand
	}
	if *name == "" {
		*name = fmt.Sprintf("agent-%d-%08x", time.Now().Unix(), rand.Intn(0xffffffff))
	}
	t := &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: *name},
		Spec: v1alpha1.TaskSpec{
			Image: *image,
			Goal:  goal,
			Runner: v1alpha1.RunnerSpec{
				Command: cmd,
			},
			Resources:               v1alpha1.Resources{Cores: *cores, MemoryMB: *memory},
			TTLSecondsAfterFinished: *ttl,
			Model:                   *model,
			Gateway:                 *gateway,
		},
	}
	for _, ref := range wss {
		n, g, _ := strings.Cut(ref, "=")
		t.Spec.Workspaces = append(t.Spec.Workspaces, v1alpha1.TaskWorkspace{Name: n, Goal: g})
	}
	if src != nil {
		// Carry the source spec wholesale, then replace only what defines
		// this run: a new goal, the session reference, and the locally
		// chosen TTL (-ttl stays meaningful with -continue). Ports
		// deliberately do not survive the copy — the follow-up is a fresh
		// task, not a re-run of the source's exposure policy.
		ttl := t.Spec.TTLSecondsAfterFinished
		t.Spec = src.Spec
		t.Spec.Goal = goal
		t.Spec.TTLSecondsAfterFinished = ttl
		t.Spec.Ports = nil
		t.Spec.Session = &v1alpha1.SessionSpec{ContinueFrom: *continueFrom}
		if slices.Equal(t.Spec.Runner.Command, defaultAgentCommand) {
			t.Spec.Runner.Command = continueAgentCommand
		}
	}
	body, err := yaml.Marshal(t)
	if err != nil {
		return err
	}
	var out struct {
		Results []string `json:"results"`
		Error   string   `json:"error"`
	}
	if err := doJSON(http.MethodPost, "/v1/apply", body, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return fmt.Errorf("%s", out.Error)
	}
	fmt.Printf("task.px.io/%s applied\n", *name)
	if *noWait {
		return nil
	}
	return followRun(*name)
}

// followRun polls the task status and the runner's log, echoing new log
// output and phase transitions, and exits with the task's own exit code
// once it reaches a terminal phase (a Failed task with a zero runner code
// still exits 1 — the phase is the signal, not the number).
func followRun(name string) error {
	var last, phase string
	printLogs := func() {
		out, blocked, err := taskLogs(name)
		if err != nil || blocked {
			return // best-effort: the status read decides the exit
		}
		if out != last {
			fmt.Print(strings.TrimPrefix(out, last))
			last = out
		}
	}
	for {
		var t *v1alpha1.Task
		if err := doJSON(http.MethodGet, "/v1/tasks/"+name, nil, &t); err != nil {
			return err
		}
		if ph := string(t.Status.Phase); ph != phase {
			if phase != "" {
				fmt.Printf("task.px.io/%s %s -> %s\n", name, phase, ph)
			} else {
				fmt.Printf("task.px.io/%s %s\n", name, ph)
			}
			phase = ph
		}
		// The logs endpoint serves a "(container gone...)" placeholder
		// before the container exists — poll the status first and read
		// logs only once there is something to read from.
		if t.Status.Container > 0 {
			printLogs()
		}
		if isTerminal(t.Status.Phase) {
			// Output written between the log read and this status read
			// would be lost — the final read usually carries the run's
			// last words, so read once more before exiting.
			printLogs()
			switch t.Status.Phase {
			case v1alpha1.TaskFailed:
				if t.Status.ExitCode != 0 {
					os.Exit(t.Status.ExitCode)
				}
				os.Exit(1)
			case v1alpha1.TaskProvisionFail:
				os.Exit(1)
			}
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// taskLogs fetches the runner log. A 409 means the endpoint refuses the
// read right now — the container is frozen (an external suspend), and
// reading logs inside a frozen cgroup would hang — which is a skip, not
// a follow failure: the loop keeps polling the status and resumes log
// reads after resume.
func taskLogs(name string) (out string, blocked bool, err error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(serverURL, "/")+"/v1/tasks/"+name+"/logs", nil)
	if err != nil {
		return "", false, err
	}
	resp, err := do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return "", true, nil
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return "", false, fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, err
	}
	return string(data), false, nil
}

func cmdGet(fs *flag.FlagSet, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: px get tasks|workspaces|models|gateways|templates")
	}
	switch args[0] {
	case "tasks":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var tasks []*v1alpha1.Task
		if err := doJSON(http.MethodGet, "/v1/tasks", nil, &tasks); err != nil {
			return err
		}
		fmt.Printf("%-24s %-16s %-12s %-8s %s\n", "NAME", "PHASE", "NODE", "CT", "AGE PORTS")
		for _, t := range tasks {
			node := t.Status.Node
			if node == "" {
				node = "-"
			}
			ports := "-"
			if len(t.Status.Ports) > 0 {
				var parts []string
				for _, p := range t.Status.Ports {
					parts = append(parts, fmt.Sprintf("%s:%d->%d", p.Name, p.HostPort, p.Port))
				}
				ports = strings.Join(parts, ",")
			}
			fmt.Printf("%-24s %-16s %-12s %-8d %s %s\n",
				t.Metadata.Name, t.Status.Phase, node, t.Status.Container, age(t.Status.StartedAt), ports)
		}
		return nil
	case "workspaces":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var wss []*v1alpha1.Workspace
		if err := doJSON(http.MethodGet, "/v1/workspaces", nil, &wss); err != nil {
			return err
		}
		fmt.Printf("%-24s %s\n", "NAME", "GIT")
		for _, ws := range wss {
			git := ws.Spec.Git.Repo
			if ws.Spec.Git.Branch != "" {
				git += "@" + ws.Spec.Git.Branch
			}
			fmt.Printf("%-24s %s\n", ws.Metadata.Name, git)
		}
		return nil
	case "models":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var models []*v1alpha1.Model
		if err := doJSON(http.MethodGet, "/v1/models", nil, &models); err != nil {
			return err
		}
		fmt.Printf("%-24s %-12s %s\n", "NAME", "PROVIDER", "BASEURL")
		for _, m := range models {
			fmt.Printf("%-24s %-12s %s\n", m.Metadata.Name, m.Spec.Provider, m.Spec.BaseURL)
		}
		return nil
	case "gateways":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var gws []*v1alpha1.Gateway
		if err := doJSON(http.MethodGet, "/v1/gateways", nil, &gws); err != nil {
			return err
		}
		fmt.Printf("%-24s %s\n", "NAME", "EGRESS")
		for _, g := range gws {
			rules := make([]string, 0, len(g.Spec.Egress))
			for _, r := range g.Spec.Egress {
				rule := r.CIDR
				if r.Ports != "" {
					rule += ":" + r.Ports
				}
				if r.Proto != "" {
					rule += "/" + r.Proto
				}
				rules = append(rules, rule)
			}
			fmt.Printf("%-24s %s\n", g.Metadata.Name, strings.Join(rules, ","))
		}
		return nil
	case "templates":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		tmpls, err := fetchTemplates()
		if err != nil {
			return err
		}
		fmt.Printf("%-28s %-8s %-12s %-6s %s\n", "NAME", "VMID", "NODE", "PX-OK", "MISSING")
		for _, t := range tmpls {
			missing := "-"
			if len(t.Missing) > 0 {
				missing = strings.Join(t.Missing, ",")
			}
			fmt.Printf("%-28s %-8d %-12s %-6t %s\n", t.Name, t.VMID, t.Node, t.PxOK, missing)
		}
		return nil
	default:
		return fmt.Errorf("usage: px get tasks|workspaces|models|gateways|templates")
	}
}

// fetchTemplates backs both `px get templates` and `px describe template`:
// one endpoint, client-side name filter — the server computes the whole
// listing anyway and a node holds a handful of templates at most.
func fetchTemplates() ([]*v1alpha1.Template, error) {
	var tmpls []*v1alpha1.Template
	if err := doJSON(http.MethodGet, "/v1/templates", nil, &tmpls); err != nil {
		return nil, err
	}
	return tmpls, nil
}

// popName finds the first non-flag argument (the object NAME) and returns it
// with the remaining args, so flags work before or after the name.
func popName(args []string) (string, []string, error) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := append(append([]string{}, args[:i]...), args[i+1:]...)
			return a, rest, nil
		}
	}
	return "", args, fmt.Errorf("missing NAME")
}

func cmdDescribe(fs *flag.FlagSet, args []string) error {
	kind := "task" // bare NAME is treated as a task
	if len(args) > 0 && (args[0] == "task" || args[0] == "workspace" || args[0] == "model" || args[0] == "gateway" || args[0] == "template") {
		kind = args[0]
		args = args[1:]
	}
	name, rest, err := popName(args)
	if err != nil {
		return fmt.Errorf("usage: px describe task NAME | describe workspace NAME | describe model NAME | describe gateway NAME | describe template NAME")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch kind {
	case "task":
		var t *v1alpha1.Task
		if err := doJSON(http.MethodGet, "/v1/tasks/"+name, nil, &t); err != nil {
			return err
		}
		printJSONIndent(t)
	case "workspace":
		var ws *v1alpha1.Workspace
		if err := doJSON(http.MethodGet, "/v1/workspaces/"+name, nil, &ws); err != nil {
			return err
		}
		printJSONIndent(ws)
	case "model":
		var m *v1alpha1.Model
		if err := doJSON(http.MethodGet, "/v1/models/"+name, nil, &m); err != nil {
			return err
		}
		printJSONIndent(m)
	case "gateway":
		var g *v1alpha1.Gateway
		if err := doJSON(http.MethodGet, "/v1/gateways/"+name, nil, &g); err != nil {
			return err
		}
		printJSONIndent(g)
	case "template":
		nodeName, vmidPart, hasVMID := strings.Cut(name, "@")
		var vmid int
		if hasVMID {
			var err error
			if vmid, err = strconv.Atoi(vmidPart); err != nil {
				return fmt.Errorf("bad template selector %q (want name or name@vmid)", name)
			}
		}
		tmpls, err := fetchTemplates()
		if err != nil {
			return err
		}
		var matches []*v1alpha1.Template
		for _, t := range tmpls {
			if t.Name == nodeName && (!hasVMID || t.VMID == vmid) {
				matches = append(matches, t)
			}
		}
		switch {
		case len(matches) == 0:
			return fmt.Errorf("template %q not found (see px get templates)", name)
		case len(matches) > 1:
			var at []string
			for _, m := range matches {
				at = append(at, fmt.Sprintf("%s@%d", m.Name, m.VMID))
			}
			return fmt.Errorf("template %q exists on several nodes (%s); pick one with px describe template name@vmid",
				nodeName, strings.Join(at, ", "))
		}
		printJSONIndent(matches[0])
	}
	return nil
}

// printJSONIndent renders describe output without HTML escaping: the Model
// key placeholder must show as <redacted>, but json.MarshalIndent escapes
// < and >, and describe output re-applied as YAML would then no longer
// match the placeholder guard.
func printJSONIndent(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func cmdLogs(fs *flag.FlagSet, args []string) error {
	follow := fs.Bool("f", false, "follow")
	name, rest, err := popName(args)
	if err != nil {
		return fmt.Errorf("usage: px logs NAME [-f] [-server URL]")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if !*follow {
		return stream(http.MethodGet, "/v1/tasks/"+name+"/logs", os.Stdout)
	}
	// Follow: poll and print the tail, stop once the task is terminal.
	var last string
	for {
		var buf strings.Builder
		if err := stream(http.MethodGet, "/v1/tasks/"+name+"/logs", &buf); err != nil {
			return err
		}
		if out := buf.String(); out != last {
			fmt.Print(strings.TrimPrefix(out, last))
			last = out
		}
		var t *v1alpha1.Task
		if err := doJSON(http.MethodGet, "/v1/tasks/"+name, nil, &t); err == nil {
			switch t.Status.Phase {
			case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskProvisionFail:
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func cmdExec(fs *flag.FlagSet, args []string) error {
	name, rest, err := popName(args)
	if err != nil || len(rest) == 0 {
		return fmt.Errorf("usage: px exec NAME -- COMMAND [ARG...]")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	// flag parsing stops at "--" (and drops it), so what follows arrives here
	// verbatim — the command must not be mangled into flags.
	argv := fs.Args()
	if len(argv) == 0 {
		return fmt.Errorf("usage: px exec NAME -- COMMAND [ARG...]")
	}
	body, err := json.Marshal(map[string]any{"command": argv})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/v1/tasks/"+name+"/exec", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	// application/json, not doJSON's apply-style YAML content type.
	req.Header.Set("Content-Type", "application/json")
	resp, err := do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error != "" {
			return fmt.Errorf("%d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		ExitCode  int    `json:"exitCode"`
		Truncated bool   `json:"truncated"`
		PctFailed bool   `json:"pctFailed"`
		PctReason string `json:"pctReason"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	_, _ = os.Stdout.WriteString(out.Stdout)
	_, _ = os.Stderr.WriteString(out.Stderr)
	if out.Truncated {
		fmt.Fprintln(os.Stderr, "(px exec: output truncated at 1 MiB per stream)")
	}
	// A pct-level refusal means the command never ran: say which layer
	// failed instead of exiting with an unrelated code.
	if out.PctFailed {
		fmt.Fprintf(os.Stderr, "px exec: pct refused the command: %s\n", out.PctReason)
		os.Exit(1)
	}
	// Like kubectl, exit with the command's code. Everything above is already
	// written synchronously, so skipping the deferred close hurts nothing.
	if out.ExitCode != 0 {
		os.Exit(out.ExitCode)
	}
	return nil
}

func cmdDelete(fs *flag.FlagSet, args []string) error {
	kind := "task"
	if len(args) > 0 && (args[0] == "task" || args[0] == "model" || args[0] == "gateway" || args[0] == "workspace") {
		kind = args[0]
		args = args[1:]
	}
	name, rest, err := popName(args)
	if err != nil {
		return fmt.Errorf("usage: px delete task NAME | delete model NAME | delete gateway NAME | delete workspace NAME")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch kind {
	case "task":
		var out map[string]string
		if err := doJSON(http.MethodDelete, "/v1/tasks/"+name, nil, &out); err != nil {
			return err
		}
		fmt.Printf("task.px.io/%s deleting\n", name)
	case "model":
		var out map[string]string
		if err := doJSON(http.MethodDelete, "/v1/models/"+name, nil, &out); err != nil {
			return err
		}
		fmt.Printf("model.px.io/%s deleted\n", name)
	case "gateway":
		var out map[string]string
		if err := doJSON(http.MethodDelete, "/v1/gateways/"+name, nil, &out); err != nil {
			return err
		}
		fmt.Printf("gateway.px.io/%s deleted\n", name)
	case "workspace":
		var out map[string]string
		if err := doJSON(http.MethodDelete, "/v1/workspaces/"+name, nil, &out); err != nil {
			return err
		}
		fmt.Printf("workspace.px.io/%s deleted\n", name)
	}
	return nil
}

// cmdSuspendResume posts the phase-flip request; the server gates on the
// current phase (409 for the wrong one), so the CLI is a thin pass-through.
// Accepts both `px suspend task NAME` and `px suspend NAME`.
func cmdSuspendResume(fs *flag.FlagSet, args []string, op string) error {
	if len(args) > 0 && args[0] == "task" {
		args = args[1:]
	}
	name, rest, err := popName(args)
	if err != nil {
		return fmt.Errorf("usage: px %s task NAME", op)
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	var out map[string]string
	if err := doJSON(http.MethodPost, "/v1/tasks/"+name+"/"+op, nil, &out); err != nil {
		return err
	}
	fmt.Printf("task.px.io/%s %s\n", name, out["status"])
	return nil
}

func cmdWatch(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(serverURL, "/")+"/v1/watch", nil)
	if err != nil {
		return err
	}
	resp, err := do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	// Print phase transitions only, and exit once every task is terminal or
	// marked for deletion — so `px watch` doubles as the scripted wait for a
	// task to finish. Frames are decoded straight off the stream: a snapshot
	// of many tasks can exceed any fixed line size, and a stream end other
	// than a clean EOF surfaces as an error, so a dead server never looks
	// like "all tasks done".
	dec := json.NewDecoder(resp.Body)
	phases := map[string]string{}
	for {
		var snap struct {
			Tasks []*v1alpha1.Task `json:"tasks"`
		}
		if err := dec.Decode(&snap); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		now := map[string]string{}
		// An empty snapshot (nothing applied yet) is not "everything done" —
		// stay connected so a watch started before `px apply` still waits.
		done := len(snap.Tasks) > 0
		for _, t := range snap.Tasks {
			ph := string(t.Status.Phase)
			now[t.Metadata.Name] = ph
			if old, ok := phases[t.Metadata.Name]; !ok {
				fmt.Printf("%s %s\n", t.Metadata.Name, ph)
			} else if old != ph {
				fmt.Printf("%s %s -> %s\n", t.Metadata.Name, old, ph)
			}
			if t.Status.DeletionTimestamp == nil && !isTerminal(t.Status.Phase) {
				done = false
			}
		}
		for name, old := range phases {
			if _, ok := now[name]; !ok {
				fmt.Printf("%s %s -> Deleted\n", name, old)
			}
		}
		phases = now
		if done {
			return nil
		}
	}
}

func isTerminal(ph v1alpha1.TaskPhase) bool {
	switch ph {
	case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskProvisionFail:
		return true
	}
	return false
}

func doJSON(method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, strings.TrimRight(serverURL, "/")+path, bodyReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/yaml")
	}
	resp, err := do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error != "" {
			return fmt.Errorf("%d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// do sends req with the API token attached and turns 401 into a hint about
// PX_TOKEN, so a server started with -token-file is easy to diagnose.
func do(req *http.Request) (*http.Response, error) {
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w (is px-server running?)", serverURL, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("401: unauthorized (set PX_TOKEN or -token to the value of px-server's -token-file)")
	}
	return resp, nil
}

func stream(method, path string, w io.Writer) error {
	req, err := http.NewRequest(method, strings.TrimRight(serverURL, "/")+path, nil)
	if err != nil {
		return err
	}
	resp, err := do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func bodyReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return strings.NewReader(string(b))
}

func age(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Since(*t).Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	return d.Truncate(time.Minute).String()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprint(os.Stderr, `px — lightweight agent orchestration on Proxmox VE

Usage:
  px apply -f <file|->            Apply a YAML manifest
  px run GOAL [-model NAME]       Launch an agent task from flags: the goal is
      [-workspace NAME[=GOAL]]... the runner's instruction (GOAL env); flags
      [-gateway NAME] [-image N]  precede the goal; a bare double dash after
      [-name N] [-ttl S]          it replaces the default Claude Code runner
      [-cores N] [-memory N]      command; Ctrl-C detaches and the task keeps
      [-no-wait]                  running
  px get tasks                    List tasks
  px get workspaces               List workspaces
  px get models                   List models (API keys redacted)
  px get gateways                 List gateways with their egress rules
  px get templates                List LXC templates on the node with px's
                                  compatibility verdict
  px describe task NAME           Show one task as JSON
  px describe workspace NAME      Show one workspace as JSON
  px describe model NAME          Show one model as JSON (API key redacted)
  px describe gateway NAME        Show one gateway as JSON
  px describe template NAME       Show one template's facts and verdict
  px logs NAME [-f]               Stream runner logs
  px exec NAME -- CMD [ARG...]    Run a command in a running task's container
                                  (exits with the command's exit code)
  px watch                        Stream task phase transitions
  px delete task NAME             Delete a task and its container
  px delete model NAME            Delete a model
  px delete gateway NAME          Delete a gateway
  px delete workspace NAME        Delete a workspace
  px suspend task NAME            Suspend a running task (freezes its container)
  px resume task NAME             Resume a suspended task
  px version                      Show version

Flags:
  -server URL    px-server URL (env PX_SERVER, default http://127.0.0.1:7420)
  -token TOK     API bearer token (env PX_TOKEN; required when px-server runs with -token-file)
`)
}
