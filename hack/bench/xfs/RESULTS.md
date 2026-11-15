# Results: run 43737016496 (2026-11-15)

Two GitHub-hosted ubuntu-24.04 runners: 4 vCPU, 16 GB, AWS (kernel 7.0.0-1014-aws). Root is ext4 on a device-mapper snapshot of EBS, its writes on the instance's local NVMe. No spare disk, so XFS is on a direct-I/O loop device over a preallocated file on that root. Re-parsed from the serial logs (the run's own summary dropped each VM's first line). 3 rounds, randomized order.

## Machine 1

### 1 VM at once

| median | ext4qcow2, warm cache | qcow2, warm cache | reflink, warm cache | ext4qcow2, cold cache | qcow2, cold cache | reflink, cold cache | reflink ÷ qcow2 on XFS, warm | cold |
|---|---|---|---|---|---|---|---|---|
| make_disk_ms | 30 (3) | 28 (3) | 74 (3) | 31 (3) | 34 (3) | 21 (3) | 2.64× | 0.62× |
| boot_ms | 2,820 (3) | 2,800 (3) | 2,900 (3) | 2,850 (3) | 2,880 (3) | 2,830 (3) | 1.04× | 0.98× |
| read_tree_ms | 4,744 (3) | 4,738 (3) | 8,902 (3) | 8,216 (3) | 8,961 (3) | 8,409 (3) | 1.88× | 0.94× |
| read_big_ms | 731 (3) | 732 (3) | 1,144 (3) | 1,019 (3) | 1,065 (3) | 626 (3) | 1.56× | 0.59× |
| copy_tree_ms | 9,085 (3) | 9,136 (3) | 7,900 (3) | 9,115 (3) | 8,305 (3) | 7,916 (3) | 0.86× | 0.95× |
| delete_tree_ms | 557 (3) | 545 (3) | 620 (3) | 541 (3) | 576 (3) | 540 (3) | 1.14× | 0.94× |
| randwrite_iops ↑ | 23,133 (3) | 21,249 (3) | 64,282 (3) | 23,039 (3) | 17,924 (3) | 67,528 (3) | 3.03× | 3.77× |
| randwrite_p99_us | 2,179 (3) | 3,097 (3) | 2,507 (3) | 2,277 (3) | 4,112 (3) | 2,441 (3) | 0.81× | 0.59× |
| syncwrite_iops ↑ | 460 (3) | 307 (3) | 831 (3) | 396 (3) | 295 (3) | 807 (3) | 2.71× | 2.74× |
| wall_ms | 40,759 (3) | 40,979 (3) | 44,794 (3) | 44,825 (3) | 44,868 (3) | 43,160 (3) | 1.09× | 0.96× |
| host_io_wait_ms | 4,244 (3) | 4,788 (3) | 5,969 (3) | 4,818 (3) | 5,293 (3) | 4,954 (3) | 1.25× | 0.94× |

### 4 VMs at once

| median | ext4qcow2, warm cache | qcow2, warm cache | reflink, warm cache | ext4qcow2, cold cache | qcow2, cold cache | reflink, cold cache | reflink ÷ qcow2 on XFS, warm | cold |
|---|---|---|---|---|---|---|---|---|
| make_disk_ms | 7 (12) | 7 (12) | 17 (12) | 7 (12) | 20 (12) | 16 (12) | 2.43× | 0.82× |
| boot_ms | 4,880 (12) | 4,880 (12) | 5,085 (12) | 4,970 (12) | 5,010 (12) | 4,960 (12) | 1.04× | 0.99× |
| read_tree_ms | 13,465 (12) | 13,148 (12) | 24,724 (12) | 14,973 (12) | 15,267 (12) | 20,869 (12) | 1.88× | 1.37× |
| read_big_ms | 1,348 (12) | 1,269 (12) | 3,612 (12) | 1,508 (12) | 1,488 (12) | 4,180 (12) | 2.85× | 2.81× |
| copy_tree_ms | 28,759 (12) | 29,445 (12) | 30,126 (12) | 29,230 (12) | 31,864 (12) | 30,396 (12) | 1.02× | 0.95× |
| delete_tree_ms | 1,074 (12) | 1,172 (12) | 1,487 (12) | 1,198 (12) | 1,394 (12) | 1,636 (12) | 1.27× | 1.17× |
| randwrite_iops ↑ | 1,819 (12) | 2,051 (12) | 13,940 (12) | 1,864 (12) | 1,541 (12) | 13,420 (12) | 6.80× | 8.71× |
| randwrite_p99_us | 51,642 (12) | 116,916 (12) | 21,364 (12) | 46,400 (12) | 120,586 (12) | 18,874 (12) | 0.18× | 0.16× |
| syncwrite_iops ↑ | 91 (12) | 46 (12) | 198 (12) | 109 (12) | 59 (12) | 277 (12) | 4.35× | 4.69× |
| wall_ms | 74,304 (12) | 75,946 (12) | 92,997 (12) | 76,705 (12) | 81,404 (12) | 87,733 (12) | 1.22× | 1.08× |
| host_io_wait_ms | 32,359 (3) | 31,121 (3) | 11,920 (3) | 30,553 (3) | 32,621 (3) | 16,615 (3) | 0.38× | 0.51× |

