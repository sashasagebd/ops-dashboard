package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"
)

// BuildError is a build that ran but failed (e.g. a RUN step exited non-zero
// or the Dockerfile is missing), as opposed to Docker being unreachable. Its
// message comes from Docker and is safe to show in the dashboard.
type BuildError struct {
	Message string
}

func (e *BuildError) Error() string { return "build failed: " + e.Message }

// Build builds the Dockerfile at the root of buildContext (a tar stream) and
// tags the image tag.
//
// It uses Docker's legacy builder, deliberately. BuildKit through the API
// needs a client-side session to look up base images (CI showed "no active
// sessions" even for a public image), which would mean the large
// moby/buildkit library plus another proxy permission. The legacy builder
// pulls base images itself, through /build alone. It's deprecated (since
// Docker 23, no removal date) and lacks newer Dockerfile features such as
// RUN --mount; switching to BuildKit with a session later only changes this
// function. See docs/decisions/platform.md.
func (c *Client) Build(ctx context.Context, buildContext io.Reader, tag string, labels map[string]string) error {
	res, err := c.api.ImageBuild(ctx, buildContext, client.ImageBuildOptions{
		Version: build.BuilderV1,
		Tags:    []string{tag},
		Labels:  labels,
		// Clean up intermediate containers, even when the build fails.
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		return fmt.Errorf("starting build: %w", err)
	}
	defer res.Body.Close()
	return readBuildOutput(res.Body)
}

// buildMessage is the part of Docker's build output this package reads. The
// output is a stream of JSON objects: progress as plain-text "stream" lines
// (kept for live logs in P2), and a failure as "error"/"errorDetail". The
// HTTP status is 200 either way, since it's sent before the build starts.
type buildMessage struct {
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// readBuildOutput consumes the whole stream (the build runs until it's read
// to the end) and returns the first error reported in it.
func readBuildOutput(r io.Reader) error {
	dec := json.NewDecoder(r)
	var buildErr *BuildError
	for {
		var m buildMessage
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading build output: %w", err)
		}
		if buildErr != nil {
			continue
		}
		switch {
		case m.ErrorDetail != nil && m.ErrorDetail.Message != "":
			buildErr = &BuildError{Message: m.ErrorDetail.Message}
		case m.Error != "":
			buildErr = &BuildError{Message: m.Error}
		}
	}
	if buildErr != nil {
		return buildErr
	}
	return nil
}
