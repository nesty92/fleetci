# Changelog

## [0.7.1](https://github.com/nesty92/fleetci/compare/v0.7.0...v0.7.1) (2026-11-10)


### Bug Fixes

* **release:** find the binaries through GoReleaser's artifacts.json ([#33](https://github.com/nesty92/fleetci/issues/33)) ([fcff245](https://github.com/nesty92/fleetci/commit/fcff24533aff4a3d3a303819fff1cacb9450415c))

## [0.7.0](https://github.com/nesty92/fleetci/compare/v0.6.0...v0.7.0) (2026-11-10)


### Features

* a fleet of machines run from controllers ([#31](https://github.com/nesty92/fleetci/issues/31)) ([8503255](https://github.com/nesty92/fleetci/commit/85032553f9e86429353f5f403ef950547e0893ca))
* a restart of fleetci serve keeps the GitHub jobs running on its VMs ([#30](https://github.com/nesty92/fleetci/issues/30)) ([275b04f](https://github.com/nesty92/fleetci/commit/275b04f9c267493d5ddcf292bcf8614fd9592e6e))
* **github:** serve whole orgs, sign in as a GitHub App, and start jobs from webhooks ([#25](https://github.com/nesty92/fleetci/issues/25)) ([6ac3617](https://github.com/nesty92/fleetci/commit/6ac3617e5bef0d4cbe0cae3ac3c23bfe238b0b91)), closes [#12](https://github.com/nesty92/fleetci/issues/12) [#13](https://github.com/nesty92/fleetci/issues/13)
* **images:** a small VM image for fleetingd, built and published by CI ([#27](https://github.com/nesty92/fleetci/issues/27)) ([5b40b46](https://github.com/nesty92/fleetci/commit/5b40b460f6efa66ab71adac4b7db19a8ecc9f70d))
* run workflows written for GitHub's hosted runners unchanged ([#29](https://github.com/nesty92/fleetci/issues/29)) ([a079b47](https://github.com/nesty92/fleetci/commit/a079b472d7b97b9b42b86465920abb183a8c00d0))


### Performance Improvements

* a waiting job gets its runner in about 1 s instead of 6 ([#28](https://github.com/nesty92/fleetci/issues/28)) ([911ea05](https://github.com/nesty92/fleetci/commit/911ea057d247770cdab2066761eac83e7cfca7dd))

## [0.6.0](https://github.com/nesty92/fleetci/compare/v0.5.0...v0.6.0) (2026-11-07)


### Features

* **fleet:** stop placing jobs on a VM with a spot interruption notice ([91a4888](https://github.com/nesty92/fleetci/commit/91a4888d01ac956fa38b5122b25b64afee7069d4))
* **gitlab:** run a gitlab-runner config.toml's runners embedded ([1d4fe59](https://github.com/nesty92/fleetci/commit/1d4fe59b5edc20f2c2aa961a52cdf4d604674e09))
* **gitlab:** run GitLab jobs in fleetci's own process ([8a9bc79](https://github.com/nesty92/fleetci/commit/8a9bc79199cb9cfbf47235cd20240dacd669fe0c))
* **source:** put job sources behind an interface, as fleeting's providers are ([ccff76e](https://github.com/nesty92/fleetci/commit/ccff76e929af5c3e365e4bfb77ab320ea388c426))


### Bug Fixes

* **fleet:** skip hosts the plugin reports unhealthy before placing a job ([22ad22c](https://github.com/nesty92/fleetci/commit/22ad22c9bcf87eeaea13a06d2a14c0841ad8a8cb)), closes [#6](https://github.com/nesty92/fleetci/issues/6)
* **github:** wait out rate limits, remove orphaned runners, serve a job whose runner took another ([3978e55](https://github.com/nesty92/fleetci/commit/3978e5522fccd381af399c26399cf8d19575a597))
* **proxy:** bound the ssh handshake, and fuzz the shared connection under network faults ([1bd9d22](https://github.com/nesty92/fleetci/commit/1bd9d226cb94151c5831e792a4283c368f902a34))
* **reclaim:** keep watching a host through IMDS throttling, errors and resets ([9726948](https://github.com/nesty92/fleetci/commit/9726948721931e64acc4fbe7f23ea4592f56909e))

## [0.5.0](https://github.com/nesty92/fleetci/compare/v0.4.0...v0.5.0) (2026-11-06)


### Features

* **fleet:** classes on this machine share its CPUs and memory ([63b476f](https://github.com/nesty92/fleetci/commit/63b476f70d0f12db8d398258f40b4623f05dddaa))
* **fleet:** nominate a job that waited 10 minutes to its host ([e5f2820](https://github.com/nesty92/fleetci/commit/e5f2820528327eb2766f83adb0a768338dd739dc))
* **fleet:** place a class's jobs across its hosts, packing by default ([19a879c](https://github.com/nesty92/fleetci/commit/19a879cc1c6ebf7e1b740af5cdff68d75388e964))

## [0.4.0](https://github.com/nesty92/fleetci/compare/v0.3.0...v0.4.0) (2026-11-05)


### Features

* **fleet:** overflow sends a full class's jobs to another class ([77995aa](https://github.com/nesty92/fleetci/commit/77995aa3d6db0318f383d3503f00d31e93d652b8))
* **fleet:** reuse VMs per class for GitLab ([f0dba0e](https://github.com/nesty92/fleetci/commit/f0dba0e7c91c819ebcb68d778ceb45a32c29c9de))

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
