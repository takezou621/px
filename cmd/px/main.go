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

var serverURL = "http://127.0.0.1:7420"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&serverURL, "server", envOr("PX_SERVER", "http://127.0.0.1:7420"), "px-server URL")
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
	if len(args) < 1 || args[0] != "tasks" {
		return fmt.Errorf("usage: px get tasks")
	}
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
}

func cmdDescribe(fs *flag.FlagSet, args []string) error {
	if len(args) > 0 && args[0] == "task" {
		args = args[1:]
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: px describe task NAME")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	name := args[0]
	var t *v1alpha1.Task
	if err := doJSON(http.MethodGet, "/v1/tasks/"+name, nil, &t); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(t, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdLogs(fs *flag.FlagSet, args []string) error {
	follow := fs.Bool("f", false, "follow")
	if len(args) > 0 && args[0] == "-f" {
		*follow = true
		args = args[1:]
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: px logs NAME [-f]")
	}
	name := args[0]
	if !*follow {
		return stream(http.MethodGet, "/v1/tasks/"+name+"/logs", os.Stdout)
	}
	// Follow: poll and print the tail.
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
		time.Sleep(2 * time.Second)
	}
}

func cmdDelete(fs *flag.FlagSet, args []string) error {
	if len(args) > 0 && args[0] == "task" {
		args = args[1:]
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: px delete task NAME")
	}
	name := args[0]
	var out map[string]string
	if err := doJSON(http.MethodDelete, "/v1/tasks/"+name, nil, &out); err != nil {
		return err
	}
	fmt.Printf("task.px.io/%s deleting\n", name)
	return nil
}

func doJSON(method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, strings.TrimRight(serverURL, "/")+path, bodyReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/yaml")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w (is px-server running?)", serverURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct{ Error string `json:"error"` }
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

func stream(method, path string, w io.Writer) error {
	resp, err := http.Get(strings.TrimRight(serverURL, "/") + path)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", serverURL, err)
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
  px describe task NAME           Show one task as JSON
  px logs NAME [-f]               Stream runner logs
  px delete task NAME             Delete a task and its container
  px version                      Show version

Flags:
  -server URL    px-server URL (env PX_SERVER, default http://127.0.0.1:7420)
`)
}
