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
// It uses BuildKit, Docker's current builder, rather than the deprecated
// legacy one. Sent this way, with the context in the request body, BuildKit
// needs no session, so only the proxy's /build endpoint is used.
func (c *Client) Build(ctx context.Context, buildContext io.Reader, tag string, labels map[string]string) error {
	res, err := c.api.ImageBuild(ctx, buildContext, client.ImageBuildOptions{
		Version: build.BuilderBuildKit,
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
// output is a stream of JSON objects; with BuildKit, progress arrives as
// encoded "aux" trace messages (decoded for live logs in P2), and a failure
// as "error"/"errorDetail". The HTTP status is 200 either way, since it's
// sent before the build starts.
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
