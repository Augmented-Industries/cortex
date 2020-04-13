/*
Copyright 2020 Cortex Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cortexlabs/cortex/cli/docker"
	"github.com/cortexlabs/cortex/pkg/consts"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/exit"
	"github.com/cortexlabs/cortex/pkg/lib/files"
	"github.com/cortexlabs/cortex/pkg/types/clusterconfig"
	"github.com/cortexlabs/yaml"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func runManager(containerConfig *container.Config) (string, *int, error) {
	containerConfig.Env = append(containerConfig.Env, "CORTEX_CLI_VERSION="+consts.CortexVersion)

	// Add a slight delay before running the command to ensure logs don't start until after the container is attached
	containerConfig.Cmd[0] = "sleep 0.1 && /root/check_cortex_version.sh && " + containerConfig.Cmd[0]

	dockerClient, err := docker.GetDockerClient()
	if err != nil {
		return "", nil, err
	}

	err = docker.PullImage(containerConfig.Image)
	if err != nil {
		return "", nil, err
	}

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: _localDir,
				Target: "/.cortex",
			},
		},
	}

	containerInfo, err := dockerClient.ContainerCreate(context.Background(), containerConfig, hostConfig, nil, "")
	if err != nil {
		return "", nil, docker.WrapDockerError(err)
	}

	removeContainer := func() {
		dockerClient.ContainerRemove(context.Background(), containerInfo.ID, dockertypes.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})
	}

	defer removeContainer()

	// Make sure to remove container immediately on ctrl+c
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	caughtCtrlC := false
	go func() {
		<-c
		caughtCtrlC = true
		removeContainer()
		exit.Error(ErrorDockerCtrlC())
	}()

	err = dockerClient.ContainerStart(context.Background(), containerInfo.ID, dockertypes.ContainerStartOptions{})
	if err != nil {
		return "", nil, docker.WrapDockerError(err)
	}

	// Use ContainerAttach() since that allow logs to be streamed even if they don't end in new lines
	logsOutput, err := dockerClient.ContainerAttach(context.Background(), containerInfo.ID, dockertypes.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return "", nil, docker.WrapDockerError(err)
	}
	defer logsOutput.Close()

	var outputBuffer bytes.Buffer
	tee := io.TeeReader(logsOutput.Reader, &outputBuffer)

	_, err = io.Copy(os.Stdout, tee)
	if err != nil && err != io.EOF {
		return "", nil, errors.WithStack(err)
	}

	output := outputBuffer.String()

	// Let the ctrl+c handler run its course
	if caughtCtrlC {
		time.Sleep(5 * time.Second)
	}

	info, err := dockerClient.ContainerInspect(context.Background(), containerInfo.ID)
	if err != nil {
		return "", nil, errors.WithStack(err)
	}

	if info.State.Running {
		return output, nil, nil
	}

	return output, &info.State.ExitCode, nil
}

func runManagerUpdateCommand(entrypoint string, clusterConfig *clusterconfig.Config, awsCreds AWSCredentials) (string, *int, error) {
	clusterConfigBytes, err := yaml.Marshal(clusterConfig)
	if err != nil {
		return "", nil, errors.WithStack(err)
	}

	cachedConfigPath := cachedClusterConfigPath(clusterConfig.ClusterName, *clusterConfig.Region)
	if err := files.WriteFile(clusterConfigBytes, cachedConfigPath); err != nil {
		return "", nil, err
	}

	mountedConfigPath := mountedClusterConfigPath(clusterConfig.ClusterName, *clusterConfig.Region)
	clusterWorkspace := strings.TrimSuffix(mountedConfigPath, ".yaml")

	containerConfig := &container.Config{
		Image:        clusterConfig.ImageManager,
		Entrypoint:   []string{"/bin/bash", "-c"},
		Cmd:          []string{fmt.Sprintf("eval $(python /root/cluster_config_env.py %s) && %s", mountedConfigPath, entrypoint)},
		Tty:          true,
		AttachStdout: true,
		AttachStderr: true,
		Env: []string{
			"CORTEX_ENV_NAME=" + _flagClusterEnv,
			"AWS_ACCESS_KEY_ID=" + awsCreds.AWSAccessKeyID,
			"AWS_SECRET_ACCESS_KEY=" + awsCreds.AWSSecretAccessKey,
			"CORTEX_AWS_ACCESS_KEY_ID=" + awsCreds.CortexAWSAccessKeyID,
			"CORTEX_AWS_SECRET_ACCESS_KEY=" + awsCreds.CortexAWSSecretAccessKey,
			"CORTEX_TELEMETRY_DISABLE=" + os.Getenv("CORTEX_TELEMETRY_DISABLE"),
			"CORTEX_TELEMETRY_SENTRY_DSN=" + os.Getenv("CORTEX_TELEMETRY_SENTRY_DSN"),
			"CORTEX_TELEMETRY_SEGMENT_WRITE_KEY=" + os.Getenv("CORTEX_TELEMETRY_SEGMENT_WRITE_KEY"),
			"CORTEX_CLUSTER_CONFIG_FILE=" + mountedConfigPath,
			"CORTEX_CLUSTER_WORKSPACE=" + clusterWorkspace,
		},
	}

	output, exitCode, err := runManager(containerConfig)
	if err != nil {
		return "", nil, err
	}

	return output, exitCode, nil
}

func runManagerAccessCommand(entrypoint string, accessConfig clusterconfig.AccessConfig, awsCreds AWSCredentials) (string, *int, error) {
	containerConfig := &container.Config{
		Image:        accessConfig.ImageManager,
		Entrypoint:   []string{"/bin/bash", "-c"},
		Cmd:          []string{entrypoint},
		Tty:          true,
		AttachStdout: true,
		AttachStderr: true,
		Env: []string{
			"CORTEX_ENV_NAME=" + _flagClusterEnv,
			"AWS_ACCESS_KEY_ID=" + awsCreds.AWSAccessKeyID,
			"AWS_SECRET_ACCESS_KEY=" + awsCreds.AWSSecretAccessKey,
			"CORTEX_AWS_ACCESS_KEY_ID=" + awsCreds.CortexAWSAccessKeyID,
			"CORTEX_AWS_SECRET_ACCESS_KEY=" + awsCreds.CortexAWSSecretAccessKey,
			"CORTEX_CLUSTER_NAME=" + *accessConfig.ClusterName,
			"CORTEX_REGION=" + *accessConfig.Region,
		},
	}

	output, exitCode, err := runManager(containerConfig)
	if err != nil {
		return "", nil, err
	}

	return output, exitCode, nil
}
