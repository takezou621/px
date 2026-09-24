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

func New(endpoint, node, token string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		node:     node,
		token:    token,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// apiError is a PVE API error response.
type apiError struct {
	Errors map[string][]string `json:"errors,omitempty"`
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
	form.Set("name", name)
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

// WaitForTask polls a PVE task until it finishes.
func (c *Client) WaitForTask(ctx context.Context, upid string) (TaskStatus, error) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		var st TaskStatus
		if err := c.do(ctx, http.MethodGet, "/nodes/"+c.node+"/tasks/"+url.PathEscape(upid)+"/status", nil, &st); err != nil {
			return st, err
		}
		if st.Status != "running" {
			return st, nil
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
