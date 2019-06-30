package helm

import (
	"crypto/sha1"
	"fmt"
	"github.com/ghodss/yaml"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zedge/kubecd/pkg/exec"
	"github.com/zedge/kubecd/pkg/model"
)

var runner exec.Runner = exec.RealRunner{}

func inspectCacheDir() string {
	dir := os.Getenv("KUBECD_CACHE")
	if dir == "" {
		me, _ := user.Current()
		dir = me.HomeDir
	}
	return filepath.Join(dir, ".kubecd", "cache", "inspect")
}

func pathExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return true
		}
	}
	return false
}

// InspectChart :
func InspectChart(chartReference, chartVersion string) ([]byte, error) {
	h := sha1.New()
	h.Write([]byte(chartReference))
	h.Write([]byte(chartVersion))
	chartHash := fmt.Sprintf("%x", h.Sum(nil))
	cacheDir := inspectCacheDir()
	cacheFile := filepath.Join(cacheDir, chartHash)
	if pathExists(cacheFile) {
		data, err := ioutil.ReadFile(cacheFile)
		if err != nil {
			return nil, fmt.Errorf(`could not read %q: %v`, cacheFile, err)
		}
		return data, nil
	}
	out, err := runner.Run("helm", "inspect", chartReference, "--version", chartVersion)
	if err != nil {
		return nil, fmt.Errorf(`error while running "helm inspect": %v`, err)
	}
	err = ioutil.WriteFile(cacheFile, out, 0644)
	if err != nil {
		return nil, fmt.Errorf(`error while writing cache file: %v`, err)
	}
	return out, nil
}

// RepoSetupCommands :
func RepoSetupCommands(repos []model.HelmRepo) [][]string {
	var cmds [][]string
	for _, repo := range repos {
		cmds = append(cmds, []string{"helm", "repo", "add", repo.Name, repo.URL})
	}
	cmds = append(cmds, []string{"helm", "repo", "update"})
	return cmds
}

func UseContextCommand(envName string) []string {
	return []string{"kubectl", "config", "use-context", model.KubeContextName(envName)}
}

func KubectlApplyCommand(resourceFiles []string, dryRun bool, envName string) []string {
	cmd := []string{"kubectl", "--context", model.KubeContextName(envName), "apply"}
	if dryRun {
		cmd = append(cmd, "--dry-run")
	}
	for _, file := range resourceFiles {
		cmd = append(cmd, "-f", file)
	}
	return cmd
}

const (
	DryRun   = true
	NoDryRun = false
	Debug    = true
	NoDebug  = false
)

