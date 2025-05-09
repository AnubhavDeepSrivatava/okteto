// Copyright 2023 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manifest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	buildv2 "github.com/okteto/okteto/cmd/build/v2"
	contextCMD "github.com/okteto/okteto/cmd/context"
	"github.com/okteto/okteto/cmd/deploy"
	pipelineCMD "github.com/okteto/okteto/cmd/pipeline"
	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/pkg/analytics"
	initCMD "github.com/okteto/okteto/pkg/cmd/init"
	"github.com/okteto/okteto/pkg/cmd/pipeline"
	"github.com/okteto/okteto/pkg/constants"
	"github.com/okteto/okteto/pkg/devenvironment"
	"github.com/okteto/okteto/pkg/discovery"
	"github.com/okteto/okteto/pkg/k8s/apps"
	"github.com/okteto/okteto/pkg/linguist"
	oktetoLog "github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/log/io"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/okteto"
	"github.com/okteto/okteto/pkg/registry"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

type analyticsTrackerInterface interface {
	TrackImageBuild(meta ...*analytics.ImageBuildMetadata)
}

// ManifestCommand has all the namespaces subcommands
type ManifestCommand struct {
	manifest          *model.Manifest
	K8sClientProvider okteto.K8sClientProvider
	analyticsTracker  analyticsTrackerInterface

	ioCtrl *io.IOController
}

// InitOpts defines the option for manifest init
type InitOpts struct {
	DevPath   string
	Namespace string
	Context   string
	Language  string
	Workdir   string

	Overwrite bool
	ShowCTA   bool
	Version1  bool

	AutoDeploy       bool
	AutoConfigureDev bool
}

// Init automatically generates the manifest
func Init(at analyticsTrackerInterface, ioCtrl *io.IOController) *cobra.Command {
	opts := &InitOpts{}
	cmd := &cobra.Command{
		Use:   "init",
		Args:  utils.NoArgsAccepted("https://okteto.com/docs/reference/cli/#init"),
		Short: "Automatically generate your okteto manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			ctxResource := &model.ContextResource{}
			if err := ctxResource.UpdateNamespace(opts.Namespace); err != nil {
				return err
			}

			if err := ctxResource.UpdateContext(opts.Context); err != nil {
				return err
			}
			ctxOptions := &contextCMD.ContextOptions{
				Context:   ctxResource.Context,
				Namespace: ctxResource.Namespace,
				Show:      true,
			}
			if err := contextCMD.NewContextCommand().Run(ctx, ctxOptions); err != nil {
				return err
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			opts.Workdir = cwd
			opts.ShowCTA = oktetoLog.IsInteractive()
			mc := &ManifestCommand{
				K8sClientProvider: okteto.NewK8sClientProvider(),
				analyticsTracker:  at,
				ioCtrl:            ioCtrl,
			}
			if opts.Version1 {
				if err := mc.RunInitV1(ctx, opts); err != nil {
					return err
				}
			} else {
				_, err := mc.RunInitV2(ctx, opts)
				return err
			}
			return err
		},
	}

	cmd.Flags().StringVarP(&opts.Namespace, "namespace", "n", "", "namespace target for generating the okteto manifest")
	cmd.Flags().StringVarP(&opts.Context, "context", "c", "", "context target for generating the okteto manifest")
	cmd.Flags().StringVarP(&opts.DevPath, "file", "f", utils.DefaultManifest, "path to the manifest file")
	cmd.Flags().BoolVarP(&opts.Overwrite, "replace", "r", false, "overwrite existing manifest file")
	cmd.Flags().BoolVarP(&opts.Version1, "v1", "", false, "create a v1 okteto manifest: https://www.okteto.com/docs/reference/manifest/")
	cmd.Flags().BoolVarP(&opts.AutoDeploy, "deploy", "", false, "deploy the application after generate the okteto manifest if it's not running already")
	cmd.Flags().BoolVarP(&opts.AutoConfigureDev, "configure-devs", "", false, "configure devs after deploying the application")
	return cmd
}

