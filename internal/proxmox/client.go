// Package proxmox is a thin REST client for the Proxmox VE API,
// scoped to what px needs: LXC clone/start/stop/delete and task tracking.
package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a single Proxmox VE node's API.
type Client struct {
	endpoint string // e.g. https://pve.example.com:8006
	node     string
	token    string // user@realm!tokenid=secret
	http     *http.Client
}

func New(endpoint, node, token string, skipTLSVerify bool) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if skipTLSVerify {
		// PVE installs a self-signed node cert (pve-ssl) by default; homelab
		// deployments opt into skipping verification via px-server's
		// -tls-insecure flag.
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		node:     node,
		token:    token,
		http:     &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}
}

// apiError is a PVE API error response. PVE is inconsistent about the
// error values: parameter verification reports strings ("name": "..."),
// task failures report arrays ("vmid": ["does not exist"]).
type apiError struct {
	Errors map[string][]string `json:"errors,omitempty"`
}

func (ae *apiError) UnmarshalJSON(b []byte) error {
	var raw struct {
		Errors map[string]json.RawMessage `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	ae.Errors = make(map[string][]string, len(raw.Errors))
	for k, v := range raw.Errors {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			ae.Errors[k] = []string{s}
			continue
		}
		var arr []string
		if err := json.Unmarshal(v, &arr); err == nil {
			ae.Errors[k] = arr
		}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	u := c.endpoint + "/api2/json" + path
	var body io.Reader
	if form != nil {
		body = bytes.NewBufferString(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pve %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("pve %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		var ae apiError
		_ = json.Unmarshal(raw, &ae)
		if len(ae.Errors) > 0 {
			var parts []string
			for k, v := range ae.Errors {
				parts = append(parts, fmt.Sprintf("%s: %s", k, strings.Join(v, ", ")))
			}
			return fmt.Errorf("pve %s %s: %d: %s", method, path, resp.StatusCode, strings.Join(parts, "; "))
		}
		return fmt.Errorf("pve %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("pve %s %s: decode: %w", method, path, err)
	}
	if len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("pve %s %s: decode data: %w", method, path, err)
		}
	}
	return nil
}

// NextID returns the next free VMID.
func (c *Client) NextID(ctx context.Context) (int, error) {
	var s string
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &s); err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// TaskStatus is the result of a finished PVE task.
type TaskStatus struct {
	Status     string // "OK" or "running" or an error word
	ExitStatus string
}

// CloneContainer clones a template into a new container.
// Template properties (ostemplate, rootfs) are inherited via clone=1.
func (c *Client) CloneContainer(ctx context.Context, templateVMID, newVMID int, name string) error {
	form := url.Values{}
	form.Set("newid", strconv.Itoa(newVMID))
	// The LXC clone endpoint takes the container hostname (QEMU's clone
	// calls it "name"; LXC rejects that parameter outright).
	form.Set("hostname", name)
	var upid string
	// POST to the template's clone endpoint.
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/lxc/%d/clone", c.node, templateVMID), form, &upid); err != nil {
		return err
	}
	_, err := c.WaitForTask(ctx, upid)
	return err
}

// StartContainer starts a container and waits until it is running.
func (c *Client) StartContainer(ctx context.Context, vmid int) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/lxc/%d/status/start", c.node, vmid), nil, &upid); err != nil {
		return err
	}
	if _, err := c.WaitForTask(ctx, upid); err != nil {
		return err
	}
	return nil
}

// StopContainer force-stops a container.
func (c *Client) StopContainer(ctx context.Context, vmid int) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/lxc/%d/status/stop", c.node, vmid), nil, &upid); err != nil {
		// Already stopped is fine.
		if strings.Contains(err.Error(), "not running") {
			return nil
		}
		return err
	}
	_, err := c.WaitForTask(ctx, upid)
	return err
}

// DestroyContainer deletes a container.
func (c *Client) DestroyContainer(ctx context.Context, vmid int) error {
	var upid string
	if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/lxc/%d", c.node, vmid), nil, &upid); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return err
	}
	_, err := c.WaitForTask(ctx, upid)
	return err
}

// ContainerRunning reports whether the container is running.
func (c *Client) ContainerRunning(ctx context.Context, vmid int) (bool, error) {
	var st struct {
		Status string `json:"status"` // "running" | "stopped"
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/lxc/%d/status/current", c.node, vmid), nil, &st); err != nil {
		return false, err
	}
	return st.Status == "running", nil
}

// IsNotFound reports whether err is PVE's response for a container that does
// not exist: an HTTP 500 whose body says the config file is gone. A network
// error never matches (a DNS failure's "no such host" must not read as
// "absent"), so callers can treat not-found as "already destroyed" safely.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, ": 500: ") && strings.Contains(s, "does not exist")
}

// ContainerHostname returns the container's hostname — the name the clone
// set, which ownership checks before a destroy compare against.
func (c *Client) ContainerHostname(ctx context.Context, vmid int) (string, error) {
	var cfg struct {
		Hostname string `json:"hostname"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/lxc/%d/config", c.node, vmid), nil, &cfg); err != nil {
		return "", err
	}
	return cfg.Hostname, nil
}

