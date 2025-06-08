package configfilearg

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/rancher/wrangler/v3/pkg/data/convert"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/agent/util"
	"gopkg.in/yaml.v2"
)

type Parser struct {
	After         []string
	ConfigFlags   []string
	OverrideFlags []string
	EnvName       string
	DefaultConfig string
	// ValidFlags are maps of flags that are valid for that particular conmmand. This enables us to ignore flags in
	// the config file that do no apply to the current command.
	ValidFlags map[string][]cli.Flag
}

// Parse will parse an os.Args style slice looking for Parser.FlagNames after Parse.After.
// It will read the parameter value of Parse.FlagNames and read the file, appending all flags directly after
// the Parser.After value. This means a the non-config file flags will override, or if a slice append to, the config
// file values.
// If Parser.DefaultConfig is set, the existence of the config file is optional if not set in the os.Args. This means
// if Parser.DefaultConfig is set we will always try to read the config file but only fail if it's not found if the
// args contains Parser.FlagNames
func (p *Parser) Parse(args []string) ([]string, error) {
	prefix, suffix, found := p.findStart(args)
	if !found {
		return args, nil
	}

	configFile, isSet := p.findConfigFileFlag(args)
	if configFile != "" {
		values, err := readConfigFile(configFile)
		if !isSet && os.IsNotExist(err) {
			return args, nil
		} else if err != nil {
			return nil, err
		}
		if len(args) > 1 {
			values, err = p.stripInvalidFlags(args[1], values)
			if err != nil {
				return nil, err
			}
		}
		return append(prefix, append(values, suffix...)...), nil
	}

	return args, nil
}

func (p *Parser) stripInvalidFlags(command string, args []string) ([]string, error) {
	var result []string
	var cmdFlags []cli.Flag
	for k, v := range p.ValidFlags {
		if k == command {
			cmdFlags = v
		}
	}
	if len(cmdFlags) == 0 {
		return args, nil
	}
	validFlags := make(map[string]bool, len(cmdFlags))
	for _, f := range cmdFlags {
		//split flags with aliases into 2 entries
		for _, s := range strings.Split(f.GetName(), ",") {
			validFlags[s] = true
		}
	}

	re, err := regexp.Compile("^-+([^=]*)=")
	if err != nil {
		return args, err
	}
	for _, arg := range args {
		mArg := arg
		if match := re.FindAllStringSubmatch(arg, -1); match != nil {
			mArg = match[0][1]
		}
		if validFlags[mArg] {
			result = append(result, arg)
		} else {
			logrus.Warnf("Unknown flag %s found in config.yaml, skipping\n", strings.Split(arg, "=")[0])
		}
	}
	return result, nil
}

func (p *Parser) FindString(args []string, target string) (string, error) {

	// Check for --help or --version flags, which override any other flags
	if val, found := p.findOverrideFlag(args); found {
		return val, nil
	}

	configFile, isSet := p.findConfigFileFlag(args)
	var lastVal string
	if configFile != "" {

		_, err := os.Stat(configFile)
		if !isSet && os.IsNotExist(err) {
			return "", nil
		} else if err != nil {
			return "", err
		}

		files, err := dotDFiles(configFile)
		if err != nil {
			return "", err
		}
		files = append([]string{configFile}, files...)
		for _, file := range files {
			bytes, err := readConfigFileData(file)
			if err != nil {
				return "", err
			}

			data := yaml.MapSlice{}
			if err := yaml.Unmarshal(bytes, &data); err != nil {
				return "", err
			}

			lastVal = p.findTargetValue(data, target, lastVal)
		}
	}

	return lastVal, nil
}

func (p *Parser) findTargetValue(data yaml.MapSlice, target string, lastVal string) string {
	for _, i := range data {
		k, v := convert.ToString(i.Key), convert.ToString(i.Value)
		isAppend := strings.HasSuffix(k, "+")
		k = strings.TrimSuffix(k, "+")
		if k == target {
			if isAppend {
				lastVal = lastVal + "," + v
			} else {
				lastVal = v
			}
		}
	}
	return lastVal
}

func (p *Parser) findOverrideFlag(args []string) (string, bool) {
	for _, arg := range args {
		for _, flagName := range p.OverrideFlags {
			if flagName == arg {
				return arg, true
			}
		}
	}

	return "", false
}

func (p *Parser) findConfigFileFlag(args []string) (string, bool) {
	if envVal := os.Getenv(p.EnvName); p.EnvName != "" && envVal != "" {
		return envVal, true
	}

	for i, arg := range args {
		for _, flagName := range p.ConfigFlags {
			if flagName == arg {
				if len(args) > i+1 {
					return args[i+1], true
				}
				// This is actually invalid, so we rely on the CLI parser after the fact flagging it as bad
				return "", false
			} else if strings.HasPrefix(arg, flagName+"=") {
				return arg[len(flagName)+1:], true
			}
		}
	}

	return p.DefaultConfig, false
}

