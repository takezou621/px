package v1alpha1

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is a parsed manifest object: header + the spec decoded by kind.
type Manifest struct {
	Kind       string
	APIVersion string
	Metadata   ObjectMeta

	Task      *TaskSpec      // non-nil when Kind == Task
	Workspace *WorkspaceSpec // non-nil when Kind == Workspace
	Model     *ModelSpec     // non-nil when Kind == Model
	Gateway   *GatewaySpec   // non-nil when Kind == Gateway
	Schedule  *ScheduleSpec  // non-nil when Kind == Schedule
}

// ParseManifests parses a multi-document YAML manifest stream.
func ParseManifests(r io.Reader) ([]*Manifest, error) {
	dec := yaml.NewDecoder(r)
	var manifests []*Manifest
	for {
		var raw map[string]any
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if len(raw) == 0 {
			continue // empty document between "---"
		}
		m, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}

func decodeObject(raw map[string]any) (*Manifest, error) {
	// Pass 1: header only (spec is consumed in pass 2).
	var header struct {
		APIVersion string     `yaml:"apiVersion"`
		Kind       string     `yaml:"kind"`
		Metadata   ObjectMeta `yaml:"metadata"`
		Spec       yaml.Node  `yaml:"spec"`
	}
	if err := strictDecode(raw, &header); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if header.APIVersion != APIVersion {
		return nil, fmt.Errorf("apiVersion must be %q, got %q", APIVersion, header.APIVersion)
	}
	if err := ValidateName(header.Metadata.Name); err != nil {
		return nil, err
	}

	m := &Manifest{Kind: header.Kind, APIVersion: header.APIVersion, Metadata: header.Metadata}

	// Pass 2: spec, decoded per kind.
	spec, ok := raw["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s %q: spec is required", m.Kind, m.Metadata.Name)
	}
	switch m.Kind {
	case KindTask:
		s := &TaskSpec{}
		if err := strictDecode(spec, s); err != nil {
			return nil, fmt.Errorf("task %q: %w", m.Metadata.Name, err)
		}
		if err := ValidateTaskSpec(s); err != nil {
			return nil, fmt.Errorf("task %q: %w", m.Metadata.Name, err)
		}
		m.Task = s
	case KindWorkspace:
		s := &WorkspaceSpec{}
		if err := strictDecode(spec, s); err != nil {
			return nil, fmt.Errorf("workspace %q: %w", m.Metadata.Name, err)
		}
		if s.Git.Repo == "" {
			return nil, fmt.Errorf("workspace %q: spec.git.repo is required", m.Metadata.Name)
		}
		if len(s.Git.Repo) > MaxRepoBytes {
			return nil, fmt.Errorf("workspace %q: spec.git.repo exceeds %d bytes", m.Metadata.Name, MaxRepoBytes)
		}
		if len(s.Git.Branch) > MaxBranchBytes {
			return nil, fmt.Errorf("workspace %q: spec.git.branch exceeds %d bytes", m.Metadata.Name, MaxBranchBytes)
		}
		m.Workspace = s
	case KindModel:
		s := &ModelSpec{}
		if err := strictDecode(spec, s); err != nil {
			return nil, fmt.Errorf("model %q: %w", m.Metadata.Name, err)
		}
		if err := ValidateProvider(s.Provider); err != nil {
			return nil, fmt.Errorf("model %q: %w", m.Metadata.Name, err)
		}
		if s.APIKey == "" {
			return nil, fmt.Errorf("model %q: spec.apiKey is required", m.Metadata.Name)
		}
		if s.APIKey == RedactedAPIKey {
			return nil, fmt.Errorf("model %q: spec.apiKey is the redaction placeholder; provide the real key", m.Metadata.Name)
		}
		if err := validateSecretValue("spec.apiKey", s.APIKey); err != nil {
			return nil, fmt.Errorf("model %q: %w", m.Metadata.Name, err)
		}
		if len(s.APIKey) > MaxAPIKeyBytes {
			return nil, fmt.Errorf("model %q: spec.apiKey exceeds %d bytes", m.Metadata.Name, MaxAPIKeyBytes)
		}
		if len(s.BaseURL) > MaxBaseURLBytes {
			return nil, fmt.Errorf("model %q: spec.baseUrl exceeds %d bytes", m.Metadata.Name, MaxBaseURLBytes)
		}
		if err := validateSecretValue("spec.baseUrl", s.BaseURL); err != nil {
			return nil, fmt.Errorf("model %q: %w", m.Metadata.Name, err)
		}
		m.Model = s
	case KindGateway:
		s := &GatewaySpec{}
		if err := strictDecode(spec, s); err != nil {
			return nil, fmt.Errorf("gateway %q: %w", m.Metadata.Name, err)
		}
		if len(s.Egress) > MaxEgressRules {
			return nil, fmt.Errorf("gateway %q: spec.egress exceeds %d rules", m.Metadata.Name, MaxEgressRules)
		}
		for i := range s.Egress {
			if err := ValidateEgress(&s.Egress[i]); err != nil {
				return nil, fmt.Errorf("gateway %q: egress[%d]: %w", m.Metadata.Name, i, err)
			}
		}
		m.Gateway = s
	case KindSchedule:
		s := &ScheduleSpec{}
		if err := strictDecode(spec, s); err != nil {
			return nil, fmt.Errorf("schedule %q: %w", m.Metadata.Name, err)
		}
		if err := ValidateSchedule(s); err != nil {
			return nil, fmt.Errorf("schedule %q: %w", m.Metadata.Name, err)
		}
		if err := ValidateTaskSpec(&s.TaskTemplate); err != nil {
			return nil, fmt.Errorf("schedule %q: taskTemplate: %w", m.Metadata.Name, err)
		}
		m.Schedule = s
	default:
		return nil, fmt.Errorf("unknown kind %q", m.Kind)
	}
	return m, nil
}