func stringInSlice(str string, slice []string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func DeployCommands(env *model.Environment, dryRun, debug bool, limitToReleases []string) ([][]string, error) {
	var commands [][]string
	for _, releaseName := range limitToReleases {
		if env.GetRelease(releaseName) == nil {
			return nil, fmt.Errorf(`env %q: release not found: %q`, env.Name, releaseName)
		}
	}
	for _, release := range env.AllReleases() {
		if limitToReleases == nil || stringInSlice(release.Name, limitToReleases) {
			relFile := release.FromFile()
			if release.Chart != nil {
				tmp, err := GenerateHelmApplyArgv(release, env, dryRun, debug)
				if err != nil {
					return nil, err
				}
				commands = append(commands, tmp)
			} else if release.ResourceFiles != nil {
				absFiles := make([]string, len(release.ResourceFiles))
				for i, path := range release.ResourceFiles {
					absFiles[i] = model.ResolvePathFromFile(path, relFile)
				}
				commands = append(commands, KubectlApplyCommand(absFiles, dryRun, env.Name))
			}
		}
	}
	return commands, nil
}

func GenerateHelmBaseArgv(env *model.Environment) []string {
	return []string{"helm", "--kube-context", model.KubeContextName(env.Name)}
}

func formatSetValuesString(values []model.ChartValue, env *model.Environment, skipValuesFrom bool) (string, error) {
	tmp := make([]string, len(values))
	for i, val := range values {
		rv, err := ResolveValue(val, env, false)
		if err != nil {
			return "", err
		}
		tmp[i] = rv.Key + "=" + rv.Value
	}
	return strings.Join(tmp, ","), nil
}

func GenerateHelmValuesArgv(rel *model.Release, env *model.Environment) ([]string, error) {
	var argv []string
	if !rel.SkipDefaultValues {
		if env.DefaultValuesFile != "" {
			argv = append(argv, "--values", rel.AbsPath(env.DefaultValuesFile))
		}
		if env.DefaultValues != nil {
			setArg, err := formatSetValuesString(env.DefaultValues, env, false)
			if err != nil {
				return []string{}, err
			}
			argv = append(argv, "--set-string", setArg)
		}
	}
	if rel.ValuesFile != nil {
		argv = append(argv, rel.AbsPath(*rel.ValuesFile))
	}
	if rel.Values != nil {
		setArg, err := formatSetValuesString(rel.Values, env, false)
		if err != nil {
			return []string{}, err
		}
		argv = append(argv, "--set-string", setArg)
	}
	return argv, nil
}

func GenerateHelmChartArgs(rel *model.Release) ([]string, error) {
	if rel.Chart.Reference == nil {
		chartDir := rel.AbsPath(*rel.Chart.Dir)
		if !pathExists(chartDir) {
			return []string{}, fmt.Errorf(`%s: release %q chart.dir %q does not exist`, rel.FromFile(), rel.Name, chartDir)
		}
		return []string{chartDir}, nil
	}
	return []string{*rel.Chart.Reference, "--version", *rel.Chart.Version}, nil
}

func GenerateHelmDiffArgv(rel *model.Release, env *model.Environment) ([]string, error) {
	argv := GenerateHelmBaseArgv(env)
	argv = append(argv, "diff", "upgrade", rel.Name)
	chartArgs, err := GenerateHelmChartArgs(rel)
	if err != nil {
		return []string{}, err
	}
	argv = append(argv, chartArgs...)
	valueArgs, err := GenerateHelmValuesArgv(rel, env)
	if err != nil {
		return []string{}, err
	}
	argv = append(argv, valueArgs...)
	return argv, nil
}

func GenerateHelmTemplateArgv(rel *model.Release, env *model.Environment) ([]string, error) {
	argv := GenerateHelmBaseArgv(env)
	chartDir := rel.AbsPath(*rel.Chart.Dir)
	argv = append(argv, "template", chartDir, "-n", rel.Name, "--namespace", env.KubeNamespace)
	valueArgs, err := GenerateHelmValuesArgv(rel, env)
	if err != nil {
		return []string{}, err
	}
	argv = append(argv, valueArgs...)
	return argv, nil
}

func GenerateHelmApplyArgv(rel *model.Release, env *model.Environment, dryRun, debug bool) ([]string, error) {
	chartArgs, err := GenerateHelmChartArgs(rel)
	if err != nil {
		return []string{}, err
	}
	valueArgs, err := GenerateHelmValuesArgv(rel, env)
	if err != nil {
		return []string{}, err
	}
	argv := GenerateHelmBaseArgv(env)
	argv = append(argv, "upgrade", rel.Name)
	argv = append(argv, chartArgs...)
	argv = append(argv, "-i", "--namespace", env.KubeNamespace)
	argv = append(argv, valueArgs...)
	if dryRun {
		argv = append(argv, "--dry-run")
	}
	if debug {
		argv = append(argv, "--debug")
	}
	return argv, nil
}

func ResolveValue(value model.ChartValue, env *model.Environment, skipValueFrom bool) (*model.ChartValue, error) {
	retVal := &model.ChartValue{Key: value.Key, Value: value.Value}
	if skipValueFrom || value.ValueFrom == nil {
		return retVal, nil
	}
	if gceRes := value.ValueFrom.GceResource; gceRes != nil {
		if gceRes.Address != nil {
			addr, err := ResolveGceAddressValue(value.ValueFrom.GceResource.Address, env)
			if err != nil {
				return nil, err
			}
			retVal.Value = addr
		}
	}
	return retVal, nil
}

var zoneToRegionRegexp = regexp.MustCompile(`-[a-z]$`)

func ResolveGceAddressValue(address *model.GceAddressValueRef, env *model.Environment) (string, error) {
	gke := env.GetCluster().Provider.GKE
	argv := []string{"compute", "addresses", "describe", address.Name, "--format", "value(address)", "--project", gke.Project}
	if address.IsGlobal {
		argv = append(argv, "--global")
	} else {
		argv = append(argv, "--region")
		if gke.Zone != nil {
			argv = append(argv, zoneToRegionRegexp.ReplaceAllString(*gke.Zone, ""))
		} else {
			argv = append(argv, *gke.Region)
		}
	}
	out, err := runner.Run("gcloud", argv...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func MergeValues(from map[string]interface{}, onto map[string]interface{}) map[string]interface{} {
	result := onto
	for key, value := range from {
		_, newValueIsMap := value.(map[string]interface{})
		_, keyExists := result[key]
		oldValueIsMap := false
		if keyExists {
			_, oldValueIsMap = result[key].(map[string]interface{})
		}
		if newValueIsMap {
			if oldValueIsMap {
				result[key] = MergeValues(from[key].(map[string]interface{}), result[key].(map[string]interface{}))
			} else {
				result[key] = value
			}
		} else {
			result[key] = value
		}
	}
	return result
}

func LoadValuesFile(fileName string) (map[string]interface{}, error) {
	var values map[string]interface{}
	r, err := os.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("error while opening %s: %v", fileName, err)
	}
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("error while reading %s: %v", fileName, err)
	}
	err = yaml.Unmarshal(data, &values)
	if err != nil {
		return nil, fmt.Errorf("error while unmarshaling %s: %v", fileName, err)
	}
	return values, nil
}

func valToMap(key []string, value interface{}) map[string]interface{} {
	if len(key) == 1 {
		return map[string]interface{}{key[0]: value}
	}
	return map[string]interface{}{key[0]: valToMap(key[1:], value)}
}

func ValuesListToMap(values []model.ChartValue, env *model.Environment, skipValueFrom bool) (map[string]interface{}, error) {
	var result = make(map[string]interface{})
	for _, value := range values {
		value, err := ResolveValue(value, env, false)
		if err != nil {
			return nil, err
		}
		result = MergeValues(valToMap(strings.Split(value.Key, "."), value.Value), result)
	}
	return result, nil
}

func LookupValueByString(key string, values map[string]interface{}) interface{} {
	return LookupValueByPath(strings.Split(key, "."), values)
}

func LookupValueByPath(key []string, values map[string]interface{}) *string {
	if len(key) > 0 && values != nil {
		if val := values[key[0]]; val != nil {
			if len(key) == 1 {
				tmp, isStr := val.(string)
				if !isStr {
					return nil
				}
				return &tmp
			}
			nextVal, isMap := val.(map[string]interface{})
			if !isMap {
				return nil
			}
			return LookupValueByPath(key[1:], nextVal)
		}
	}
	return nil
}

func KeyIsInValues(key string, values map[string]interface{}) bool {
	return LookupValueByString(key, values) != nil
}

func GetResolvedValues(release *model.Release, forEnv *model.Environment, skipValueFrom bool) (map[string]interface{}, error) {
	values := make(map[string]interface{})
	if release.Chart != nil && release.Chart.Dir != nil {
		valuesFile := model.ResolvePathFromDir("values.yaml", *release.Chart.Dir)
		if pathExists(valuesFile) {
			chartValues, err := LoadValuesFile(valuesFile)
			if err != nil {
				return nil, fmt.Errorf(`failed to load values file %q for chart dir %q: %v`, valuesFile, *release.Chart.Dir, err)
			}
			values = MergeValues(chartValues, values)
		}
	} else if release.Chart != nil && release.Chart.Reference != nil {
		output, err := InspectChart(*release.Chart.Reference, *release.Chart.Version)
		if err != nil {
			return nil, fmt.Errorf(`failed to spect Helm chart %q version %q: %v`, *release.Chart.Reference, *release.Chart.Version, err)
		}
		var chartDefaultValues map[string]interface{}
		if err = yaml.Unmarshal(output, &chartDefaultValues); err != nil {
			return nil, fmt.Errorf(`failed to unmarshal inspected values for chart %q version %q: %v`, *release.Chart.Reference, *release.Chart.Version, err)
		}
	}
	if forEnv != nil && forEnv.DefaultValues != nil {
		envDefaultValues, err := ValuesListToMap(forEnv.DefaultValues, forEnv, skipValueFrom)
		if err != nil {
			return nil, fmt.Errorf(`failed to resolve defaultValues for env %q and release %q: %v`, forEnv.Name, release.Name, err)
		}
		values = MergeValues(envDefaultValues, values)
	}
	if release.ValuesFile != nil {
		absPath := release.AbsPath(*release.ValuesFile)
		releaseFileValues, err := LoadValuesFile(absPath)
		if err != nil {
			return nil, fmt.Errorf(`failed to load release values file %q for release %q: %v`, absPath, release.Name, err)
		}
		values = MergeValues(releaseFileValues, values)
	}
	if forEnv != nil && release.Values != nil {
		releaseValues, err := ValuesListToMap(release.Values, forEnv, skipValueFrom)
		if err != nil {
			return nil, fmt.Errorf(`failed to resolve inline values for release %q: %v`, release.Name, err)
		}
		values = MergeValues(releaseValues, values)
	}
	return values, nil
}