func (p *Parser) findStart(args []string) ([]string, []string, bool) {
	if len(p.After) == 0 {
		return []string{}, args, true
	}
	afterTemp := append([]string{}, p.After...)
	afterIndex := make(map[string]int)
	re, err := regexp.Compile(`(.+):(\d+)`)
	if err != nil {
		return args, nil, false
	}
	// After keywords ending with ":<NUM>" can set + NUM of arguments as the split point.
	// used for matching on subcommmands
	for i, arg := range afterTemp {
		if match := re.FindAllStringSubmatch(arg, -1); match != nil {
			afterTemp[i] = match[0][1]
			afterIndex[match[0][1]], err = strconv.Atoi(match[0][2])
			if err != nil {
				return args, nil, false
			}
		}
	}

	for i, val := range args {
		for _, test := range afterTemp {
			if val == test {
				if skip := afterIndex[test]; skip != 0 {
					if len(args) <= i+skip || strings.HasPrefix(args[i+skip], "-") {
						return args[0 : i+1], args[i+1:], true
					}
					return args[0 : i+skip+1], args[i+skip+1:], true
				}
				return args[0 : i+1], args[i+1:], true
			}
		}
	}
	return args, nil, false
}

func dotDFiles(basefile string) (result []string, _ error) {
	files, err := os.ReadDir(basefile + ".d")
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.IsDir() || !util.HasSuffixI(file.Name(), ".yaml", ".yml") {
			continue
		}
		result = append(result, filepath.Join(basefile+".d", file.Name()))
	}
	return
}

// Main function with reduced complexity
func readConfigFile(file string) (result []string, _ error) {
	files, err := prepareConfigFiles(file)
	if err != nil {
		return nil, err
	}

	values, keyOrder, err := parseConfigFiles(files)
	if err != nil {
		return nil, err
	}

	return formatCommandLineArgs(values, keyOrder), nil
}

// Extract file preparation logic
func prepareConfigFiles(file string) ([]string, error) {
	files, err := dotDFiles(file)
	if err != nil {
		return nil, err
	}

	_, err = os.Stat(file)
	if os.IsNotExist(err) && len(files) > 0 {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	
	return append([]string{file}, files...), nil
}

// Extract config parsing logic
func parseConfigFiles(files []string) (map[string]interface{}, []string, error) {
	var (
		keySeen  = make(map[string]bool)
		keyOrder []string
		values   = make(map[string]interface{})
	)

	for _, file := range files {
		if err := parseConfigFile(file, keySeen, &keyOrder, values); err != nil {
			return nil, nil, err
		}
	}

	return values, keyOrder, nil
}

// Extract single file parsing logic
func parseConfigFile(file string, keySeen map[string]bool, keyOrder *[]string, values map[string]interface{}) error {
	bytes, err := readConfigFileData(file)
	if err != nil {
		return err
	}

	data := yaml.MapSlice{}
	if err := yaml.Unmarshal(bytes, &data); err != nil {
		return err
	}

	for _, i := range data {
		processConfigEntry(i, keySeen, keyOrder, values)
	}

	return nil
}

// Extract entry processing logic
func processConfigEntry(entry yaml.MapItem, keySeen map[string]bool, keyOrder *[]string, values map[string]interface{}) {
	k, v := convert.ToString(entry.Key), entry.Value
	isAppend := strings.HasSuffix(k, "+")
	k = strings.TrimSuffix(k, "+")

	if !keySeen[k] {
		keySeen[k] = true
		*keyOrder = append(*keyOrder, k)
	}

	if oldValue, ok := values[k]; ok && isAppend {
		values[k] = append(toSlice(oldValue), toSlice(v)...)
	} else {
		values[k] = v
	}
}

// Extract formatting logic
func formatCommandLineArgs(values map[string]interface{}, keyOrder []string) []string {
	var result []string

	for _, k := range keyOrder {
		v := values[k]
		prefix := getArgPrefix(k)

		if slice, ok := v.([]interface{}); ok {
			result = append(result, formatSliceArgs(prefix, k, slice)...)
		} else {
			result = append(result, formatSingleArg(prefix, k, v))
		}
	}

	return result
}

// Extract prefix determination logic
func getArgPrefix(key string) string {
	if len(key) == 1 {
		return "-"
	}
	return "--"
}

// Extract slice formatting logic
func formatSliceArgs(prefix, key string, slice []interface{}) []string {
	var args []string
	for _, v := range slice {
		args = append(args, prefix+key+"="+convert.ToString(v))
	}
	return args
}

// Extract single value formatting logic
func formatSingleArg(prefix, key string, value interface{}) string {
	return prefix + key + "=" + convert.ToString(value)
}

func toSlice(v interface{}) []interface{} {
	switch k := v.(type) {
	case string:
		return []interface{}{k}
	case []interface{}:
		return k
	default:
		str := strings.TrimSpace(convert.ToString(v))
		if str == "" {
			return nil
		}
		return []interface{}{str}
	}
}

func readConfigFileData(file string) ([]byte, error) {
	u, err := url.Parse(file)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config location %s: %w", file, err)
	}

	switch u.Scheme {
	case "http", "https":
		resp, err := http.Get(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read http config %s: %w", file, err)
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	default:
		return os.ReadFile(file)
	}
}
