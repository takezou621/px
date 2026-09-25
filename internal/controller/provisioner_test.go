package controller

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/proxmox"
	"github.com/kawai/px/internal/sshexec"
)

func testProvTask() *v1alpha1.Task {
	return &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: "t1"},
		Spec: v1alpha1.TaskSpec{
			Image:      "tmpl",
			Workspaces: []v1alpha1.TaskWorkspace{{Name: "ws1", Goal: "Fix bug #123"}},
			Runner:     v1alpha1.RunnerSpec{Command: []string{"claude", "-p", "it's fine"}},
		},
	}
}

// The net0 rewrite must keep every property the clone produced (bridge,
// hwaddr, ip mode) and only add or force the firewall flag: rewriting the
// hwaddr would change the container's MAC, and dropping ip=dhcp would break
// the lease.
func TestNet0WithFirewall(t *testing.T) {
	in := "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:BE:95:31,ip=dhcp,type=veth"
	out, err := net0WithFirewall(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := in + ",firewall=1"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	// An existing firewall=0 is forced to 1 rather than appended twice.
	out, err = net0WithFirewall("name=eth0,bridge=vmbr0,firewall=0,ip=dhcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "name=eth0,bridge=vmbr0,firewall=1,ip=dhcp" {
		t.Fatalf("existing flag not forced, got %q", out)
	}
	if _, err := net0WithFirewall(""); err == nil {
		t.Fatal("empty net0 must be rejected")
	}
}

// applyEgressPolicy always writes the implicit rules after policy_out=DROP:
// without DNS the runner cannot resolve anything, and without DHCP the
// ip=dhcp template loses its lease mid-run. The default-deny lands on the
// guest firewall options endpoint (policy_out is not an LXC config
// property — PVE's schema rejects it on the config endpoint), and the
// datacenter firewall must already be enabled or the whole install is
// refused: with it off PVE ignores every guest rule and the sandbox would
// run wide open behind an allowlist that looks enforced.
func TestApplyEgressPolicyRules(t *testing.T) {
	var mu sync.Mutex
	var rules []proxmox.FirewallRule
	var net0Set string
	var fwOpts map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/cluster/firewall/options", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {"enable": 1}}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/123/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(`{"data": {"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:BE:95:31,ip=dhcp,type=veth"}}`))
		case http.MethodPut:
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			net0Set = r.Form.Get("net0")
			mu.Unlock()
			w.Write([]byte(`{"data": null}`))
		}
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/123/firewall/options", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("guest firewall options write must be a PUT, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		fwOpts = map[string]string{
			"enable":     r.Form.Get("enable"),
			"policy_out": r.Form.Get("policy_out"),
		}
		mu.Unlock()
		w.Write([]byte(`{"data": null}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/123/firewall/rules", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		rules = append(rules, proxmox.FirewallRule{
			Proto:   r.Form.Get("proto"),
			Dest:    r.Form.Get("dest"),
			Dport:   r.Form.Get("dport"),
			Comment: r.Form.Get("comment"),
		})
		mu.Unlock()
		w.Write([]byte(`{"data": null}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := &provisioner{pve: proxmox.New(srv.URL, "n1", "root@pam!px=fake", false)}

	gw := &ResolvedGateway{Name: "locked", Egress: []v1alpha1.EgressRule{
		{CIDR: "10.0.0.0/8", Ports: "443", Proto: "tcp"},
	}}
	if err := p.applyEgressPolicy(context.Background(), 123, gw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []proxmox.FirewallRule{
		{Proto: "udp", Dport: "53", Comment: "px: dns"},
		{Proto: "tcp", Dport: "53", Comment: "px: dns"},
		{Proto: "udp", Dport: "67", Comment: "px: dhcp"},
		{Proto: "tcp", Dest: "10.0.0.0/8", Dport: "443", Comment: "px: gateway locked"},
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d: %+v", len(rules), len(want), rules)
	}
	for i, r := range want {
		if rules[i] != r {
			t.Errorf("rule %d = %+v, want %+v", i, rules[i], r)
		}
	}
	if fwOpts["enable"] != "1" || fwOpts["policy_out"] != "DROP" {
		t.Errorf("guest firewall options = %+v, want enable=1 policy_out=DROP", fwOpts)
	}
	// net0 passed to the config write must carry firewall=1 and keep the hwaddr.
	if !strings.Contains(net0Set, "firewall=1") || !strings.Contains(net0Set, "hwaddr=BC:24:11:BE:95:31") {
		t.Fatalf("net0 sent to PVE = %q", net0Set)
	}
}

// A disabled datacenter firewall makes every guest rule inert, so the
// install must be refused before anything is written — and the error must
// say how to fix it.
func TestApplyEgressPolicyRequiresClusterFirewall(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/cluster/firewall/options", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {}}`))
	})
	for _, path := range []string{
		"/api2/json/nodes/n1/lxc/123/config",
		"/api2/json/nodes/n1/lxc/123/firewall/options",
		"/api2/json/nodes/n1/lxc/123/firewall/rules",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Write([]byte(`{"data": null}`))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := &provisioner{pve: proxmox.New(srv.URL, "n1", "root@pam!px=fake", false)}

	err := p.applyEgressPolicy(context.Background(), 123, &ResolvedGateway{Name: "locked"})
	if err == nil {
		t.Fatal("a disabled cluster firewall must be refused")
	}
	if !strings.Contains(err.Error(), "cluster firewall is disabled") || !strings.Contains(err.Error(), "enable") {
		t.Errorf("error must name the problem and the fix, got: %v", err)
	}
	if calls != 0 {
		t.Errorf("nothing must be written to the container once the check fails, got %d writes", calls)
	}
}

// Create must have the deny-by-default policy fully installed before the
// container ever boots — a sandbox that starts wide open even briefly is
// not a sandbox. The final SSH boot step needs a real node, so the flow
// ends in the destroy-on-boot-failure path (a zero-value Executor never
// connects); the recorded PVE call order is the point.
func TestCreateOrdersEgressBeforeStart(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/nodes/n1/lxc", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"vmid": 9000, "name": "tmpl", "template": 1}]}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		record("clone")
		w.Write([]byte(`{"data": "UPID:n1:1:1:clone"}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(`{"data": {"unprivileged": 1, "net0": "name=eth0,bridge=vmbr0,hwaddr=AA,ip=dhcp,type=veth"}}`))
		case http.MethodPut:
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(r.Form.Get("net0"), "firewall=1") {
				record("net0-firewall")
			} else {
				record("config-put")
			}
		}
	})
	mux.HandleFunc("/api2/json/cluster/firewall/options", func(w http.ResponseWriter, r *http.Request) {
		record("cluster-firewall-check")
		w.Write([]byte(`{"data": {"enable": 1}}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142/firewall/options", func(w http.ResponseWriter, r *http.Request) {
		record("fw-options")
		w.Write([]byte(`{"data": null}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142/firewall/rules", func(w http.ResponseWriter, r *http.Request) {
		record("rule")
		w.Write([]byte(`{"data": null}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142/status/start", func(w http.ResponseWriter, r *http.Request) {
		record("start")
		w.Write([]byte(`{"data": "UPID:n1:2:2:start"}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142/status/stop", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": "UPID:n1:3:3:stop"}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142", func(w http.ResponseWriter, r *http.Request) {
		record("destroy")
		w.Write([]byte(`{"data": "UPID:n1:4:4:destroy"}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {"status": "stopped", "exitstatus": "OK"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The real gate polls the node's iptables; a stub keeps the test
	// deterministic while still pinning WHEN enforcement is awaited.
	p := &provisioner{pve: proxmox.New(srv.URL, "n1", "root@pam!px=fake", false), ssh: &sshexec.Executor{},
		egressGate: func(_ context.Context, vmid int) error {
			if vmid != 142 {
				t.Errorf("egress gate called with vmid %d, want 142", vmid)
			}
			record("egress-gate")
			return nil
		}}

	err := p.Create(context.Background(), testProvTask(), 142, nil, nil,
		&ResolvedGateway{Name: "locked", Egress: []v1alpha1.EgressRule{{CIDR: "10.0.0.0/8", Ports: "443"}}})
	if err == nil || !strings.Contains(err.Error(), "boot runner") {
		t.Fatalf("Create must fail at the (unreachable) SSH boot step, got: %v", err)
	}
	first := func(e string) int {
		for i, x := range events {
			if x == e {
				return i
			}
		}
		return -1
	}
	for _, want := range []string{"clone", "net0-firewall", "fw-options", "rule", "start", "destroy"} {
		if first(want) < 0 {
			t.Fatalf("event %q never happened; events: %v", want, events)
		}
	}
	for _, pair := range [][2]string{
		{"clone", "net0-firewall"},
		{"cluster-firewall-check", "net0-firewall"},
		{"net0-firewall", "fw-options"},
		{"fw-options", "rule"},
		{"rule", "start"},
		// The runner must not boot until the egress gate has passed — that
		// gap is the window in which a restricted sandbox is still open.
		{"start", "egress-gate"},
		{"egress-gate", "destroy"},
		{"start", "destroy"},
	} {
		if first(pair[0]) >= first(pair[1]) {
			t.Errorf("%s must precede %s; events: %v", pair[0], pair[1], events)
		}
	}
}

// The sandbox contract is an unprivileged uid mapping: a template that
// clones into a privileged container must fail the provision (and destroy
// the clone), not ship a sandbox the threat model does not cover.
func TestCreateRefusesPrivilegedClone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/nodes/n1/lxc", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"vmid": 9000, "name": "tmpl", "template": 1}]}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": "UPID:n1:1:1:clone"}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142/config", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {"unprivileged": 0}}`))
	})
	var destroyed bool
	mux.HandleFunc("/api2/json/nodes/n1/lxc/142", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			destroyed = true
		}
		w.Write([]byte(`{"data": "UPID:n1:2:2:destroy"}`))
	})
	mux.HandleFunc("/api2/json/nodes/n1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {"status": "stopped", "exitstatus": "OK"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := NewProvisioner(proxmox.New(srv.URL, "n1", "root@pam!px=fake", false), &sshexec.Executor{})

	err := p.Create(context.Background(), testProvTask(), 142, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("a privileged clone must fail the provision naming the problem, got: %v", err)
	}
	if !destroyed {
		t.Fatal("a privileged clone must be destroyed before the provision fails")
	}
}

func TestRunnerScriptLayout(t *testing.T) {
	script := runnerScript(testProvTask(), nil, nil)
	for _, want := range []string{
		"#!/bin/sh",
		"mkdir -p /run/px",
		"> /run/px/goal",
		"> /run/px/cmd.sh",
		"> /run/px/task.log 2>&1",
		"echo $? > /run/px/exit",
		"echo PX_BOOT_OK",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("boot script missing %q:\n%s", want, script)
		}
	}
	// The umask must precede every write, so the credential files never fall
	// back to the container's default (0644 under the usual 022).
	if !strings.HasPrefix(script, "#!/bin/sh\numask 077\n") {
		t.Errorf("boot script must set umask 077 before any writes:\n%s", script)
	}
	// The goal must be embedded base64-encoded, never raw.
	if strings.Contains(script, "Fix bug #123") {
		t.Error("goal leaked raw into boot script")
	}
	// Without mounts there must be no clone machinery, not even the DHCP wait.
	if strings.Contains(script, "ip route") || strings.Contains(script, "clone_ws") {
		t.Errorf("workspace machinery present without mounts:\n%s", script)
	}
	// The booted marker must be touched only after the runner is spawned, so
	// that a present marker proves the runner process exists.
	marker := strings.Index(script, "touch /run/px/booted")
	spawn := strings.Index(script, "nohup sh -c")
	if marker == -1 || spawn == -1 || marker < spawn {
		t.Errorf("booted marker must follow the runner spawn:\n%s", script)
	}
}

func TestRunnerScriptClonesWorkspaces(t *testing.T) {
	mounts := []ResolvedWorkspace{
		{Name: "repo-a", Repo: "https://example.com/a.git", Branch: "main"},
		{Name: "repo-b", Repo: "https://example.com/b.git"},
	}
	script := runnerScript(testProvTask(), mounts, nil)
	if !strings.Contains(script, "mkdir -p /workspace") {
		t.Errorf("missing /workspace mkdir:\n%s", script)
	}
	if !strings.Contains(script, "ip route 2>/dev/null | grep -q default") {
		t.Errorf("missing DHCP wait before the clones:\n%s", script)
	}
	// Repo and branch go in base64, so neither may appear raw in the script.
	for _, secret := range []string{"https://example.com/a.git", "main"} {
		if strings.Contains(script, secret) {
			t.Errorf("workspace input leaked raw into boot script: %q", secret)
		}
	}
	if !strings.Contains(script, `clone_ws0() { git clone --depth 1 --branch "$(cat /run/px/ws0.branch)" "$(cat /run/px/ws0.repo)" -- /workspace/repo-a; }`) {
		t.Errorf("missing branch clone for repo-a:\n%s", script)
	}
	if !strings.Contains(script, `clone_ws1() { git clone --depth 1 "$(cat /run/px/ws1.repo)" -- /workspace/repo-b; }`) {
		t.Errorf("missing default clone for repo-b:\n%s", script)
	}
	// Each clone retries once before the boot fails, clearing any partial
	// clone the first attempt left behind.
	for _, retry := range []string{
		`clone_ws0 || { rm -rf /workspace/repo-a; sleep 2; clone_ws0; } || { echo 'px: git clone repo-a failed' >&2; exit 1; }`,
		`clone_ws1 || { rm -rf /workspace/repo-b; sleep 2; clone_ws1; } || { echo 'px: git clone repo-b failed' >&2; exit 1; }`,
	} {
		if !strings.Contains(script, retry) {
			t.Errorf("missing clone retry line %q:\n%s", retry, script)
		}
	}
	// A failed clone must exit before the runner spawns, so the task lands in
	// the existing provision-failure path instead of a fake Running.
	cloneEnd := strings.Index(script, "exit 1")
	spawn := strings.Index(script, "nohup sh -c")
	if cloneEnd == -1 || spawn == -1 || cloneEnd > spawn {
		t.Errorf("clone failure handling must precede the runner spawn:\n%s", script)
	}
}

func TestQuoteCommandExportsGoalAndQuotesArgv(t *testing.T) {
	out := quoteCommand([]string{"claude", "-p", "it's fine"})
	if !strings.Contains(out, `export GOAL="$(cat /run/px/goal)"`) {
		t.Errorf("missing GOAL export:\n%s", out)
	}
	if !strings.Contains(out, `'claude' '-p' 'it'\''s fine'`) {
		t.Errorf("argv not safely quoted:\n%s", out)
	}
}

func TestBootCommandMkdirBeforeRedirect(t *testing.T) {
	cmd := bootCommand(142, "", "echo hi")
	um := strings.Index(cmd, "umask 077")
	mk := strings.Index(cmd, "mkdir -p /run/px")
	rd := strings.Index(cmd, "> /run/px/boot.sh")
	if um == -1 || mk == -1 || rd == -1 || um > mk || mk > rd {
		t.Fatalf("umask and mkdir must precede the /run/px/boot.sh redirect: %s", cmd)
	}
	if !strings.Contains(cmd, "pct exec 142 -- sh -c '") {
		t.Fatalf("bad pct invocation: %s", cmd)
	}
	// The embedded script must survive a base64 round-trip untouched.
	i := strings.Index(cmd, "echo ") + len("echo ")
	j := strings.Index(cmd[i:], " | base64")
	dec, err := base64.StdEncoding.DecodeString(cmd[i : i+j])
	if err != nil || string(dec) != "echo hi" {
		t.Fatalf("embedded script broken: %q err=%v", dec, err)
	}
}

func TestBootCommandUser(t *testing.T) {
	cmd := bootCommand(142, "agent", "echo hi")
	if !strings.Contains(cmd, "pct exec 142 --user 'agent' --") {
		t.Fatalf("user arg missing: %s", cmd)
	}
	if cmd := bootCommand(142, "", "echo hi"); strings.Contains(cmd, "--user") {
		t.Fatalf("no --user expected for empty runner.user: %s", cmd)
	}
}

// probeVerdict reads the PX_PROBE:<n> trailer a successful boot-probe exec
// prints; anything else is a retryable error, not a verdict — a garbage
// output must never be read as "unbooted" (that destroys live runners).
func TestProbeVerdict(t *testing.T) {
	if v, err := probeVerdict("PX_PROBE:0\n"); err != nil || !v {
		t.Fatalf("PX_PROBE:0 = (%v, %v), want (true, nil)", v, err)
	}
	if v, err := probeVerdict("PX_PROBE:1\n"); err != nil || v {
		t.Fatalf("PX_PROBE:1 = (%v, %v), want (false, nil)", v, err)
	}
	// Boot noise can precede the marker, so the trailer is what counts.
	if v, err := probeVerdict("some pct noise\nPX_PROBE:0\n"); err != nil || !v {
		t.Fatalf("trailer after noise = (%v, %v)", v, err)
	}
	for _, bad := range []string{"", "\n", "garbage"} {
		if _, err := probeVerdict(bad); err == nil {
			t.Errorf("output %q must be an error, not a verdict", bad)
		}
	}
}

// embeddedPayload decodes the base64 embedding the boot script carries for
// target (e.g. "/run/px/model.env"). base64 contains no single quotes, so the
// quoted token before the redirect marker is unambiguous.
func embeddedPayload(t *testing.T, script, target string) string {
	t.Helper()
	marker := "| base64 -d > " + target
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("no base64 embedding for %s:\n%s", target, script)
	}
	j := strings.LastIndex(script[:i], "'")
	k := strings.LastIndex(script[:j], "'")
	if j < 0 || k < 0 {
		t.Fatalf("malformed embedding for %s:\n%s", target, script)
	}
	dec, err := base64.StdEncoding.DecodeString(script[k+1 : j])
	if err != nil {
		t.Fatalf("bad base64 payload for %s: %v", target, err)
	}
	return string(dec)
}

// A resolved model must land in the boot script base64-encoded: the key file
// holds the raw key, the env file references it by cat — neither the key nor
// the base URL may appear raw anywhere in the script, and the runner spawn
// must source the env file so the vars reach the runner and its children.
func TestRunnerScriptInjectsModelEnv(t *testing.T) {
	model := &ResolvedModel{
		Provider: v1alpha1.ProviderAnthropic,
		APIKey:   "sk-test-123",
		BaseURL:  "https://proxy.example.com/v1",
	}
	script := runnerScript(testProvTask(), nil, model)

	for _, raw := range []string{"sk-test-123", "https://proxy.example.com", "ANTHROPIC_API_KEY=\"sk"} {
		if strings.Contains(script, raw) {
			t.Errorf("credential leaked raw into boot script: %q", raw)
		}
	}
	if got := embeddedPayload(t, script, "/run/px/model.key"); got != "sk-test-123" {
		t.Errorf("model.key payload = %q, want the raw key", got)
	}
	env := embeddedPayload(t, script, "/run/px/model.env")
	for _, want := range []string{
		`export ANTHROPIC_API_KEY="$(cat /run/px/model.key)"`,
		`export ANTHROPIC_BASE_URL="$(cat /run/px/model.baseurl)"`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("model.env missing %q, got:\n%s", want, env)
		}
	}
	if !strings.Contains(script, "> /run/px/model.baseurl") {
		t.Errorf("missing model.baseurl write:\n%s", script)
	}
	spawn := strings.Index(script, "nohup sh -c")
	if spawn == -1 || !strings.Contains(script[spawn:], "if [ -f /run/px/model.env ]; then . /run/px/model.env; fi;") {
		t.Errorf("runner spawn must source model.env behind an existence guard:\n%s", script)
	}
}

// An OpenAI model maps to the OPENAI_* names, per the provider env map.
func TestRunnerScriptOpenAIEnvPrefix(t *testing.T) {
	model := &ResolvedModel{Provider: v1alpha1.ProviderOpenAI, APIKey: "sk-oai"}
	script := runnerScript(testProvTask(), nil, model)
	env := embeddedPayload(t, script, "/run/px/model.env")
	if !strings.Contains(env, `export OPENAI_API_KEY="$(cat /run/px/model.key)"`) {
		t.Errorf("model.env missing OPENAI_API_KEY export, got:\n%s", env)
	}
	if strings.Contains(env, "BASE_URL") {
		t.Errorf("empty baseUrl must not emit a BASE_URL export, got:\n%s", env)
	}
	if strings.Contains(script, "> /run/px/model.baseurl") {
		t.Errorf("empty baseUrl must not write model.baseurl:\n%s", script)
	}
}

// A task without a model must not write any credential files. The spawn line
// still references model.env (one boot script serves both shapes), so the
// reference must be the guarded no-op source, never an unguarded one.
func TestRunnerScriptWithoutModel(t *testing.T) {
	script := runnerScript(testProvTask(), nil, nil)
	for _, absent := range []string{"> /run/px/model.key", "> /run/px/model.env", "> /run/px/model.baseurl"} {
		if strings.Contains(script, absent) {
			t.Errorf("model-less script writes %s:\n%s", absent, script)
		}
	}
	if !strings.Contains(script, "if [ -f /run/px/model.env ]; then . /run/px/model.env; fi;") {
		t.Errorf("spawn must tolerate a missing model.env (dash exits on a failed dot-builtin):\n%s", script)
	}
}
