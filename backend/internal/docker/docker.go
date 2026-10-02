// Package docker reads container information from the Docker Engine API.
package docker

import "time"

// Container is a container as the dashboard sees it: only the fields we
// display, already cleaned up (e.g. the name has no leading "/").
type Container struct {
	ID     string
	Name   string
	Image  string
	State  string // machine-readable, e.g. "running", "exited"
	Status string // human-readable, e.g. "Up 3 hours"

	// StartedAt and FinishedAt are the zero time when they don't apply: a
	// container that was created but never started has neither, and a running
	// container that has never stopped has no FinishedAt.
	StartedAt  time.Time
	FinishedAt time.Time
}
