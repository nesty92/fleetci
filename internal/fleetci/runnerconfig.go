package fleetci

import (
	"bytes"
	"cmp"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// RunnerOptions fill in the parts of gitlab-runner's [[runners]] entry that
// fleetci.toml doesn't know.
type RunnerOptions struct {
	// ConfigFile is fleetci.toml's absolute path, as the runner will see it.
	ConfigFile string
	// Plugin is fleetci's fleeting plugin. Default fleeting-plugin-fleetci,
	// found on the PATH.
	Plugin string
	Name   string // default "fleetci"
	URL    string // default https://gitlab.com
	// Token is the runner's authentication token, or a placeholder that a
	// setup script replaces.
	Token string
	// Image is the job image when a job names none. Default ubuntu:24.04.
	Image string
	// Global adds concurrent and check_interval, for a config.toml that
	// holds only this runner.
	Global bool
}

// setting is one value gitlab-runner must have for fleetci to work. The
// same list writes the runner's config and checks an existing one.
type setting struct {
	key   string // dotted path under [[runners]]
	want  any
	atMin bool // want is a minimum, not an exact value
	why   string
}

func (c Config) required() []setting {
	nesting := cmp.Or(c.NestingPath, "/fleetci/nesting.sock")
	return []setting{
		{key: "executor", want: "docker-autoscaler", why: "fleetci is a docker-autoscaler plugin"},
		{key: "autoscaler.max_instances", want: int64(1), why: "fleetci is one gateway; the classes hold the VMs"},
		{key: "autoscaler.max_use_count", want: int64(0), why: "the gateway serves every job; each VM is used once by fleetci itself"},
		{key: "autoscaler.capacity_per_instance", want: int64(c.MaxInstances()), atMin: true,
			why: "every GitLab job passes through the gateway: at least the sum of the classes' max_instances"},
		{key: "autoscaler.connector_config.timeout", want: "set", why: "gitlab-runner dials with no retries when it is unset"},
		{key: "autoscaler.vm_isolation.enabled", want: true, why: "fleetci gets one VM per job through vm_isolation"},
		{key: "autoscaler.vm_isolation.nesting_host", want: "unix://" + nesting, why: "fleetci's nesting socket"},
		{key: "autoscaler.vm_isolation.image", want: "class-${FLEETCI_CLASS}", why: "how a job's FLEETCI_CLASS reaches fleetci"},
		{key: "autoscaler.vm_isolation.connector_config.password", want: "set",
			why: "gitlab-runner logs in to job VMs with a password only; it ends inside fleetci, so any value works"},
		{key: "autoscaler.vm_isolation.connector_config.use_static_credentials", want: true, why: "the placeholder password is used as is"},
		{key: "autoscaler.vm_isolation.connector_config.timeout", want: "set", why: "gitlab-runner dials with no retries when it is unset"},
	}
}

// RunnerConfig writes gitlab-runner's [[runners]] entry for this fleetci.
func (c Config) RunnerConfig(o RunnerOptions) (string, error) {
	if o.ConfigFile == "" {
		return "", fmt.Errorf("the path of fleetci.toml is required")
	}
	timeout := 2 * time.Minute
	for _, class := range c.Classes {
		timeout = max(timeout, time.Duration(class.Connector.Timeout))
	}
	data := map[string]any{
		"Global":      o.Global,
		"Concurrent":  c.MaxInstances(),
		"Name":        cmp.Or(o.Name, "fleetci"),
		"URL":         cmp.Or(o.URL, "https://gitlab.com"),
		"Token":       o.Token,
		"Image":       cmp.Or(o.Image, "ubuntu:24.04"),
		"Plugin":      cmp.Or(o.Plugin, "fleeting-plugin-fleetci"),
		"Capacity":    c.MaxInstances(),
		"ConfigFile":  o.ConfigFile,
		"NestingHost": "unix://" + cmp.Or(c.NestingPath, "/fleetci/nesting.sock"),
		"Timeout":     timeout.String(),
	}
	var b bytes.Buffer
	if err := runnerTemplate.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

var runnerTemplate = template.Must(template.New("runner").Funcs(template.FuncMap{"q": tomlString}).Parse(
	`{{if .Global}}concurrent = {{.Concurrent}}
check_interval = 3

{{end}}# Written by "fleetci runner-config". The classes live in fleetci.toml;
# the values below follow from it, so regenerate rather than edit.
[[runners]]
  name = {{q .Name}}
  url = {{q .URL}}
  token = {{q .Token}}
  executor = "docker-autoscaler"
  # Cache archives: fastzip, zstd inside the zip at its fast level, which
  # save and restore faster than the default zip. A job's variables win.
  environment = ["FF_USE_FASTZIP=true", "CACHE_COMPRESSION_FORMAT=zipzstd", "CACHE_COMPRESSION_LEVEL=fast"]
  [runners.docker]
    image = {{q .Image}}
  [runners.autoscaler]
    plugin = {{q .Plugin}}
    # One fleetci gateway carries every job: the sum of the classes' limits.
    capacity_per_instance = {{.Capacity}}
    max_use_count = 0
    max_instances = 1
    [runners.autoscaler.plugin_config]
      config_file = {{q .ConfigFile}}
    [runners.autoscaler.connector_config]
      timeout = {{q .Timeout}}
    [runners.autoscaler.vm_isolation]
      enabled = true
      nesting_host = {{q .NestingHost}}
      image = "class-${FLEETCI_CLASS}"
      [runners.autoscaler.vm_isolation.connector_config]
        # Ends inside fleetci, which logs in to the VM with its own key.
        username = "fleetci"
        password = "unused"
        use_static_credentials = true
        timeout = {{q .Timeout}}
`))

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}
