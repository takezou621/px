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
		if s.Image == "" {
			return nil, fmt.Errorf("task %q: spec.image is required", m.Metadata.Name)
		}
		if len(s.Runner.Command) == 0 {
			return nil, fmt.Errorf("task %q: spec.runner.command is required", m.Metadata.Name)
		}
		if s.Runner.User != "" {
			if err := ValidateUser(s.Runner.User); err != nil {
				return nil, fmt.Errorf("task %q: %w", m.Metadata.Name, err)
			}
		}
		var goalBytes = len(s.Goal)
		if len(s.Workspaces) > MaxWorkspaces {
			return nil, fmt.Errorf("task %q: spec.workspaces exceeds %d entries", m.Metadata.Name, MaxWorkspaces)
		}
		seenWS := map[string]bool{}
		for i, ws := range s.Workspaces {
			if ws.Name == "" {
				return nil, fmt.Errorf("task %q: workspaces[%d].name is required", m.Metadata.Name, i)
			}
			// The reference name becomes a path segment (/workspace/<name>) in
			// the boot script; validating here keeps that safety property
			// local to the parser instead of relying on the Workspace kind
			// having been validated too.
			if err := ValidateName(ws.Name); err != nil {
				return nil, fmt.Errorf("task %q: workspaces[%d].name: %w", m.Metadata.Name, i, err)
			}
			if seenWS[ws.Name] {
				return nil, fmt.Errorf("task %q: workspaces[%d].name %q is duplicated", m.Metadata.Name, i, ws.Name)
			}
			seenWS[ws.Name] = true
			goalBytes += len(ws.Goal)
		}
		if goalBytes > MaxGoalBytes {
			return nil, fmt.Errorf("task %q: combined task and workspace goal exceeds %d bytes", m.Metadata.Name, MaxGoalBytes)
		}
		var cmdBytes int
		for _, a := range s.Runner.Command {
			cmdBytes += len(a) + 1
		}
		if cmdBytes > MaxCommandBytes {
			return nil, fmt.Errorf("task %q: spec.runner.command exceeds %d bytes", m.Metadata.Name, MaxCommandBytes)
		}
		if s.Model != "" {
			if err := ValidateName(s.Model); err != nil {
				return nil, fmt.Errorf("task %q: spec.model: %w", m.Metadata.Name, err)
			}
		}
		if s.Gateway != "" {
			if err := ValidateName(s.Gateway); err != nil {
				return nil, fmt.Errorf("task %q: spec.gateway: %w", m.Metadata.Name, err)
			}
		}
		if err := ValidatePorts(s.Ports); err != nil {
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
	default:
		return nil, fmt.Errorf("unknown kind %q", m.Kind)
	}
	return m, nil
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
