package dockerengine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

type recordingAPI struct {
	create    client.ContainerCreateOptions
	attachErr error
	removed   []string
}

func (*recordingAPI) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{}, nil
}
func (a *recordingAPI) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	a.create = options
	return client.ContainerCreateResult{ID: "container-id"}, nil
}
func (a *recordingAPI) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	if a.attachErr != nil {
		return client.ContainerAttachResult{}, a.attachErr
	}
	worker, peer := net.Pipe()
	go peer.Close()
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(worker, "")}, nil
}
func (*recordingAPI) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	failures := make(chan error, 1)
	result <- container.WaitResponse{StatusCode: 0}
	return client.ContainerWaitResult{Result: result, Error: failures}
}
func (*recordingAPI) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, nil
}
func (a *recordingAPI) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	a.removed = append(a.removed, id)
	return client.ContainerRemoveResult{}, nil
}
func (*recordingAPI) Close() error { return nil }

func TestRunBuildsExactIsolatedContainerProfile(t *testing.T) {
	api := &recordingAPI{}
	engine := &Client{api: api}
	request := RunRequest{Name: "worker", Image: "sha256:image", User: "1000:1000",
		WorkingDir: "/workspace", Env: []string{"PATH=/bin"},
		Mounts: []Mount{{Source: "/host", Target: "/workspace", ReadOnly: true}},
		Tmpfs:  map[string]string{"/tmp": "rw,noexec"}, NanoCPUs: 750_000_000,
		Memory: 512 << 20, Pids: 64}
	if err := engine.Run(t.Context(), request, bytes.NewReader(nil), io.Discard); err != nil {
		t.Fatal(err)
	}
	pids := int64(64)
	want := client.ContainerCreateOptions{
		Name: "worker",
		Config: &container.Config{
			Image: "sha256:image", User: "1000:1000", WorkingDir: "/workspace",
			Env: []string{"PATH=/bin"}, AttachStdin: true, AttachStdout: true,
			AttachStderr: true, OpenStdin: true, StdinOnce: true, NetworkDisabled: true,
		},
		HostConfig: &container.HostConfig{
			AutoRemove: true, NetworkMode: "none", ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			Tmpfs: map[string]string{"/tmp": "rw,noexec"},
			Mounts: []mount.Mount{{
				Type: mount.TypeBind, Source: "/host", Target: "/workspace", ReadOnly: true,
			}},
			Resources: container.Resources{
				NanoCPUs: 750_000_000, Memory: 512 << 20, PidsLimit: &pids,
			},
		},
	}
	if !reflect.DeepEqual(api.create, want) {
		t.Fatalf("container profile=%+v want=%+v", api.create, want)
	}
}

func TestRunForceRemovesCreatedContainerAfterAttachFailure(t *testing.T) {
	api := &recordingAPI{attachErr: errors.New("attach failed")}
	err := (&Client{api: api}).Run(t.Context(), RunRequest{Name: "worker"}, bytes.NewReader(nil), io.Discard)
	if err == nil || len(api.removed) != 1 || api.removed[0] != "container-id" {
		t.Fatalf("error=%v removed=%v", err, api.removed)
	}
}
