// Package dockerengine provides the narrow Docker Engine boundary used by the
// packaged workflow service. It deliberately exposes no general-purpose
// container administration surface.
package dockerengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// Mount is one exact bind mount granted to a worker.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// RunRequest is the complete, closed worker-container profile.
type RunRequest struct {
	Name       string
	Image      string
	User       string
	WorkingDir string
	Env        []string
	Mounts     []Mount
	Tmpfs      map[string]string
	Memory     int64
	Pids       int64
}

// Engine is the consumer-facing worker Engine contract.
type Engine interface {
	ImageID(context.Context, string) (string, error)
	Run(context.Context, RunRequest, io.Reader, io.Writer) error
	Remove(context.Context, string) error
}

// Client is an API-version-negotiating Docker Engine client.
type dockerAPI interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	Close() error
}

type Client struct{ api dockerAPI }

// NewFromEnvironment connects using Docker's standard host configuration.
func NewFromEnvironment() (*Client, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Docker Engine client: %w", err)
	}
	return &Client{api: api}, nil
}

// Close releases client-side transport resources.
func (c *Client) Close() error { return c.api.Close() }

// ImageID resolves the daemon's immutable identity for image.
func (c *Client) ImageID(ctx context.Context, image string) (string, error) {
	result, err := c.api.ImageInspect(ctx, image)
	if err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	return result.ID, nil
}

// Run creates and starts one hardened worker, attaches its streams, and waits
// for its terminal status. Callers retain cancellation cleanup authority.
func (c *Client) Run(
	ctx context.Context, request RunRequest, input io.Reader, output io.Writer,
) (runErr error) {
	mounts := make([]mount.Mount, 0, len(request.Mounts))
	for _, item := range request.Mounts {
		mounts = append(mounts, mount.Mount{
			Type: mount.TypeBind, Source: item.Source, Target: item.Target,
			ReadOnly: item.ReadOnly,
		})
	}
	pids := request.Pids
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: request.Name,
		Config: &container.Config{
			Image: request.Image, User: request.User, WorkingDir: request.WorkingDir,
			Env: request.Env, AttachStdin: true, AttachStdout: true, AttachStderr: true,
			OpenStdin: true, StdinOnce: true, NetworkDisabled: true,
		},
		HostConfig: &container.HostConfig{
			AutoRemove: true, NetworkMode: "none", ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			Tmpfs: request.Tmpfs, Mounts: mounts,
			Resources: container.Resources{
				NanoCPUs: 1_000_000_000, Memory: request.Memory, PidsLimit: &pids,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create worker container: %w", err)
	}
	cleanupArmed := true
	defer func() {
		if !cleanupArmed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := c.api.ContainerRemove(
			cleanupCtx, created.ID, client.ContainerRemoveOptions{Force: true},
		); err != nil && runErr == nil {
			runErr = fmt.Errorf("remove worker container: %w", err)
		}
	}()
	attached, err := c.api.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("attach worker container: %w", err)
	}
	defer attached.Close()
	wait := c.api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionRemoved,
	})
	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start worker container: %w", err)
	}
	go func() {
		_, _ = io.Copy(attached.Conn, input)
		_ = attached.CloseWrite()
	}()
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(output, output, attached.Reader)
		copyDone <- err
	}()
	select {
	case <-ctx.Done():
		attached.Close()
		return ctx.Err()
	case err := <-wait.Error:
		if err != nil {
			return fmt.Errorf("wait for worker container: %w", err)
		}
		return errors.New("worker wait ended without status")
	case status := <-wait.Result:
		if err := <-copyDone; err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read worker output: %w", err)
		}
		if status.Error != nil {
			return fmt.Errorf("worker container: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return fmt.Errorf("worker container exited with status %d", status.StatusCode)
		}
		// The removed wait condition confirms terminal AutoRemove completed.
		cleanupArmed = false
		return nil
	}
}

// Remove force-removes a named worker after cancellation.
func (c *Client) Remove(ctx context.Context, name string) error {
	_, err := c.api.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	return err
}