// ContainerNet0 returns the container's net0 config value
// ("name=eth0,bridge=vmbr0,hwaddr=...,ip=dhcp,type=veth") — read before
// rewriting it, so enabling the firewall keeps the clone's hwaddr and type.
func (c *Client) ContainerNet0(ctx context.Context, vmid int) (string, error) {
	var cfg struct {
		Net0 string `json:"net0"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/lxc/%d/config", c.node, vmid), nil, &cfg); err != nil {
		return "", err
	}
	return cfg.Net0, nil
}

// EnableFirewall sets the guest firewall flag on the container's config
// (a valid LXC config property, `pct set <vmid> --firewall 1`). net0 must
// be the container's current value with firewall=1 appended.
func (c *Client) EnableFirewall(ctx context.Context, vmid int, net0 string) error {
	form := url.Values{}
	form.Set("net0", net0)
	form.Set("firewall", "1")
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/lxc/%d/config", c.node, vmid), form, nil)
}

// SetEgressDropPolicy writes the guest firewall options (the per-guest
// section under Datacenter > Firewall): enable the guest firewall and
// default-deny egress — policy_out=DROP; policy_in stays at its default,
// so replies ride the same open inbound path as before. These are firewall
// options, not LXC config properties: PVE's parameter schema rejects
// policy_out on the config endpoint.
func (c *Client) SetEgressDropPolicy(ctx context.Context, vmid int) error {
	form := url.Values{}
	form.Set("enable", "1")
	form.Set("policy_out", "DROP")
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/lxc/%d/firewall/options", c.node, vmid), form, nil)
}

// ClusterFirewallEnabled reports whether the datacenter-level firewall is
// enabled. Guest rules only apply when the cluster option is on — with it
// off PVE ignores every guest rule, which would silently turn an egress
// allowlist into a no-op, so callers must refuse to provision rather than
// fake the guarantee.
func (c *Client) ClusterFirewallEnabled(ctx context.Context) (bool, error) {
	var opts struct {
		Enable int `json:"enable"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/firewall/options", nil, &opts); err != nil {
		return false, err
	}
	return opts.Enable == 1, nil
}

// FirewallRule is one CT firewall rule to append.
type FirewallRule struct {
	Proto   string // "tcp", "udp" or "" for any protocol
	Dest    string // destination IP/CIDR, "" for any
	Dport   string // destination port(s), "" for any
	Comment string
}

// AddFirewallRule appends one outbound ACCEPT rule: a conntrack-matched
// reply comes back through policy_in, so allowing the outbound initiation
// is all an egress allow needs.
func (c *Client) AddFirewallRule(ctx context.Context, vmid int, r FirewallRule) error {
	form := url.Values{}
	form.Set("enable", "1")
	form.Set("type", "out")
	form.Set("action", "ACCEPT")
	if r.Proto != "" {
		form.Set("proto", r.Proto)
	}
	if r.Dest != "" {
		form.Set("dest", r.Dest)
	}
	if r.Dport != "" {
		form.Set("dport", r.Dport)
	}
	if r.Comment != "" {
		form.Set("comment", r.Comment)
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/lxc/%d/firewall/rules", c.node, vmid), form, nil)
}

// maxTaskWait bounds WaitForTask so a stuck PVE task cannot wedge the
// serial reconcile loop forever.
const maxTaskWait = 15 * time.Minute

// WaitForTask polls a PVE task until it finishes and verifies it succeeded.
func (c *Client) WaitForTask(ctx context.Context, upid string) (TaskStatus, error) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(maxTaskWait)
	for {
		var st TaskStatus
		if err := c.do(ctx, http.MethodGet, "/nodes/"+c.node+"/tasks/"+url.PathEscape(upid)+"/status", nil, &st); err != nil {
			return st, err
		}
		if st.Status != "running" {
			if !strings.HasPrefix(st.ExitStatus, "OK") {
				return st, fmt.Errorf("pve task failed: %s", st.ExitStatus)
			}
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("pve task timed out after %s: %s", maxTaskWait, upid)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-tick.C:
		}
	}
}

// FindTemplateVMID looks up a container template by its name.
func (c *Client) FindTemplateVMID(ctx context.Context, name string) (int, error) {
	var list []struct {
		VMID     int    `json:"vmid"`
		Name     string `json:"name"`
		Template int    `json:"template"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/lxc", c.node), nil, &list); err != nil {
		return 0, err
	}
	for _, ct := range list {
		if ct.Template == 1 && ct.Name == name {
			return ct.VMID, nil
		}
	}
	return 0, fmt.Errorf("template %q not found on node %s", name, c.node)
}