// ValidateTaskSpec checks a task spec's required fields, size caps and
// reference name shapes — everything an applied Task manifest is checked
// for. Schedule taskTemplates go through the same function, so a generated
// task can never carry what apply would reject. Error text names fields,
// not objects: callers wrap it with their kind and name.
func ValidateTaskSpec(s *TaskSpec) error {
	if s.Image == "" {
		return fmt.Errorf("spec.image is required")
	}
	if len(s.Runner.Command) == 0 {
		return fmt.Errorf("spec.runner.command is required")
	}
	if s.Runner.User != "" {
		if err := ValidateUser(s.Runner.User); err != nil {
			return err
		}
	}
	var goalBytes = len(s.Goal)
	if len(s.Workspaces) > MaxWorkspaces {
		return fmt.Errorf("spec.workspaces exceeds %d entries", MaxWorkspaces)
	}
	seenWS := map[string]bool{}
	for i, ws := range s.Workspaces {
		if ws.Name == "" {
			return fmt.Errorf("workspaces[%d].name is required", i)
		}
		// The reference name becomes a path segment (/workspace/<name>) in
		// the boot script; validating here keeps that safety property
		// local to the parser instead of relying on the Workspace kind
		// having been validated too.
		if err := ValidateName(ws.Name); err != nil {
			return fmt.Errorf("workspaces[%d].name: %w", i, err)
		}
		if seenWS[ws.Name] {
			return fmt.Errorf("workspaces[%d].name %q is duplicated", i, ws.Name)
		}
		seenWS[ws.Name] = true
		goalBytes += len(ws.Goal)
	}
	if goalBytes > MaxGoalBytes {
		return fmt.Errorf("combined task and workspace goal exceeds %d bytes", MaxGoalBytes)
	}
	var cmdBytes int
	for _, a := range s.Runner.Command {
		cmdBytes += len(a) + 1
	}
	if cmdBytes > MaxCommandBytes {
		return fmt.Errorf("spec.runner.command exceeds %d bytes", MaxCommandBytes)
	}
	if s.Model != "" {
		if err := ValidateName(s.Model); err != nil {
			return fmt.Errorf("spec.model: %w", err)
		}
	}
	if s.Gateway != "" {
		if err := ValidateName(s.Gateway); err != nil {
			return fmt.Errorf("spec.gateway: %w", err)
		}
	}
	if err := ValidatePorts(s.Ports); err != nil {
		return err
	}
	if err := ValidateSession(s.Session); err != nil {
		return err
	}
	return nil
}

// validateSecretValue rejects Model secret values that could not survive the
// boot-script round-trip intact: /run/px/model.env reads the stored files via
// shell command substitution, which strips trailing newlines — so a value
// applied with surrounding whitespace or control bytes would reach the
// runner as a different value than the one apply accepted.
func validateSecretValue(field, v string) error {
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%s must not have leading or trailing whitespace", field)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s must not contain control characters", field)
		}
	}
	return nil
}

// strictDecode decodes YAML data into out, rejecting unknown fields.
func strictDecode(raw any, out any) error {
	buf := &bytes.Buffer{}
	enc := yaml.NewEncoder(buf)
	if err := enc.Encode(raw); err != nil {
		return err
	}
	enc.Close()
	dec := yaml.NewDecoder(buf)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w (check for unknown or misspelled fields)", err)
	}
	return nil
}
