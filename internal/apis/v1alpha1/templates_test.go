package v1alpha1

import (
	"reflect"
	"testing"
)

func TestVerdictTemplate(t *testing.T) {
	dhcpNet0 := "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:2E:1F:00,ip=dhcp,type=veth"
	staticNet0 := "name=eth0,bridge=vmbr0,ip=192.168.2.50/24,gw=192.168.2.1,type=veth"
	tests := []struct {
		name         string
		unprivileged bool
		net0         string
		wantOK       bool
		wantMissing  []string
	}{
		{"meets the contract", true, dhcpNet0, true, nil},
		{"privileged template", false, dhcpNet0, false, []string{"unprivileged"}},
		{"static address", true, staticNet0, false, []string{"net0 ip=dhcp"}},
		{"no net0 at all", true, "", false, []string{"net0 ip=dhcp"}},
		{"both missing", false, staticNet0, false, []string{"unprivileged", "net0 ip=dhcp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VerdictTemplate("tpl", 101, "n1", tt.unprivileged, tt.net0)
			if got.Name != "tpl" || got.VMID != 101 || got.Node != "n1" {
				t.Fatalf("facts not carried: %+v", got)
			}
			if got.Unprivileged != tt.unprivileged || got.DHCP != (tt.net0 == dhcpNet0) {
				t.Fatalf("facts not derived: %+v", got)
			}
			if got.PxOK != tt.wantOK {
				t.Errorf("PxOK = %v, want %v", got.PxOK, tt.wantOK)
			}
			if !reflect.DeepEqual(got.Missing, tt.wantMissing) {
				t.Errorf("Missing = %v, want %v", got.Missing, tt.wantMissing)
			}
		})
	}
}