ext4qcow2: qcow2 overlays on the runner's ext4 root partition, as fleetci does today. qcow2 and reflink: on the XFS.
(n) is the number of samples: VMs × rounds. ↑ higher is better; the rest are times, lower is better.

## Machine 2

### 1 VM at once

| median | ext4qcow2, warm cache | qcow2, warm cache | reflink, warm cache | ext4qcow2, cold cache | qcow2, cold cache | reflink, cold cache | reflink ÷ qcow2 on XFS, warm | cold |
|---|---|---|---|---|---|---|---|---|
| make_disk_ms | 32 (3) | 37 (3) | 23 (3) | 34 (3) | 38 (3) | 20 (3) | 0.62× | 0.53× |
| boot_ms | 2,750 (3) | 2,760 (3) | 2,770 (3) | 2,820 (3) | 2,860 (3) | 2,760 (3) | 1.00× | 0.97× |
| read_tree_ms | 4,668 (3) | 4,616 (3) | 8,873 (3) | 7,765 (3) | 8,854 (3) | 8,056 (3) | 1.92× | 0.91× |
| read_big_ms | 715 (3) | 697 (3) | 1,091 (3) | 968 (3) | 1,175 (3) | 762 (3) | 1.57× | 0.65× |
| copy_tree_ms | 8,527 (3) | 8,760 (3) | 7,479 (3) | 9,152 (3) | 8,818 (3) | 7,050 (3) | 0.85× | 0.80× |
| delete_tree_ms | 528 (3) | 516 (3) | 527 (3) | 532 (3) | 551 (3) | 518 (3) | 1.02× | 0.94× |
| randwrite_iops ↑ | 36,079 (3) | 20,650 (3) | 67,738 (3) | 23,961 (3) | 21,416 (3) | 88,019 (3) | 3.28× | 4.11× |
| randwrite_p99_us | 1,712 (3) | 3,326 (3) | 2,507 (3) | 2,114 (3) | 3,097 (3) | 1,810 (3) | 0.75× | 0.58× |
| syncwrite_iops ↑ | 346 (3) | 345 (3) | 1,127 (3) | 440 (3) | 329 (3) | 767 (3) | 3.27× | 2.33× |
| wall_ms | 39,925 (3) | 40,162 (3) | 43,260 (3) | 44,697 (3) | 45,207 (3) | 42,263 (3) | 1.08× | 0.93× |
| host_io_wait_ms | 5,395 (3) | 5,017 (3) | 3,523 (3) | 5,006 (3) | 6,237 (3) | 5,435 (3) | 0.70× | 0.87× |

### 4 VMs at once

| median | ext4qcow2, warm cache | qcow2, warm cache | reflink, warm cache | ext4qcow2, cold cache | qcow2, cold cache | reflink, cold cache | reflink ÷ qcow2 on XFS, warm | cold |
|---|---|---|---|---|---|---|---|---|
| make_disk_ms | 6 (12) | 7 (12) | 16 (12) | 7 (12) | 6 (12) | 16 (12) | 2.21× | 2.58× |
| boot_ms | 4,835 (12) | 4,785 (12) | 4,905 (12) | 4,765 (12) | 4,815 (12) | 4,890 (12) | 1.03× | 1.02× |
| read_tree_ms | 12,956 (12) | 13,050 (12) | 22,425 (12) | 14,460 (12) | 15,022 (12) | 20,169 (12) | 1.72× | 1.34× |
| read_big_ms | 1,336 (12) | 1,300 (12) | 4,131 (12) | 1,492 (12) | 1,364 (12) | 2,854 (12) | 3.18× | 2.09× |
| copy_tree_ms | 29,019 (12) | 29,838 (12) | 31,434 (12) | 29,535 (12) | 30,118 (12) | 29,712 (12) | 1.05× | 0.99× |
| delete_tree_ms | 1,046 (12) | 1,134 (12) | 1,246 (12) | 1,190 (12) | 1,320 (12) | 1,432 (12) | 1.10× | 1.08× |
| randwrite_iops ↑ | 1,802 (12) | 1,659 (12) | 15,186 (12) | 2,052 (12) | 1,574 (12) | 14,671 (12) | 9.15× | 9.32× |
| randwrite_p99_us | 50,332 (12) | 133,431 (12) | 17,564 (12) | 45,875 (12) | 117,440 (12) | 21,364 (12) | 0.13× | 0.18× |
| syncwrite_iops ↑ | 85 (12) | 40 (12) | 56 (12) | 101 (12) | 60 (12) | 230 (12) | 1.41× | 3.84× |
| wall_ms | 73,708 (12) | 75,322 (12) | 90,982 (12) | 76,082 (12) | 79,121 (12) | 87,778 (12) | 1.21× | 1.11× |
| host_io_wait_ms | 32,032 (3) | 31,705 (3) | 17,310 (3) | 30,411 (3) | 31,051 (3) | 16,373 (3) | 0.55× | 0.53× |

ext4qcow2: qcow2 overlays on the runner's ext4 root partition, as fleetci does today. qcow2 and reflink: on the XFS.
(n) is the number of samples: VMs × rounds. ↑ higher is better; the rest are times, lower is better.
