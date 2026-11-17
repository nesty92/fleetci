#!/bin/bash
# Runs in each bench VM at boot (bench.service), when the kernel command
# line has bench.vm=N. Prints "BENCH key=value" lines on the serial
# console, then powers off. The workloads are a CI job's disk work: read
# what the image holds, copy a tree of files, delete it, write at random
# into new space, and synced small writes.
out=/dev/ttyS0
say() { echo "BENCH $*" > $out; }
ms() { echo $(( $(date +%s%N) / 1000000 )); }
fio1() { fio --name=t --filename=/root/fio --direct=1 --ioengine=libaio --group_reporting --output-format=json "$@" 2>/dev/null; }

say boot_ms=$(awk '{printf "%d", $1 * 1000}' /proc/uptime)
t=$(ms); tar cf - -C /data tree | wc -c > /dev/null; say read_tree_ms=$(( $(ms) - t ))
t=$(ms); cat /data/big.bin > /dev/null; say read_big_ms=$(( $(ms) - t ))
t=$(ms); cp -a /data/tree /root/copy && sync; say copy_tree_ms=$(( $(ms) - t ))
t=$(ms); rm -rf /root/copy && sync; say delete_tree_ms=$(( $(ms) - t ))
j=$(fio1 --rw=randwrite --bs=4k --iodepth=32 --size=2G --runtime=10 --time_based)
say randwrite_iops=$(jq -r '.jobs[0].write.iops | round' <<<"$j") randwrite_p99_us=$(jq -r '.jobs[0].write.clat_ns.percentile["99.000000"] / 1000 | round' <<<"$j")
rm -f /root/fio
j=$(fio1 --rw=randwrite --bs=4k --iodepth=1 --fsync=1 --size=256M --runtime=10 --time_based)
say syncwrite_iops=$(jq -r '.jobs[0].write.iops | round' <<<"$j")
rm -f /root/fio
say done=1
sync
systemctl poweroff
