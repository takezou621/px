// px is the kubectl-style CLI for the px control plane.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
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
	case "get":
		err = cmdGet(fs, args)
	case "describe":
		err = cmdDescribe(fs, args)
	case "logs":
		err = cmdLogs(fs, args)
	case "watch":
		err = cmdWatch(fs, args)
	case "delete":
		err = cmdDelete(fs, args)
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

func cmdGet(fs *flag.FlagSet, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: px get tasks|workspaces|models|gateways")
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
		fmt.Printf("%-24s %-16s %-8s %s\n", "NAME", "PHASE", "CT", "AGE")
		for _, t := range tasks {
			fmt.Printf("%-24s %-16s %-8d %s\n",
				t.Metadata.Name, t.Status.Phase, t.Status.Container, age(t.Status.StartedAt))
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
	default:
		return fmt.Errorf("usage: px get tasks|workspaces|models|gateways")
	}
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
	if len(args) > 0 && (args[0] == "task" || args[0] == "workspace" || args[0] == "model" || args[0] == "gateway") {
		kind = args[0]
		args = args[1:]
	}
	name, rest, err := popName(args)
	if err != nil {
		return fmt.Errorf("usage: px describe task NAME | describe workspace NAME | describe model NAME | describe gateway NAME")
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

func cmdDelete(fs *flag.FlagSet, args []string) error {
	kind := "task"
	if len(args) > 0 && (args[0] == "task" || args[0] == "model" || args[0] == "gateway") {
		kind = args[0]
		args = args[1:]
	}
	name, rest, err := popName(args)
	if err != nil {
		return fmt.Errorf("usage: px delete task NAME | delete model NAME | delete gateway NAME")
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
	}
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
  px get tasks                    List tasks
  px get workspaces               List workspaces
  px get models                   List models (API keys redacted)
  px get gateways                 List gateways with their egress rules
  px describe task NAME           Show one task as JSON
  px describe workspace NAME      Show one workspace as JSON
  px describe model NAME          Show one model as JSON (API key redacted)
  px describe gateway NAME        Show one gateway as JSON
  px logs NAME [-f]               Stream runner logs
  px watch                        Stream task phase transitions
  px delete task NAME             Delete a task and its container
  px delete model NAME            Delete a model
  px delete gateway NAME          Delete a gateway
  px version                      Show version

Flags:
  -server URL    px-server URL (env PX_SERVER, default http://127.0.0.1:7420)
  -token TOK     API bearer token (env PX_TOKEN; required when px-server runs with -token-file)
`)
}
