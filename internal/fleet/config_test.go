package fleet

import (
	"encoding/json"
	"testing"
	"time"
)

// gitlab-runner hands plugin_config over as JSON, so TOML durations arrive
// as strings and nested plugin_config tables as objects.
func TestClassFromRunnerJSON(t *testing.T) {
	raw := `{
	  "plugin": "/opt/fleeting-plugin-aws",
	  "max_instances": 2,
	  "idle_count": 1,
	  "idle_time": "20m",
	  "acquire_timeout": "10m",
	  "plugin_config": {"name": "fleetci-test-small", "region": "eu-west-1"},
	  "connector_config": {"username": "ec2-user", "use_external_addr": true, "timeout": "5m"}
	}`
	var c Class
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.validate("small"); err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.IdleTime) != 20*time.Minute || time.Duration(c.AcquireTimeout) != 10*time.Minute {
		t.Errorf("durations: idle %v, acquire %v", time.Duration(c.IdleTime), time.Duration(c.AcquireTimeout))
	}
	if !c.Connector.UseExternalAddr || time.Duration(c.Connector.Timeout) != 5*time.Minute {
		t.Errorf("connector: %+v", c.Connector)
	}
	var pc map[string]string
	if err := json.Unmarshal(c.PluginConfig, &pc); err != nil || pc["name"] != "fleetci-test-small" {
		t.Errorf("plugin_config passed on as %s", c.PluginConfig)
	}

	s, err := c.Connector.settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Username != "ec2-user" || s.Timeout != 5*time.Minute || s.Protocol != "ssh" {
		t.Errorf("settings: %+v", s.ConnectorConfig)
	}
}

func TestClassValidate(t *testing.T) {
	for name, c := range map[string]Class{
		"no plugin":      {MaxInstances: 1},
		"no max":         {Plugin: "p"},
		"idle above max": {Plugin: "p", MaxInstances: 1, IdleCount: 2},
		"negative idle":  {Plugin: "p", MaxInstances: 1, IdleCount: -1},
	} {
		if err := c.validate("x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
