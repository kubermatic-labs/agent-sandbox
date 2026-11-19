// Package labels is the one definition of the podman labels agent-sandbox
// puts on its containers, volumes and images (spec 2.1). Lookup, status,
// stop and prune all find objects by them; nothing is parsed back from
// names.
//
// Labels outlive the binary that set them, so a key or value must never
// change: containers, volumes and images created by an older version would
// no longer be found, and prune would no longer recognise them as ours.
package labels

import (
	"maps"
	"slices"

	"k8c.io/agent-sandbox/internal/project"
)

// Label keys.
const (
	// Managed marks every object agent-sandbox creates, with value
	// ManagedValue.
	Managed = "agent-sandbox"
	// Project is the absolute project root. Volumes from before it was
	// added carry only ProjectID.
	Project = "agent-sandbox.project"
	// ProjectID is the project ID (spec 2.1).
	ProjectID = "agent-sandbox.project-id"
	// Name is the sandbox name (containers).
	Name = "agent-sandbox.name"
	// ImageID is the podman image ID a container was started from.
	ImageID = "agent-sandbox.image-id"
	// Binary is the sha256 of the agent-sandbox binary that started a
	// container.
	Binary = "agent-sandbox.binary"
	// GitMode is the git mode a container was started in: ro or worktree.
	GitMode = "agent-sandbox.git-mode"
	// Config is the hash of the policy a container enforces (spec 2.4).
	Config = "agent-sandbox.config"
	// Kind tells base and project images apart: KindBase or KindProject.
	Kind = "agent-sandbox.kind"
)

// ManagedValue is the value of Managed.
const ManagedValue = "1"

// Values of Kind.
const (
	KindBase    = "base"
	KindProject = "project"
)

// Owned returns the filter that matches every object agent-sandbox created.
func Owned() map[string]string {
	return map[string]string{Managed: ManagedValue}
}

// IsOwned reports whether labels mark an object agent-sandbox created.
func IsOwned(l map[string]string) bool {
	return l[Managed] == ManagedValue
}

// Container returns a sandbox container's labels. RunArgs adds Config.
func Container(p project.Project, name, gitMode, imageID, binaryHash string) map[string]string {
	return map[string]string{
		Managed:   ManagedValue,
		Project:   p.Root,
		ProjectID: p.ID,
		Name:      name,
		ImageID:   imageID,
		Binary:    binaryHash,
		GitMode:   gitMode,
	}
}

// Lookup returns the filter that identifies one sandbox's container.
func Lookup(projectID, name string) map[string]string {
	return map[string]string{ProjectID: projectID, Name: name}
}

// Volume returns the labels of a project's volumes. The root lets
// prune --orphaned find the volumes of deleted projects.
func Volume(p project.Project) map[string]string {
	return map[string]string{Managed: ManagedValue, Project: p.Root, ProjectID: p.ID}
}

// BaseImage returns the labels of a base image (spec 3.3).
func BaseImage() map[string]string {
	return map[string]string{Managed: ManagedValue, Kind: KindBase}
}

// ProjectImage returns the labels of a project image. Images inherit their
// base's labels, so this overrides Kind.
func ProjectImage(root, projectID string) map[string]string {
	return map[string]string{Managed: ManagedValue, Kind: KindProject, Project: root, ProjectID: projectID}
}

// Args returns --label flags for podman run, build and volume create,
// sorted by key so the argv is deterministic.
func Args(l map[string]string) []string {
	args := make([]string, 0, 2*len(l))
	for _, k := range slices.Sorted(maps.Keys(l)) {
		args = append(args, "--label", k+"="+l[k])
	}
	return args
}
