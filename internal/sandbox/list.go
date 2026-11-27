package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// List names every collie container and whether it runs (without the
// usage numbers All reports).
func (m *Manager) List() []ContainerInfo {
	if !m.available {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.containers(ctx, "")
}

// ReadOnlyMounts lists where a user's container for a project has
// read-only mounts (for Home: the projects it may read).
func (m *Manager) ReadOnlyMounts(projectID, userID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.podman(ctx, "container", "inspect", "--format", "{{json .Mounts}}", Name(projectID, userID))
	if err != nil {
		return nil, err
	}
	var ms []struct {
		Destination string
		RW          bool
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ms); err != nil {
		return nil, err
	}
	var ro []string
	for _, x := range ms {
		if !x.RW {
			ro = append(ro, x.Destination)
		}
	}
	return ro, nil
}
