package gitlab

import (
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	"gitlab.com/gitlab-org/gitlab-runner/common"
)

func TestBucketCachesGoFileByFileUnlessTheRunnerSaysArchive(t *testing.T) {
	s3 := &cacheconfig.Config{Type: "s3"}
	for name, c := range map[string]struct {
		rc   *common.RunnerConfig
		want bool
	}{
		"no cache":       {&common.RunnerConfig{}, false},
		"a bucket":       {&common.RunnerConfig{RunnerSettings: common.RunnerSettings{Cache: s3}}, true},
		"not a bucket":   {&common.RunnerConfig{RunnerSettings: common.RunnerSettings{Cache: &cacheconfig.Config{Type: "local"}}}, false},
		"archive":        {&common.RunnerConfig{RunnerSettings: common.RunnerSettings{Cache: s3, Environment: withCacheStore(nil, StoreArchive)}}, false},
		"files, said so": {&common.RunnerConfig{RunnerSettings: common.RunnerSettings{Cache: s3, Environment: withCacheStore(nil, StoreFiles)}}, true},
	} {
		if got := usesFileCache(c.rc); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	// A runner's own setting wins over [gitlab] cache_store.
	env := withCacheStore([]string{cacheStoreEnv + "=archive"}, StoreFiles)
	if len(env) != 1 {
		t.Fatalf("%v", env)
	}
	if got := withTarZstd(withCacheCompression(nil)); len(got) != len(cacheCompression) || got[len(got)-1] != "CACHE_COMPRESSION_FORMAT=tarzstd" {
		t.Fatalf("tar.zst in place of the runner's: %v", got)
	}
	if err := (Config{ConfigFile: "x", CacheStore: "zip"}).Validate(nil); err == nil {
		t.Fatal("cache_store zip: no error")
	}
}
