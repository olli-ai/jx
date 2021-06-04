package helm

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/jenkins-x/jx/v2/pkg/config"
	"github.com/jenkins-x/jx/v2/pkg/secreturl"
	"github.com/jenkins-x/jx/v2/pkg/util"
	"github.com/pkg/errors"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/engine"

	"github.com/ghodss/yaml"

	"github.com/jenkins-x/jx-logging/pkg/log"
)

//DefaultValuesTreeIgnores is the default set of ignored files for collapsing the values tree which are used if
// ignores is nil
var DefaultValuesTreeIgnores = []string{
	"templates/*",
}

// GenerateValues will generate a values.yaml file in dir. It scans all subdirectories for values.yaml files,
// and merges them into the values.yaml in the root directory,
// creating a nested key structure that matches the directory structure.
// Any keys used that match files with the same name in the directory (
// and have empty values) will be inlined as block scalars.
// Standard UNIX glob patterns can be passed to IgnoreFile directories.
func GenerateValues(requirements *config.RequirementsConfig, funcMap template.FuncMap, dir string, ignores []string, verbose bool, secretURLClient secreturl.Client) ([]byte, chartutil.Values, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	} else if os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("%s does not exist", dir)
	} else if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", dir)
	}

	// load the parameter values if there are any
	params, err := LoadParameters(dir, secretURLClient)
	if err != nil {
		return nil, params, err
	}
	if funcMap == nil {
		funcMap = NewFunctionMap()
	}
	if ignores == nil {
		ignores = DefaultValuesTreeIgnores
	}
	files := make(map[string]map[string]string)
	values := make(map[string]interface{})
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		rPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Check if should IgnoreFile the path
		if ignore, err := util.IgnoreFile(rPath, ignores); err != nil {
			return err
		} else if !ignore {
			rDir, file := filepath.Split(rPath)
			// For the root dir we just consider directories (which the walk func does for us)
			if rDir != "" {
				// If it's values.tmpl.yaml, then evalate it as a go template and parse it
				if file == ValuesTemplateFileName {
					b, err := ReadValuesYamlFileTemplateOutput(path, params, funcMap, requirements)
					if err != nil {
						return err
					}
					v := make(map[string]interface{})

					err = yaml.Unmarshal(b, &v)
					if err != nil {
						return err
					}
					if values[rDir] != nil {
						return fmt.Errorf("already has a nested values map at %s when processing file %s", rDir, rPath)
					}
					values[rDir] = v
				} else if file == ValuesFileName {
					b, err := ioutil.ReadFile(path)
					if err != nil {
						return err
					}
					v := make(map[string]interface{})

					err = yaml.Unmarshal(b, &v)
					if err != nil {
						return err
					}
					if values[rDir] != nil {
						return fmt.Errorf("already has a nested values map at %s when processing file %s", rDir, rPath)
					}
					values[rDir] = v
				} else {
					// for other files, just store a reference
					if _, ok := files[rDir]; !ok {
						files[rDir] = make(map[string]string)
					}
					files[rDir][file] = path
				}
			}
		} else {
			if verbose {
				log.Logger().Infof("Ignoring %s", rPath)
			}
		}
		return nil
	})
	if err != nil {
		return nil, params, err
	}
	// Load the root values.yaml
	rootData := []byte{}

	rootValuesFileName := filepath.Join(dir, ValuesTemplateFileName)
	exists, err := util.FileExists(rootValuesFileName)
	if err != nil {
		return nil, params, errors.Wrapf(err, "failed to find %s", rootValuesFileName)
	}
	if exists {
		rootData, err = ReadValuesYamlFileTemplateOutput(rootValuesFileName, params, funcMap, requirements)
		if err != nil {
			return nil, params, errors.Wrapf(err, "failed to render template of file %s", rootValuesFileName)
		}
	} else {
		rootValuesFileName = filepath.Join(dir, ValuesFileName)
		exists, err = util.FileExists(rootValuesFileName)
		if err != nil {
			return nil, params, errors.Wrapf(err, "failed to find %s", rootValuesFileName)
		}
		if exists {
			rootData, err = ioutil.ReadFile(rootValuesFileName)
			if err != nil {
				return nil, params, errors.Wrapf(err, "failed to load file %s", rootValuesFileName)
			}
		}
	}
	rootValues, err := LoadValues(rootData)
	if err != nil {
		return nil, params, err
	}

	// externalFileHandler is used to read any inline any files that match into the values.yaml
	externalFileHandler := func(path string, element map[string]interface{}, key string) error {
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		element[key] = string(b)
		return nil
	}
	for p, v := range values {
		// First, do file substitution - but only if any files were actually found
		if dirFiles := files[p]; dirFiles != nil && len(dirFiles) > 0 {
			err := HandleExternalFileRefs(v, dirFiles, "", externalFileHandler)
			if err != nil {
				return nil, params, err
			}
		}

		// Then, merge the values to the root file
		keys := strings.Split(strings.TrimSuffix(p, "/"), string(os.PathSeparator))
		x := rootValues
		jsonPath := "$"
		for i, k := range keys {
			k = strings.TrimSuffix(k, "/")
			jsonPath = fmt.Sprintf("%s.%s", jsonPath, k)
			v1, ok1 := x[k]
			if i < len(keys)-1 {
				// Create the nested file object structure
				if !ok1 {
					// Easy, just create the nested object!
					new := make(map[string]interface{})
					x[k] = new
					x = new
				} else {
					// Need to do a type check
					v2, ok2 := v1.(map[string]interface{})

					if !ok2 {
						return nil, params, fmt.Errorf("%s is not an associative array", jsonPath)
					}
					x = v2
				}
			} else {
				// Apply
				x[k] = v
			}
		}
	}
	data, err := yaml.Marshal(rootValues)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	var text string
	if secretURLClient != nil {
		text, err = secretURLClient.ReplaceURIs(string(data))
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}
	} else {
		text = string(data)
	}

	return []byte(text), params, err
}

