# Changelog

## [0.3.0](https://github.com/nesty92/fleetci/compare/v0.2.0...v0.3.0) (2026-11-05)


### Features

* fleetci serve runs GitHub jobs without gitlab-runner ([f1599f9](https://github.com/nesty92/fleetci/commit/f1599f9aab572102f16d409ea932aef44bef98cb))
* fleetci watchdog restarts gitlab-runner when its plugin is gone ([bb3aea3](https://github.com/nesty92/fleetci/commit/bb3aea31e12cd568f51fd1958573bad66fa5c50c))
* **github:** cache_url sends Actions caches to a self-hosted cache server ([8b3248d](https://github.com/nesty92/fleetci/commit/8b3248dd620ce8cf043d968daf910caeebd514ec))
* **hack:** cold boot timings, Cloud Hypervisor against Firecracker ([d3a9b3d](https://github.com/nesty92/fleetci/commit/d3a9b3d02fd6be497a94a4d8f16252a1ace71dbe))
* **hack:** Firecracker run of the m1 snapshot experiment ([c456413](https://github.com/nesty92/fleetci/commit/c456413adca88844cbdff946465a3da453e2de15))
* **hack:** Firecracker with Ubuntu's kernel in the snapshot experiment ([8ff520e](https://github.com/nesty92/fleetci/commit/8ff520e959f074be8d6239d44d335be21527efde))
* **hack:** local Actions cache server on RustFS, zstd and a slimmer m1 image ([41a6b1d](https://github.com/nesty92/fleetci/commit/41a6b1d1f5231ebdd659d4b865162509ac25e5ab))
* **hack:** m1 builds fleetingd from our fork and can run 1vcpu-2gb on it too ([3e0c632](https://github.com/nesty92/fleetci/commit/3e0c6322180f4c3d683a394261c6307157d617cb))
* **hack:** m1 kit with a Cloud Hypervisor microVM class through fleetingd ([15b9190](https://github.com/nesty92/fleetci/commit/15b919085d5935911ea0e4c33ed4186aa5521791))
* **hack:** m1's fleetingd groups share one image and build it at start ([eba1871](https://github.com/nesty92/fleetci/commit/eba1871150bfa1862fe6a32990107dcd32558759))
* **hack:** reflink disks and a quiet console for the Firecracker runs ([5a745b2](https://github.com/nesty92/fleetci/commit/5a745b29d9074bf8c62ac740503092b2a1a879f4))
* **hack:** snapshot and restore experiment for the m1 microVMs ([06c8377](https://github.com/nesty92/fleetci/commit/06c8377cdef0f6c55826054b4370b0a7e7a33cdb))
* **hack:** snapshot clones re-addressed by a vsock agent, in fleetingd's network model ([df61a17](https://github.com/nesty92/fleetci/commit/df61a176b7884da3118c197a4b3a0798b73df416))
* public GitLab CI template with a standard class ladder ([e7c8cb9](https://github.com/nesty92/fleetci/commit/e7c8cb9d56078103b90a25362a8c52701370fd64))


### Bug Fixes

* **fleet:** start a class's plugin again when it dies ([16d4289](https://github.com/nesty92/fleetci/commit/16d428977992116eb1a14e1a4189846f3e63976a))
* **github:** log the job a runner took, not only the one it was for ([e4dbe82](https://github.com/nesty92/fleetci/commit/e4dbe821c8eaed5717260a47314f54a0ec007377))
* notice a vanished VM in seconds, not when TCP gives up ([1a88de5](https://github.com/nesty92/fleetci/commit/1a88de5ada21926924a5738028ae5fe12d791900))


### Performance Improvements

* **fleet:** look for new instances every second and retry SSH sooner ([0a9b189](https://github.com/nesty92/fleetci/commit/0a9b189e93723d6db50fa1d5b51abdeb1915ff21))

## [0.2.0](https://github.com/nesty92/fleetci/compare/v0.1.0...v0.2.0) (2026-11-04)


### Features

* fleetci ci-include, and install steps from nothing to a first job ([4065de9](https://github.com/nesty92/fleetci/commit/4065de9b2510b1cc00d825887cfbc5906f37f220))

## 0.1.0 (2026-11-04)


### Features

* publish releases with binaries and checksums ([d52e64c](https://github.com/nesty92/fleetci/commit/d52e64c07e23351ec733825e092e44e83f9a3754))


### Continuous Integration

* use the maintained release-please action ([de28d42](https://github.com/nesty92/fleetci/commit/de28d429d9aa12792346b7eabb6b0375b3268ab7))
