package fleet

import (
	"strings"
	"testing"
)

func TestCacheDiskValidate(t *testing.T) {
	ok := Class{Plugin: "/usr/bin/fleeting-plugin-fleetingd", MaxInstances: 2,
		PluginConfig: RawConfig(`{"vm_cache_directory": "/var/lib/fleetci/cache"}`), CacheDisk: &CacheDisk{}}
	if err := ok.validate("small"); err != nil {
		t.Fatalf("valid class: %v", err)
	}
	for want, c := range map[string]Class{
		"fleetingd": func() Class { c := ok; c.Plugin = "fleeting-plugin-aws"; return c }(),
		"vm_cache_directory": func() Class {
			c := ok
			c.PluginConfig = RawConfig(`{"vm_cache_directory": "cache"}`)
			return c
		}(),
		"a VM per job": func() Class { c := ok; c.MaxUseCount = 3; return c }(),
		"negative":     func() Class { c := ok; c.CacheDisk = &CacheDisk{Budget: -1}; return c }(),
	} {
		err := c.validate("small")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error about %s, got %v", want, err)
		}
	}
	none := ok
	none.CacheDisk, none.Plugin = nil, "fleeting-plugin-aws"
	if err := none.validate("small"); err != nil {
		t.Fatalf("class without cache_disk: %v", err)
	}
}

func TestCacheDiskConfig(t *testing.T) {
	c := CacheDisk{Budget: 2048, MinFree: 1024}.config()
	if c.Budget != 2<<30 || c.MinFree != 1<<30 || c.MaxPerCache != 0 {
		t.Fatalf("config %+v", c)
	}
}
