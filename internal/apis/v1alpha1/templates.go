package v1alpha1

import "strings"

// Template is a discovered LXC template plus px's verdict on it. Read-only
// discovery: templates are not px objects — there is no apply, no delete.
// The facts (VMID, node, unprivileged, net0-derived DHCP) come from the PVE
// cluster view and one config read; PxOK/Missing summarize them against the
// contract published in template/README.md.
type Template struct {
	Name         string `json:"name"`
	VMID         int    `json:"vmid"`
	Node         string `json:"node"`
	Unprivileged bool   `json:"unprivileged"`
	DHCP         bool   `json:"dhcp"`
	PxOK         bool   `json:"pxOk"`
	// Missing names the failed requirements, in contract order. It also
	// carries discovery faults ("config unreadable") so one broken guest
	// lists as PX-OK false instead of blanking the whole listing.
	Missing []string `json:"missing,omitempty"`
}

// VerdictTemplate computes px's compatibility verdict for one discovered
// template. unprivileged comes from the template's config flag, net0 from
// the same config. Only the two API-visible requirements are judged here —
// the in-CT ones (a writable /run/px, /bin/sh) would need exec'ing into a
// container that is not px's, so they stay documented in template/README.md
// and surface as ProvisionFailed instead.
func VerdictTemplate(name string, vmid int, node string, unprivileged bool, net0 string) *Template {
	t := &Template{Name: name, VMID: vmid, Node: node, Unprivileged: unprivileged}
	t.DHCP = strings.Contains(net0, "ip=dhcp")
	if !unprivileged {
		t.Missing = append(t.Missing, "unprivileged")
	}
	if !t.DHCP {
		t.Missing = append(t.Missing, "net0 ip=dhcp")
	}
	t.PxOK = len(t.Missing) == 0
	return t
}
