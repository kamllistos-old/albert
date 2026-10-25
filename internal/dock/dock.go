// Package dock controls the Docker container that serves Albert's GUI.
package dock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kamllistos/albert/internal/runner"
)

// Container is the name of the GUI container.
const Container = "albert-ui"

const registryImage = "ghcr.io/kamllistos/albert-ui:"

// Image returns the image for a build version. "dev" builds are local-only.
func Image(version string) string {
	if version == "dev" {
		return "albert-ui:dev"
	}
	return registryImage + version
}

// Docker drives the docker CLI through a Runner.
type Docker struct {
	R runner.Runner

	// mkTemp creates the build-context directory; nil means os.MkdirTemp.
	// Tests replace it to get a predictable path.
	mkTemp func() (string, error)
}

// Ready reports whether docker is installed and its engine is answering.
func (d Docker) Ready(ctx context.Context) error {
	if _, err := d.R.LookPath("docker"); err != nil {
		return errors.New("docker is not installed")
	}
	res, err := d.R.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errors.New("docker is not running")
	}
	return nil
}

// HasImage reports whether image is already present locally.
func (d Docker) HasImage(ctx context.Context, image string) bool {
	res, err := d.R.Run(ctx, "docker", "image", "inspect", image)
	return err == nil && res.ExitCode == 0
}

// EnsureImage makes sure image exists locally: it uses an existing one,
// otherwise pulls it (registry images only), otherwise builds it from the web/
// and docker/ directories of files.
func (d Docker) EnsureImage(ctx context.Context, image string, files fs.FS) error {
	if res, err := d.R.Run(ctx, "docker", "image", "inspect", image); err != nil {
		return err
	} else if res.ExitCode == 0 {
		return nil
	}
	if strings.Contains(image, "/") {
		res, err := d.R.Run(ctx, "docker", "pull", image)
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			return nil
		}
	}
	return d.build(ctx, image, files)
}

func (d Docker) build(ctx context.Context, image string, files fs.FS) error {
	mk := d.mkTemp
	if mk == nil {
		mk = func() (string, error) { return os.MkdirTemp("", "albert-ui-build-") }
	}
	dir, err := mk()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := copyBuildContext(files, dir); err != nil {
		return fmt.Errorf("prepare image build: %w", err)
	}
	res, err := d.R.Run(ctx, "docker", "build", "-t", image, "-f", filepath.Join(dir, "docker", "Dockerfile"), dir)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker build failed: %s", output(res))
	}
	return nil
}

// Start (re)creates the GUI container. The container holds no secret: it only
// learns where the agent is.
func (d Docker) Start(ctx context.Context, image string, uiPort, agentPort int) error {
	// A stale container with the same name is fine to lose; ignore the result.
	_, _ = d.R.Run(ctx, "docker", "rm", "-f", Container)
	res, err := d.R.Run(ctx, "docker", "run", "-d", "--rm", "--name", Container,
		"-p", "127.0.0.1:"+strconv.Itoa(uiPort)+":80",
		"-e", "ALBERT_AGENT=host.docker.internal:"+strconv.Itoa(agentPort),
		image)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker run failed: %s", output(res))
	}
	return nil
}

// Stop stops the GUI container. A container that is already gone is fine.
func (d Docker) Stop(ctx context.Context) error {
	res, err := d.R.Run(ctx, "docker", "stop", Container)
	if err != nil {
		return err
	}
	if res.ExitCode == 0 || strings.Contains(string(res.Stderr), "No such container") {
		return nil
	}
	return fmt.Errorf("docker stop failed: %s", output(res))
}

// RemoveImages deletes Albert's window image for version, the local dev
// image and every registry tag of the window image, so an old version
// doesn't linger. An image that is already gone is fine. It tries every image
// and reports the ones it couldn't remove.
func (d Docker) RemoveImages(ctx context.Context, version string) error {
	var errs []error
	targets := []string{Image(version)}
	if !slices.Contains(targets, Image("dev")) {
		targets = append(targets, Image("dev"))
	}
	res, err := d.R.Run(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}")
	switch {
	case err != nil:
		return err
	case res.ExitCode != 0:
		errs = append(errs, fmt.Errorf("docker image ls failed: %s", output(res)))
	default:
		for _, line := range strings.Split(string(res.Stdout), "\n") {
			img := strings.TrimSpace(line)
			if strings.HasPrefix(img, registryImage) && !slices.Contains(targets, img) {
				targets = append(targets, img)
			}
		}
	}
	for _, img := range targets {
		res, err := d.R.Run(ctx, "docker", "rmi", img)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 && !strings.Contains(string(res.Stderr), "No such image") {
			errs = append(errs, fmt.Errorf("docker rmi %s failed: %s", img, output(res)))
		}
	}
	return errors.Join(errs...)
}

func output(res runner.Result) string {
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stdout))
	}
	return msg
}

// copyBuildContext copies the web/ and docker/ trees of files into dst.
func copyBuildContext(files fs.FS, dst string) error {
	for _, root := range []string{"web", "docker"} {
		err := fs.WalkDir(files, root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			target := filepath.Join(dst, filepath.FromSlash(path.Clean(p)))
			if e.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			b, err := fs.ReadFile(files, p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