// RunInitV2 initializes a new okteto manifest
func (mc *ManifestCommand) RunInitV2(ctx context.Context, opts *InitOpts) (*model.Manifest, error) {
	c, _, er := mc.K8sClientProvider.Provide(okteto.Context().Cfg)
	if er != nil {
		return nil, er
	}
	inferer := devenvironment.NewNameInferer(c)
	name := inferer.InferName(ctx, opts.Workdir, okteto.Context().Namespace, opts.DevPath)
	os.Setenv(constants.OktetoNameEnvVar, name)
	manifest := model.NewManifest()
	var err error
	if !opts.Overwrite {
		manifest, err = model.GetManifestV2(opts.DevPath)
		if err != nil && !errors.Is(err, discovery.ErrOktetoManifestNotFound) {
			return nil, err
		}
	}

	if manifest == nil || len(manifest.Build) == 0 || manifest.Deploy == nil {
		manifest, err = mc.configureManifestDeployAndBuild(opts.Workdir)
		if err != nil {
			return nil, err
		}
	}

	if manifest != nil {
		mc.manifest = manifest
		manifest.Name = os.Getenv(constants.OktetoNameEnvVar)
		if opts.Namespace == "" {
			manifest.Namespace = ""
		}
		if opts.Context == "" {
			manifest.Context = ""
		}

		if manifest.IsDeployDefault() && len(manifest.Build) == 1 {
			if err := configureAutoCreateDev(manifest); err != nil {
				return nil, err
			}
			manifest.Deploy = nil
			if err := manifest.WriteToFile(opts.DevPath); err != nil {
				return nil, err
			}
			oktetoLog.Success("Okteto manifest (%s) configured successfully", opts.DevPath)
			if opts.ShowCTA {
				oktetoLog.Information("Run 'okteto up' to activate your development container")
			}
			return manifest, nil
		}

		if err := manifest.WriteToFile(opts.DevPath); err != nil {
			return nil, err
		}
		oktetoLog.Success("Okteto manifest (%s) deploy and build configured successfully", opts.DevPath)

		c, _, err := mc.K8sClientProvider.Provide(okteto.Context().Cfg)
		if err != nil {
			return nil, err
		}
		namespace := manifest.Namespace
		if namespace == "" {
			namespace = okteto.Context().Namespace
		}
		isDeployed := pipeline.IsDeployed(ctx, manifest.Name, namespace, c)
		deployAnswer := false
		if !isDeployed && !opts.AutoDeploy {
			deployAnswer, err = utils.AskYesNo("Do you want to launch your development environment?", utils.YesNoDefault_Yes)
			if err != nil {
				return nil, err
			}
		}
		if deployAnswer || (!isDeployed && opts.AutoDeploy) {
			if err := mc.deploy(ctx, opts); err != nil {
				return nil, err
			}
			isDeployed = true
		}

		if isDeployed {
			configureDevEnvsAnswer := false
			if !opts.AutoConfigureDev {
				configureDevEnvsAnswer, err = utils.AskYesNo("Do you want to configure your development containers?", utils.YesNoDefault_Yes)
				if err != nil {
					return nil, err
				}
			}

			if configureDevEnvsAnswer || opts.AutoConfigureDev {
				if err := mc.configureDevsByResources(ctx, namespace); err != nil {
					return nil, err
				}
			}

			if err := manifest.WriteToFile(opts.DevPath); err != nil {
				return nil, err
			}
			oktetoLog.Success("Okteto manifest (%s) configured successfully", opts.DevPath)
			if opts.ShowCTA {
				if !configureDevEnvsAnswer {
					oktetoLog.Information("Run 'okteto init' to continue configuring your dev section")
				}
				oktetoLog.Information("Run 'okteto up' to activate your development container")
			}
		}
	}
	return manifest, nil
}

func (*ManifestCommand) configureManifestDeployAndBuild(cwd string) (*model.Manifest, error) {

	composeFiles := utils.GetStackFiles(cwd)
	if len(composeFiles) > 0 {
		composePath, err := selectComposeFile(composeFiles)
		if err != nil {
			return nil, err
		}
		if composePath != "" {
			answer, err := utils.AskYesNo("creating an okteto manifest is optional if you want to use a compose file. Do you want to continue?", utils.YesNoDefault_Yes)
			if err != nil {
				return nil, err
			}
			if !answer {
				return nil, nil
			}
			manifest, err := createFromCompose(composePath)
			if err != nil {
				return nil, err
			}
			return manifest, nil
		}
		manifest, err := createFromKubernetes(cwd)
		if err != nil {
			return nil, err
		}
		return manifest, nil

	}
	manifest, err := createFromKubernetes(cwd)
	if err != nil {
		return nil, err
	}
	return manifest, nil

}

func (mc *ManifestCommand) deploy(ctx context.Context, opts *InitOpts) error {
	pc, err := pipelineCMD.NewCommand()
	if err != nil {
		return err
	}
	c := &deploy.DeployCommand{
		GetManifest:        mc.getManifest,
		TempKubeconfigFile: deploy.GetTempKubeConfigFile(mc.manifest.Name),
		K8sClientProvider:  mc.K8sClientProvider,
		Builder:            buildv2.NewBuilderFromScratch(mc.analyticsTracker, mc.ioCtrl),
		GetExternalControl: deploy.NewDeployExternalK8sControl,
		Fs:                 afero.NewOsFs(),
		CfgMapHandler:      deploy.NewConfigmapHandler(mc.K8sClientProvider),
		PipelineCMD:        pc,
		DeployWaiter:       deploy.NewDeployWaiter(mc.K8sClientProvider),
		EndpointGetter:     deploy.NewEndpointGetter,
	}

	err = c.RunDeploy(ctx, &deploy.Options{
		Name:         mc.manifest.Name,
		ManifestPath: opts.DevPath,
		Timeout:      5 * time.Minute,
		Build:        false,
		Wait:         false,
	})
	if err != nil {
		return err
	}
	return nil
}

