//go:generate go run github.com/golang/mock/mockgen -source builder.go -package builder -destination builder_mock.go ComponentBuilderFactory ComponentBuilder

package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/digitalocean/doctl/commands/charm"
	"github.com/digitalocean/doctl/commands/charm/template"
	"github.com/digitalocean/godo"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/stdcopy"
)

// ComponentBuilderFactory is the interface for creating a component builder.
type ComponentBuilderFactory interface {
	NewComponentBuilder(DockerEngineClient, string, *godo.AppSpec, NewBuilderOpts) (ComponentBuilder, error)
}

// ComponentBuilder is the interface of building one or more components.
type ComponentBuilder interface {
	Build(context.Context) (ComponentBuilderResult, error)
}

// ComponentBuilderResult ...
type ComponentBuilderResult struct {
	Image         string
	BuildDuration time.Duration
	ExitCode      int
}

type baseComponentBuilder struct {
	cli                  DockerEngineClient
	contextDir           string
	spec                 *godo.AppSpec
	component            godo.AppBuildableComponentSpec
	registry             string
	envOverrides         map[string]string
	buildCommandOverride string
	copyOnWriteSemantics bool
	noCache              bool

	logWriter io.Writer
}

func (b baseComponentBuilder) AppImageOutputName() string {
	ref := fmt.Sprintf("%s:dev", b.component.GetName())
	if b.registry != "" {
		ref = fmt.Sprintf("%s/%s", b.registry, ref)
	}

	return ref
}

func (b baseComponentBuilder) StaticSiteImageOutputName() string {
	return b.AppImageOutputName() + "-static"
}

func (b baseComponentBuilder) getLogWriter() io.Writer {
	if b.logWriter == nil {
		return os.Stdout
	}
	return b.logWriter
}

func (b baseComponentBuilder) getEnvMap() (map[string]string, error) {
	envMap := map[string]string{}
	lw := b.getLogWriter()
	template.Render(lw, `{{success checkmark}} configuring build environment variables{{nl}}`, nil)
	subLW := charm.IndentWriter(lw, 2)

	addEnvs := func(envs ...*godo.AppVariableDefinition) {
		for _, e := range envs {
			if e.Type == godo.AppVariableType_Secret {
				template.Render(subLW, `{{success checkmark}} ignoring SECRET variable {{highlight .GetKey}}{{nl}}`, e)
				continue
			}
			if e.Scope != godo.AppVariableScope_RunTime {
				val := e.Value
				envMap[e.Key] = val
			}
		}
	}

	addEnvs(b.spec.GetEnvs()...)
	addEnvs(b.component.GetEnvs()...)

	for k, v := range b.envOverrides {
		v := v
		_, exists := envMap[k]
		if !exists {
			// TODO: if interactive prompt to auto add to spec
			return nil, fmt.Errorf("variable not in found in app spec: %s", k)
		}
		template.Render(subLW, `{{success checkmark}} overriding value for variable {{highlight .}}{{nl}}`, k)
		envMap[k] = v
	}

	return envMap, nil
}

func (b *baseComponentBuilder) imageExists(ctx context.Context, ref string) (bool, error) {
	images, err := b.cli.ImageList(ctx, types.ImageListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", ref)),
	})
	if err != nil {
		return false, fmt.Errorf("checking if container image exists: %w", err)
	}
	return len(images) > 0, nil
}

func (b *baseComponentBuilder) getStaticNginxConfig() string {
	return `
server {
	listen 8080;
	listen [::]:8080;

	resolver 127.0.0.11;
	autoindex off;

	server_name _;
	server_tokens off;

	root /www;
	gzip_static on;
}
`
}

// ContainerExecError contains additional data on a container exec failure.
type ContainerExecError struct {
	Err      error
	ExitCode int
}

func (e ContainerExecError) Error() string {
	return e.Err.Error()
}

func (b *baseComponentBuilder) runExec(ctx context.Context, containerID string, command, env []string, w io.Writer) error {
	execRes, err := b.cli.ContainerExecCreate(ctx, containerID, types.ExecConfig{
		AttachStderr: true,
		AttachStdout: true,
		Env:          env,
		Cmd:          command,
	})
	if err != nil {
		return fmt.Errorf("creating container exec: %w", err)
	}

	// read the output
	attachRes, err := b.cli.ContainerExecAttach(ctx, execRes.ID, types.ExecStartCheck{})
	if err != nil {
		return fmt.Errorf("attaching to container exec: %w", err)
	}
	defer attachRes.Close()
	outputDone := make(chan error)

	go func() {
		// StdCopy demultiplexes the stream into two separate stdout and stderr buffers
		_, err := stdcopy.StdCopy(w, w, attachRes.Reader)
		outputDone <- err
	}()

	select {
	case err = <-outputDone:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return err
	}

	// the exec process completed. check its exit code and return an error if it failed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := b.cli.ContainerExecInspect(ctx, execRes.ID)
	if err != nil {
		// graceful failure
		template.Render(w, "{{warning crossmark}} inspecting container: {{warning .}}{{nl}}", err)
		return nil
	} else if res.ExitCode > 0 {
		return ContainerExecError{
			Err:      fmt.Errorf("command exited with a non-zero status code"),
			ExitCode: res.ExitCode,
		}
	}

	return nil
}

// NewBuilderOpts ...
type NewBuilderOpts struct {
	Component            string
	Registry             string
	EnvOverride          map[string]string
	BuildCommandOverride string
	LogWriter            io.Writer
	Versioning           Versioning
	LocalCacheDir        string
	NoCache              bool
}

type Versioning struct {
	CNB *godo.AppBuildConfigCNBVersioning
}

// DefaultComponentBuilderFactory is the standard component builder factory.
type DefaultComponentBuilderFactory struct{}

// NewComponentBuilder returns the correct builder type depending upon the provided
// app and component.
func (f *DefaultComponentBuilderFactory) NewComponentBuilder(cli DockerEngineClient, contextDir string, spec *godo.AppSpec, opts NewBuilderOpts) (ComponentBuilder, error) {
	if opts.Component == "" {
		return nil, errors.New("component is required")
	}

	component, err := godo.GetAppSpecComponent[godo.AppBuildableComponentSpec](spec, opts.Component)
	if err != nil {
		return nil, err
	}
	if component == nil {
		return nil, fmt.Errorf("component %s does not exist", opts.Component)
	}

	// NOTE(ntate); We don't provide this as a configureable argument today.
	// We always assume we want copy-on-write. Caching occurs through re-use of the built OCI image.
	// This may change in the future so we provide as an argument to the baseComponentBuilder.
	copyOnWriteSemantics := true

	base := baseComponentBuilder{
		cli,
		contextDir,
		spec,
		component,
		opts.Registry,
		opts.EnvOverride,
		opts.BuildCommandOverride,
		copyOnWriteSemantics,
		opts.NoCache,
		opts.LogWriter,
	}

	if component.GetDockerfilePath() == "" {
		var cnbVersioning CNBVersioning
		for _, bp := range opts.Versioning.CNB.GetBuildpacks() {
			cnbVersioning.Buildpacks = append(cnbVersioning.Buildpacks, &Buildpack{
				ID:      bp.ID,
				Version: fmt.Sprintf("%d.0.0", bp.MajorVersion),
			})
		}

		return &CNBComponentBuilder{
			baseComponentBuilder: base,
			versioning:           cnbVersioning,
			localCacheDir:        opts.LocalCacheDir,
		}, nil
	}

	return &DockerComponentBuilder{base}, nil
}
