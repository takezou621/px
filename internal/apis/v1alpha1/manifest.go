package v1alpha1

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Manifest is a parsed manifest object: header + the spec decoded by kind.
type Manifest struct {
	Kind       string
	APIVersion string
	Metadata   ObjectMeta

	Task      *TaskSpec      // non-nil when Kind == Task
	Workspace *WorkspaceSpec // non-nil when Kind == Workspace
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
		var goalBytes int
		for i, ws := range s.Workspaces {
			if ws.Name == "" {
				return nil, fmt.Errorf("task %q: workspaces[%d].name is required", m.Metadata.Name, i)
			}
			goalBytes += len(ws.Goal)
		}
		if goalBytes > MaxGoalBytes {
			return nil, fmt.Errorf("task %q: combined workspaces goal exceeds %d bytes", m.Metadata.Name, MaxGoalBytes)
		}
		var cmdBytes int
		for _, a := range s.Runner.Command {
			cmdBytes += len(a) + 1
		}
		if cmdBytes > MaxCommandBytes {
			return nil, fmt.Errorf("task %q: spec.runner.command exceeds %d bytes", m.Metadata.Name, MaxCommandBytes)
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
		m.Workspace = s
	default:
		return nil, fmt.Errorf("unknown kind %q", m.Kind)
	}
	return m, nil
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