// NewFunctionMap creates a new function map for values.tmpl.yaml templating
func NewFunctionMap() template.FuncMap {
	funcMap := engine.FuncMap()
	funcMap["hashPassword"] = util.HashPassword
	funcMap["removeScheme"] = util.RemoveScheme
	return funcMap
}

// ReadValuesYamlFileTemplateOutput evaluates the given values.yaml file as a go template and returns the output data
func ReadValuesYamlFileTemplateOutput(templateFile string, params chartutil.Values, funcMap template.FuncMap, requirements *config.RequirementsConfig) ([]byte, error) {
	tmpl, err := template.New(ValuesTemplateFileName).Option("missingkey=error").Funcs(funcMap).ParseFiles(templateFile)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse Secrets template: %s", templateFile)
	}

	requirementsMap, err := requirements.ToMap()
	if err != nil {
		return nil, errors.Wrapf(err, "failed turn requirements into a map: %v", requirements)
	}

	templateData := map[string]interface{}{
		"Parameters":   params,
		"Requirements": chartutil.Values(requirementsMap),
		"Environments": chartutil.Values(requirements.EnvironmentMap()),
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, templateData)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to execute Secrets template: %s", templateFile)
	}
	data := buf.Bytes()
	return data, nil
}

// HandleExternalFileRefs recursively scans the element map structure,
// looking for nested maps. If it finds keys that match any key-value pair in possibles it will call the handler.
// The jsonPath is used for referencing the path in the map structure when reporting errors.
func HandleExternalFileRefs(element interface{}, possibles map[string]string, jsonPath string,
	handler func(path string, element map[string]interface{}, key string) error) error {
	if jsonPath == "" {
		// set zero value
		jsonPath = "$"
	}
	if e, ok := element.(map[string]interface{}); ok {
		for k, v := range e {
			if paths, ok := possibles[k]; ok {
				if v == nil || util.IsZeroOfUnderlyingType(v) {
					// There is a filename in the directory structure that matches this key, and it has no value,
					// so we handle it
					err := handler(paths, e, k)
					if err != nil {
						return err
					}
				} else {
					return fmt.Errorf("value at %s must be empty but is %v", jsonPath, v)
				}
			} else {
				// keep on recursing
				jsonPath = fmt.Sprintf("%s.%s", jsonPath, k)
				err := HandleExternalFileRefs(v, possibles, jsonPath, handler)
				if err != nil {
					return err
				}
			}
		}
	}
	// If it's not an object, we can't do much with it
	return nil
}

