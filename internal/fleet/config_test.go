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
	  "plugin_config": {"name": "fleetci-test-2vcpu-1gb", "region": "eu-west-1"},
	  "connector_config": {"username": "ec2-user", "use_external_addr": true, "timeout": "5m"}
	}`
	var c Class
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.validate("2vcpu-1gb"); err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.IdleTime) != 20*time.Minute || time.Duration(c.AcquireTimeout) != 10*time.Minute {
		t.Errorf("durations: idle %v, acquire %v", time.Duration(c.IdleTime), time.Duration(c.AcquireTimeout))
	}
	if !c.Connector.UseExternalAddr || time.Duration(c.Connector.Timeout) != 5*time.Minute {
		t.Errorf("connector: %+v", c.Connector)
	}
	var pc map[string]string
	if err := json.Unmarshal(c.PluginConfig, &pc); err != nil || pc["name"] != "fleetci-test-2vcpu-1gb" {
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

func TestClassPolicies(t *testing.T) {
	raw := `{
	  "plugin": "p", "max_instances": 4,
	  "policy": [
	    {"idle_count": 0},
	    {"periods": ["* 8-18 * * mon-fri"], "timezone": "Europe/Warsaw", "idle_count": 2, "idle_time": "20m",
	     "scale_factor": 0.5, "scale_factor_limit": 4}
	  ]
	}`
	var c Class
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.validate("x"); err != nil {
		t.Fatal(err)
	}
	s := c.schedules()
	if len(s) != 2 {
		t.Fatalf("%d schedules", len(s))
	}
	if s[0].IdleCount != 0 || s[0].IdleTime != defaultIdleTime || len(s[0].Periods) != 0 {
		t.Errorf("default policy: %+v", s[0])
	}
	w := s[1]
	if w.Periods[0] != "* 8-18 * * mon-fri" || w.Timezone != "Europe/Warsaw" || w.IdleCount != 2 ||
		w.IdleTime != 20*time.Minute || w.ScaleFactor != 0.5 || w.ScaleFactorLimit != 4 {
		t.Errorf("work hours policy: %+v", w)
	}

	// idle_count alone is one policy at all times.
	short := Class{Plugin: "p", MaxInstances: 2, IdleCount: 1}
	if s := short.schedules(); len(s) != 1 || s[0].IdleCount != 1 || s[0].IdleTime != defaultIdleTime {
		t.Errorf("shorthand: %+v", s)
	}
}

func TestClassPolicyValidate(t *testing.T) {
	for name, c := range map[string]Class{
		"idle beside policy": {Plugin: "p", MaxInstances: 2, IdleCount: 1, Policy: []Policy{{}}},
		"policy above max":   {Plugin: "p", MaxInstances: 2, Policy: []Policy{{IdleCount: 3}}},
		"bad period":         {Plugin: "p", MaxInstances: 2, Policy: []Policy{{Periods: []string{"every day"}}}},
		"bad timezone":       {Plugin: "p", MaxInstances: 2, Policy: []Policy{{Periods: []string{"* * * * *"}, Timezone: "Mars/Olympus"}}},
		"negative factor":    {Plugin: "p", MaxInstances: 2, Policy: []Policy{{ScaleFactor: -1}}},
		"preemptive mode":    {Plugin: "p", MaxInstances: 2, Policy: []Policy{{PreemptiveMode: new(bool)}}},
	} {
		if err := c.validate("x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
