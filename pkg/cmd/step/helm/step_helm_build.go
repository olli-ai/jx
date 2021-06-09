package helm

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/jenkins-x/jx-logging/pkg/log"
	"github.com/jenkins-x/jx/v2/pkg/cmd/helper"
	"github.com/jenkins-x/jx/v2/pkg/cmd/opts/step"
	"github.com/jenkins-x/jx/v2/pkg/config"
	"github.com/jenkins-x/jx/v2/pkg/helm"
	"github.com/jenkins-x/jx/v2/pkg/io/secrets"
	"github.com/pkg/errors"

	"github.com/jenkins-x/jx/v2/pkg/cmd/opts"
	"github.com/jenkins-x/jx/v2/pkg/cmd/templates"
	"github.com/spf13/cobra"
)

// StepHelmBuildOptions contains the command line flags
type StepHelmBuildOptions struct {
	StepHelmOptions

	Recursive         bool
	Boot              bool
	ProviderValuesDir string
	MultiTemplates    bool
}

var (
	StepHelmBuildLong = templates.LongDesc(`
		Builds the helm chart in a given directory.

		This step is usually used to validate any GitOps Pull Requests.
`)

	StepHelmBuildExample = templates.Examples(`
		# builds the helm chart in the env directory
		jx step helm build --dir env

`)
)

func NewCmdStepHelmBuild(commonOpts *opts.CommonOptions) *cobra.Command {
	options := StepHelmBuildOptions{
		StepHelmOptions: StepHelmOptions{
			StepOptions: step.StepOptions{
				CommonOptions: commonOpts,
			},
		},
	}
	cmd := &cobra.Command{
		Use:     "build",
		Short:   "Builds the helm chart in a given directory and validate the build completes",
		Aliases: []string{""},
		Long:    StepHelmBuildLong,
		Example: StepHelmBuildExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}

	options.addStepHelmFlags(cmd)

	cmd.Flags().BoolVarP(&options.Recursive, "recursive", "r", false, "Build recursively the dependent charts")
	cmd.Flags().BoolVarP(&options.Boot, "boot", "", false, "In Boot mode we load the Version Stream from the 'jx-requirements.yml' and use that to replace any missing versions in the 'reuqirements.yaml' file from the Version Stream")
	cmd.Flags().StringVarP(&options.ProviderValuesDir, "provider-values-dir", "", "", "The optional directory of kubernetes provider specific override values.tmpl.yaml files a kubernetes provider specific folder")
	cmd.Flags().BoolVarP(&options.MultiTemplates, "multi-templates", "", false, "Calls helm template on each sub chart instead of globally, to avoid conflict among charts with different versions")
	return cmd
}

func (o *StepHelmBuildOptions) Run() error {
	_, _, err := o.KubeClientAndNamespace()
	if err != nil {
		return err
	}

	dir := o.Dir
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	valuesFiles, err := o.discoverValuesFiles(dir)
	if err != nil {
		return err
	}

	if o.Boot {
		requirements, requirementsFileName, err := config.LoadRequirementsConfig(dir, config.DefaultFailOnValidationError)
		if err != nil {
			return err
		}

		secretURLClient, err := o.GetSecretURLClient(secrets.ToSecretsLocation(string(requirements.SecretStorage)))
		if err != nil {
			return errors.Wrap(err, "creating a Secret URL client")
		}

		devGitInfo, err := o.FindGitInfo(dir)
		if err != nil {
			log.Logger().Warnf("could not find a git repository in the directory %s: %s\n", dir, err.Error())
		}

		DefaultEnvironments(requirements, devGitInfo)

		funcMap, err := o.createFuncMap(requirements)
		if err != nil {
			return err
		}
		chartValues, params, err := helm.GenerateValues(requirements, funcMap, dir, nil, true, secretURLClient)
		if err != nil {
			return errors.Wrapf(err, "generating values.yaml for tree from %s", dir)
		}
		if o.ProviderValuesDir != "" {
			chartValues, err = o.overwriteProviderValues(requirements, requirementsFileName, chartValues, params, o.ProviderValuesDir)
			if err != nil {
				return errors.Wrapf(err, "failed to overwrite provider values in dir: %s", dir)
			}
		}

		err = o.replaceMissingVersionsFromVersionStream(requirements, dir)
		if err != nil {
			return errors.Wrapf(err, "failed to replace missing versions in the requirements.yaml in dir %s", dir)
		}

		chartValuesFile := filepath.Join(dir, helm.ValuesFileName)
		err = ioutil.WriteFile(chartValuesFile, chartValues, 0600)
		if err != nil {
			return errors.Wrapf(err, "writing values.yaml for tree to %s", chartValuesFile)
		}
		log.Logger().Infof("Wrote chart values.yaml %s generated from directory tree", chartValuesFile)

		valuesFiles, err = o.discoverValuesFiles(dir)
		if err != nil {
			return err
		}
	}

	if !o.MultiTemplates {
		if o.Recursive {
			return o.HelmInitRecursiveDependencyBuild(dir, o.DefaultReleaseCharts(), valuesFiles)
		} else {
			return o.HelmInitDependencyBuild(dir, o.DefaultReleaseCharts(), valuesFiles)
		}
	}

	if o.Recursive {
		err = o.HelmInitRecursiveDependencyBuildNoLint(dir, o.DefaultReleaseCharts())
	} else {
		err = o.HelmInitDependencyBuildNoLint(dir, o.DefaultReleaseCharts())
	}
	if err != nil {
		return err
	}

	// Unpack all dependencies and lint them seperately
	err = o.UnpackCharts(dir)
	if err != nil {
		return err
	}

	completeValueFiles := valuesFiles
	mainValueFile := filepath.Join(dir, helm.ValuesFileName)
	if _, err := os.Stat(mainValueFile); err == nil {
		completeValueFiles = append([]string{mainValueFile}, valuesFiles...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Wrapf(err, "stat %s", mainValueFile)
	}
	splitValueFiles, err := helm.SplitValueFiles(completeValueFiles)
	if err != nil {
		return errors.Wrapf(err, "failed to split value files")
	}

	requirements, err := helm.LoadRequirementsFile(filepath.Join(dir, helm.RequirementsFileName))
	if err != nil {
		return err
	}
	for _, dependency := range requirements.Dependencies {
		alias := dependency.Alias
		if alias == "" {
			alias = dependency.Name
		}
		localValuesFiles := helm.GetSplitted(splitValueFiles, alias)
		err = o.HelmLint(filepath.Join(dir, "charts", dependency.Name), localValuesFiles)
		if err != nil {
			return err
		}
	}

	err = helm.RemoveSubTemplates(dir)
	if err != nil {
		return err
	}
	return o.HelmLint(dir, valuesFiles)
}