func SplitValueArgs(valueArgs []string) (map[string][]string, error) {
	globalArgs := []string{}
	localArgs := map[string][]string{}
	for _, arg := range valueArgs {
		splits := strings.SplitN(arg, "=", 2)
		if len(splits) <= 1 {
			return nil, fmt.Errorf("unparsable --set arg \"%s\"", arg)
		}
		field := splits[0]
		splits = strings.SplitN(field, ".", 2)
		prefix := splits[0]
		if prefix == "global" || prefix == "tags" {
			globalArgs = append(globalArgs, arg)
		} else if len(splits) > 1 {
			arg = arg[len(prefix)+1:len(arg)]
			localArgs[prefix] = append(localArgs[prefix], arg)
		}
	}
	if len(globalArgs) == 0 {
		return localArgs, nil
	}
	splitArgs := map[string][]string{
		"global": globalArgs,
	}
	for key, args := range localArgs {
		var copy []string
		copy = append(copy, globalArgs...)
		splitArgs[key] = append(copy, args...)
	}
	return splitArgs, nil
}

func SplitValueFiles(valueFiles []string) (map[string][]string, error) {
	globalFiles := []string{}
	localFiles := map[string][]string{}
	dest, err := ioutil.TempDir("", "split-value-files-")
	if err != nil {
		return nil, err
	}
	err = os.Mkdir(filepath.Join(dest, "global"), 0700)
	if err != nil {
		return nil, err
	}
	for _, file := range valueFiles {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}
		value := map[string]interface{}{}
		err = yaml.Unmarshal(data, &value)
		if err != nil {
			return nil, err
		}
		globalValues := map[string]interface{}{}
		for key, subvalue := range value {
			if subvalue == nil {
				continue
			}
			if key == "global" || key == "tags" {
				globalValues[key] = subvalue
				continue
			}
			if localFiles[key] == nil {
				err = os.Mkdir(filepath.Join(dest, key), 0700)
				if err != nil {
					return nil, err
				}
			}
			data, err = yaml.Marshal(subvalue)
			if err != nil {
				return nil, err
			}
			target := fmt.Sprintf("values%d.yaml", len(localFiles[key]))
			target = filepath.Join(dest, key, target)
			err = ioutil.WriteFile(target, data, 0600)
			if err != nil {
				return nil, err
			}
			localFiles[key] = append(localFiles[key], target)
		}
		if len(globalValues) > 0 {
			data, err = yaml.Marshal(globalValues)
			if err != nil {
				return nil, err
			}
			target := fmt.Sprintf("values%d.yaml", len(globalFiles))
			target = filepath.Join(dest, "global", target)
			err = ioutil.WriteFile(target, data, 0600)
			if err != nil {
				return nil, err
			}
			globalFiles = append(globalFiles, target)
		}
	}
	if len(globalFiles) == 0 {
		return localFiles, nil
	}
	splitFiles := map[string][]string{
		"global": globalFiles,
	}
	for key, files := range localFiles {
		var copy []string
		copy = append(copy, globalFiles...)
		splitFiles[key] = append(copy, files...)
	}
	return splitFiles, nil
}
