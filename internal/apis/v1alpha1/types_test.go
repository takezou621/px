package v1alpha1

import "testing"

func TestValidatePorts(t *testing.T) {
	ok := []struct {
		name string
		ps   []PortSpec
	}{
		{"auto-assigned", []PortSpec{{Name: "http", Port: 8080}}},
		{"explicit in range", []PortSpec{{Name: "http", Port: 8080, HostPort: 31234}}},
		{"range bounds", []PortSpec{
			{Name: "a", Port: 80, HostPort: HostPortMin},
			{Name: "b", Port: 443, HostPort: HostPortMax},
		}},
		{"distinct everything", []PortSpec{
			{Name: "http", Port: 80, HostPort: 30001},
			{Name: "https", Port: 443, HostPort: 30002},
		}},
	}
	for _, tc := range ok {
		if err := ValidatePorts(tc.ps); err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
	}

	bad := []struct {
		name string
		ps   []PortSpec
	}{
		{"too many", func() []PortSpec {
			ps := make([]PortSpec, MaxPorts+1)
			for i := range ps {
				ps[i] = PortSpec{Name: "p" + string(rune('a'+i)), Port: i + 1}
			}
			return ps
		}()},
		{"bad name", []PortSpec{{Name: "Bad_Name", Port: 80}}},
		{"duplicate name", []PortSpec{{Name: "http", Port: 80}, {Name: "http", Port: 443}}},
		{"port zero", []PortSpec{{Name: "http", Port: 0}}},
		{"port too high", []PortSpec{{Name: "http", Port: 65536}}},
		{"duplicate container port", []PortSpec{{Name: "a", Port: 8080}, {Name: "b", Port: 8080}}},
		{"hostPort below range", []PortSpec{{Name: "http", Port: 80, HostPort: HostPortMin - 1}}},
		{"hostPort above range", []PortSpec{{Name: "http", Port: 80, HostPort: HostPortMax + 1}}},
		{"duplicate hostPort", []PortSpec{{Name: "a", Port: 80, HostPort: 30001}, {Name: "b", Port: 443, HostPort: 30001}}},
	}
	for _, tc := range bad {
		if err := ValidatePorts(tc.ps); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestClaimedHostPorts(t *testing.T) {
	mk := func(name string, specHost, statusHost int) *Task {
		task := &Task{}
		task.Metadata.Name = name
		if specHost != 0 {
			task.Spec.Ports = []PortSpec{{Name: "http", Port: 80, HostPort: specHost}}
		}
		if statusHost != 0 {
			task.Status.Ports = []PortStatus{{Name: "http", Port: 80, HostPort: statusHost}}
		}
		return task
	}

	unresolved := &Task{} // auto-assigned entry whose hostPort no tick resolved yet
	unresolved.Metadata.Name = "unresolved"
	unresolved.Status.Ports = []PortStatus{{Name: "http", Port: 80, HostPort: 0}}

	tasks := []*Task{
		mk("explicit", 31234, 0),
		mk("auto", 0, 30000),
		mk("no-ports", 0, 0),
		unresolved,
	}

	claimed := ClaimedHostPorts(tasks, "")
	if got := claimed[31234]; got != "explicit" {
		t.Errorf("explicit spec hostPort: claimed by %q, want \"explicit\"", got)
	}
	if got := claimed[30000]; got != "auto" {
		t.Errorf("assigned status hostPort: claimed by %q, want \"auto\"", got)
	}
	if owner, ok := claimed[0]; ok {
		t.Errorf("hostPort 0 must not claim anything, claimed by %q", owner)
	}

	// The excluded task's claims must vanish, or the controller would
	// treat its own ports as taken and its allocator would never converge.
	claimed = ClaimedHostPorts(tasks, "explicit")
	if _, ok := claimed[31234]; ok {
		t.Error("excluded task's spec hostPort still claimed")
	}
	if _, ok := claimed[30000]; !ok {
		t.Error("unrelated claim dropped by exclude")
	}
}
