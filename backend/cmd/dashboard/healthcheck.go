package main

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// healthcheck asks a running dashboard on listenAddr whether it's serving,
// for the Dockerfile's HEALTHCHECK. The image is distroless, with no curl or
// shell to do this, so the binary checks itself: Docker runs
// `/dashboard -healthcheck` inside the container, with the same environment
// as the server, so listenAddr matches.
//
// It only checks /healthz (the process is up), not Docker: see handleHealthz.
func healthcheck(listenAddr string) error {
	url, err := healthURL(listenAddr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}

// healthURL turns a listen address into the URL to check. An address that
// listens on every interface (":8080", "0.0.0.0:8080", "[::]:8080") is
// checked over loopback; a specific host is used as is.
func healthURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("LISTEN_ADDR %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