func (mc *ManifestCommand) configureDevsByResources(ctx context.Context, namespace string) error {
	c, _, err := okteto.GetK8sClient()
	if err != nil {
		return err
	}

	dList, err := pipeline.ListDeployments(ctx, mc.manifest.Name, namespace, c)
	if err != nil {
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, d := range dList {
		app := apps.NewDeploymentApp(&d)
		if apps.IsDevModeOn(app) {
			oktetoLog.Infof("App '%s' is in dev mode", app.ObjectMeta().Name)
			continue
		}
		container := ""
		if len(app.PodSpec().Containers) > 1 {
			container = app.PodSpec().Containers[0].Name
		}

		suffix := fmt.Sprintf("Analyzing %s '%s'...", strings.ToLower(app.Kind()), app.ObjectMeta().Name)
		oktetoLog.Spinner(suffix)
		oktetoLog.StartSpinner()
		defer oktetoLog.StopSpinner()

		path := getPathFromApp(wd, app.ObjectMeta().Name)

		language, err := getLanguageFromPath(wd, path)
		if err != nil {
			return err
		}

		configFromImage, err := initCMD.GetDevDefaultsFromImage(app)
		if err != nil {
			return err
		}
		dev, err := linguist.GetDevDefaults(language, path, configFromImage)
		if err != nil {
			return err
		}
		setFromImageConfig(dev, configFromImage)
		initCMD.SetImage(dev, language, path)
		err = initCMD.SetDevDefaultsFromApp(ctx, dev, app, container, language, path)
		if err != nil {
			oktetoLog.Infof("could not get defaults from app: %s", err.Error())
		}
		oktetoLog.Success("Development container '%s' configured successfully", app.ObjectMeta().Name)
		mc.manifest.Dev[app.ObjectMeta().Name] = dev
	}
	return nil
}

func setFromImageConfig(dev *model.Dev, imageConfig registry.ImageMetadata) {
	if len(dev.Command.Values) == 0 && len(imageConfig.CMD) > 0 {
		dev.Command = model.Command{Values: imageConfig.CMD}
	}

	if imageConfig.Workdir != "" {
		dev.Workdir = imageConfig.Workdir
	}
}

func getLanguageFromPath(wd, appName string) (string, error) {
	possibleAppPath := filepath.Join(wd, appName)
	language := ""
	var err error
	if fInfo, err := os.Stat(possibleAppPath); err != nil {
		oktetoLog.Infof("could not detect path: %s", err)
	} else if fInfo.IsDir() {
		language, err = GetLanguage("", possibleAppPath)
		if err != nil {
			return "", err
		}
	}
	if language == "" {
		language, err = GetLanguage("", wd)
		if err != nil {
			return "", err
		}
	}
	return language, nil
}

func getPathFromApp(wd, appName string) string {
	possibleAppPath := filepath.Join(wd, appName)

	if fInfo, err := os.Stat(possibleAppPath); err != nil {
		oktetoLog.Infof("could not detect path: %s", err)
	} else if fInfo.IsDir() {
		path, err := filepath.Rel(wd, possibleAppPath)
		if err != nil {
			oktetoLog.Infof("could not get relative path: %s", err)
		}
		return path
	}
	return wd
}

func createFromCompose(composePath string) (*model.Manifest, error) {
	stack, err := model.LoadStack("", []string{composePath}, true)
	if err != nil {
		return nil, err
	}
	manifest := &model.Manifest{
		Type: model.StackType,
		Deploy: &model.DeployInfo{
			ComposeSection: &model.ComposeSectionInfo{
				ComposesInfo: []model.ComposeInfo{
					{File: composePath},
				},
				Stack: stack,
			},
		},
		Dev:   model.ManifestDevs{},
		Build: model.ManifestBuild{},
		IsV2:  true,
	}
	cwd, err := os.Getwd()
	if err != nil {
		oktetoLog.Info("could not detect working directory")
	}
	manifest, err = manifest.InferFromStack(cwd)
	if err != nil {
		return nil, err
	}
	manifest.Context = okteto.Context().Name
	manifest.Namespace = okteto.Context().Namespace

	for _, build := range manifest.Build {
		context, err := filepath.Abs(build.Context)
		if err != nil {
			return nil, fmt.Errorf("can not get absolute path of %s", build.Context)
		}
		build.Context, err = filepath.Rel(cwd, context)
		if err != nil {
			return nil, fmt.Errorf("can not set the relative path of '%s' from your current working directory: '%s'", build.Context, cwd)
		}
		dockerfile, err := filepath.Abs(build.Dockerfile)
		if err != nil {
			return nil, fmt.Errorf("can not get absolute path of %s", build.Context)
		}
		build.Dockerfile, err = filepath.Rel(cwd, dockerfile)
		if err != nil {
			return nil, fmt.Errorf("can not set the relative path of '%s' from your current working directory: '%s'", build.Context, cwd)
		}
	}
	return manifest, err
}

func createFromKubernetes(cwd string) (*model.Manifest, error) {
	manifest := model.NewManifest()
	dockerfiles, err := selectDockerfiles(cwd)
	if err != nil {
		return nil, err
	}
	manifest.Build, err = inferBuildSectionFromDockerfiles(cwd, dockerfiles)
	if err != nil {
		return nil, err
	}
	manifest.Deploy, err = inferDeploySection(cwd)
	if err != nil {
		return nil, err
	}
	manifest.Dev, err = inferDevsSection(cwd)
	if err != nil {
		return nil, err
	}

	return manifest, nil
}

func inferBuildSectionFromDockerfiles(cwd string, dockerfiles []string) (model.ManifestBuild, error) {
	manifestBuild := model.ManifestBuild{}
	for _, dockerfile := range dockerfiles {
		var name string
		var buildInfo *model.BuildInfo
		if dockerfile == dockerfileName {
			c, _, err := okteto.NewK8sClientProvider().Provide(okteto.Context().Cfg)
			if err != nil {
				return nil, err
			}
			inferer := devenvironment.NewNameInferer(c)
			// In this case, the path is empty because we are inferring the names from Dockerfiles, so no manifest
			name = inferer.InferName(context.Background(), cwd, okteto.Context().Namespace, "")
			buildInfo = &model.BuildInfo{
				Context:    ".",
				Dockerfile: dockerfile,
			}
		} else {
			name = filepath.Dir(dockerfile)
			buildInfo = &model.BuildInfo{
				Context:    filepath.Dir(dockerfile),
				Dockerfile: dockerfile,
			}
		}
		if !okteto.IsOkteto() {
			imageName, err := utils.AsksQuestion(fmt.Sprintf("Which is the image name for %s: ", dockerfile))
			if err != nil {
				return nil, err
			}
			buildInfo.Image = imageName
		}
		manifestBuild[name] = buildInfo
	}
	return manifestBuild, nil
}

func inferDeploySection(cwd string) (*model.DeployInfo, error) {
	m, err := model.GetInferredManifest(cwd)
	if err != nil {
		return nil, err
	}
	if m != nil && m.Deploy != nil {
		return m.Deploy, nil
	}
	return &model.DeployInfo{
		Commands: []model.DeployCommand{
			{
				Name:    "Deploy",
				Command: model.FakeCommand,
			},
		},
	}, nil
}

func inferDevsSection(cwd string) (model.ManifestDevs, error) {
	files, err := os.ReadDir(cwd)
	if err != nil {
		return nil, err
	}

	devs := model.ManifestDevs{}
	for _, f := range files {
		if !f.IsDir() {
			continue
		}
		dev, err := model.GetManifestV2(f.Name())
		if err != nil {
			oktetoLog.Debugf("could not detect any okteto manifest on %s", f.Name())
			continue
		}
		if !dev.IsV2 && len(dev.Dev) != 0 {
			for devName, d := range dev.Dev {
				devs[devName] = d
			}
		}
	}
	return devs, nil
}

func (mc *ManifestCommand) getManifest(path string) (*model.Manifest, error) {
	if mc.manifest != nil {
		// Deepcopy so it does not get overwritten these changes
		manifest := *mc.manifest
		b := model.ManifestBuild{}
		for k, v := range mc.manifest.Build {
			info := *v
			b[k] = &info
		}
		manifest.Build = b
		d := model.NewDeployInfo()
		if mc.manifest.Deploy != nil {
			copy(d.Commands, mc.manifest.Deploy.Commands)
			d.Endpoints = mc.manifest.Deploy.Endpoints
			d.ComposeSection = mc.manifest.Deploy.ComposeSection
		}
		manifest.Deploy = d
		return &manifest, nil
	}
	return model.GetManifestV2(path)
}

func configureAutoCreateDev(manifest *model.Manifest) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	language, err := GetLanguage("", wd)
	if err != nil {
		return err
	}

	dev, err := linguist.GetDevDefaults(language, wd, registry.ImageMetadata{})
	if err != nil {
		return err
	}

	dev.Autocreate = true
	linguist.SetForwardDefaults(dev, language)
	manifest.Dev[dev.Name] = dev
	return nil
}
