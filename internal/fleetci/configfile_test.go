package fleetci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleFile = `
default_class = "2vcpu-4gb"

[classes.2vcpu-4gb]
plugin = "fleeting-plugin-aws"
max_instances = 4
idle_count = 1
plugin_config = { name = "fleetci-2vcpu-4gb", region = "eu-north-1" }
connector_config = { username = "ubuntu", use_external_addr = true, timeout = "5m" }

[classes.8vcpu-16gb]
plugin = "bin/fleeting-plugin-aws"
max_instances = 2

[classes.8vcpu-16gb.plugin_config]
name = "fleetci-8vcpu-16gb"

[[classes.8vcpu-16gb.policy]]
idle_count = 0

[[classes.8vcpu-16gb.policy]]
periods = ["* 8-18 * * mon-fri"]
timezone = "Europe/Warsaw"
idle_count = 1
idle_time = "20m"

[github]
repos = ["owner/name"]
orgs = ["acme"]
token_file = "github.token"

[github.webhook]
listen = ":9480"
secret_file = "webhook.secret"
tls_cert_file = "/etc/tls/cert.pem"
tls_key_file = "tls/key.pem"
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleetci.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigFile(t *testing.T) {
	path := writeFile(t, sampleFile)
	dir := filepath.Dir(path)

	c := Config{ConfigFile: path, NestingPath: "/custom/nesting.sock"}
	if err := c.loadFile(); err != nil {
		t.Fatal(err)
	}
	if c.DefaultClass != "2vcpu-4gb" || len(c.Classes) != 2 || c.NestingPath != "/custom/nesting.sock" {
		t.Fatalf("loaded %+v", c)
	}

	small := c.Classes["2vcpu-4gb"]
	if small.Plugin != "fleeting-plugin-aws" || small.IdleCount != 1 || !small.Connector.UseExternalAddr ||
		time.Duration(small.Connector.Timeout) != 5*time.Minute || small.Connector.Username != "ubuntu" {
		t.Errorf("2vcpu-4gb: %+v", small)
	}
	var pc map[string]string
	if err := json.Unmarshal([]byte(small.PluginConfig), &pc); err != nil || pc["region"] != "eu-north-1" {
		t.Errorf("plugin_config %s", small.PluginConfig)
	}

	big := c.Classes["8vcpu-16gb"]
	if big.Plugin != filepath.Join(dir, "bin/fleeting-plugin-aws") {
		t.Errorf("relative plugin path: %s", big.Plugin)
	}
	if len(big.Policy) != 2 || big.Policy[1].Timezone != "Europe/Warsaw" || time.Duration(big.Policy[1].IdleTime) != 20*time.Minute {
		t.Errorf("policies: %+v", big.Policy)
	}

	if c.GitHub == nil || c.GitHub.Repos[0] != "owner/name" || c.GitHub.TokenFile != filepath.Join(dir, "github.token") {
		t.Errorf("github: %+v", c.GitHub)
	}
	if w := c.GitHub.Webhook; w == nil || w.SecretFile != filepath.Join(dir, "webhook.secret") ||
		w.TLSCertFile != "/etc/tls/cert.pem" || w.TLSKeyFile != filepath.Join(dir, "tls/key.pem") || c.GitHub.Orgs[0] != "acme" {
		t.Errorf("github webhook: %+v", c.GitHub.Webhook)
	}
}

func TestConfigFileErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		cfg  func(path string) Config
		want string
	}{
		"misspelled key": {
			file: "default_class = \"a\"\n[classes.a]\nplugin = \"p\"\nmax_instances = 1\nidle_cont = 1\n",
			want: "unknown keys: classes.a.idle_cont",
		},
		"bad toml":                  {file: "default_class = \"a\"\nclasses = [", want: "line 2"},
		"bad duration":              {file: "[classes.a]\nplugin = \"p\"\nmax_instances = 1\nidle_time = \"ten minutes\"\n", want: "idle_time"},
		"plugin_config not a table": {file: "[classes.a]\nplugin = \"p\"\nmax_instances = 1\nplugin_config = \"x\"\n", want: "must be a table"},
		"classes in both places": {
			file: "default_class = \"a\"\n",
			cfg:  func(p string) Config { return Config{ConfigFile: p, DefaultClass: "a"} },
			want: "not in plugin_config",
		},
	} {
		path := writeFile(t, tc.file)
		c := Config{ConfigFile: path}
		if tc.cfg != nil {
			c = tc.cfg(path)
		}
		err := c.loadFile()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v; want %q", name, err, tc.want)
		}
	}
}
