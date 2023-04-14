// Copyright © 2022 sealos.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package buildah

import (
	"fmt"
	"path"
	"runtime"
	"strings"

	"github.com/labring/sealos/pkg/utils/exec"
	"github.com/labring/sealos/pkg/utils/file"
	"github.com/labring/sealos/pkg/utils/rand"

	"github.com/containerd/containerd/platforms"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/image/v5/types"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/labring/sealos/pkg/buildimage"
	"github.com/labring/sealos/pkg/constants"
	"github.com/labring/sealos/pkg/registry"
	"github.com/labring/sealos/pkg/utils/logger"
)

type saveOptions struct {
	maxPullProcs int
	enabled      bool
	compress     bool
}

func (opts *saveOptions) RegisterFlags(fs *pflag.FlagSet) {
	fs.IntVar(&opts.maxPullProcs, "max-pull-procs", 5, "maximum number of goroutines for pulling")
	fs.BoolVar(&opts.enabled, "save-image", true, "save images parsed to local")
	fs.BoolVar(&opts.compress, "compress", false, "save images with compress")
}

const (
	defaultTarRegistry = "rm -rf %[1]s/* && tar -czf %[2]s  --directory=%[3]s docker && rm -rf %[3]s/docker"
)

func runSaveImages(contextDir string, platforms []v1.Platform, sys *types.SystemContext, opts *saveOptions) error {
	if !opts.enabled {
		logger.Warn("save-image is disabled, skip pulling images")
		return nil
	}
	registryDir := path.Join(contextDir, constants.RegistryDirName)
	compress := func() error {
		if opts.compress {
			logger.Debug("build images using compress mode, compress file in %s/compressed dir", registryDir)
			if file.IsExist(path.Join(registryDir, "docker")) {
				if err := file.MkDirs(path.Join(registryDir, "compressed")); err != nil {
					return err
				}
				registryHash := fmt.Sprintf("compressed-%s", rand.Generator(16))
				compressedFile := fmt.Sprintf("%s/%s/%s", registryDir, "compressed", registryHash)
				return exec.Cmd("bash", "-c", fmt.Sprintf(defaultTarRegistry, path.Join(registryDir, "compressed"), compressedFile, registryDir))
			}
		}
		return nil
	}
	images, err := buildimage.List(contextDir)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return compress()
	}
	auths, err := registry.GetAuthInfo(sys)
	if err != nil {
		return err
	}
	is := registry.NewImageSaver(getContext(), opts.maxPullProcs, auths)

	for _, pf := range platforms {
		logger.Debug("pull images %v for platform %s", images, strings.Join([]string{pf.OS, pf.Architecture}, "/"))
		images, err = is.SaveImages(images, registryDir, pf)
		if err != nil {
			return fmt.Errorf("failed to save images: %w", err)
		}
		logger.Info("saving images %s", strings.Join(images, ", "))
	}
	return compress()
}

func parsePlatforms(c *cobra.Command) ([]v1.Platform, error) {
	parsedPlatforms, err := parse.PlatformsFromOptions(c)
	if err != nil {
		return nil, err
	}
	// flags are not modified, use local platform
	switch len(parsedPlatforms) {
	case 0:
		return []v1.Platform{platforms.DefaultSpec()}, nil
	case 1:
		var platform v1.Platform
		idx0 := parsedPlatforms[0]
		if idx0.OS != "" {
			platform.OS = idx0.OS
		} else {
			platform.OS = runtime.GOOS
		}
		if idx0.Arch != "" {
			platform.Architecture = idx0.Arch
		} else {
			platform.Architecture = runtime.GOARCH
		}
		if idx0.Variant != "" {
			platform.Variant = idx0.Variant
		} else {
			platform.Variant = platforms.DefaultSpec().Variant
		}
		return []v1.Platform{platform}, nil
	}

	var ret []v1.Platform
	for _, pf := range parsedPlatforms {
		ret = append(ret, v1.Platform{Architecture: pf.Arch, OS: pf.OS, Variant: pf.Variant})
	}
	return ret, nil
}