# Raw disks on reflinks

Status: measured; not built. The change is in fleetingd's plugin
(fleeting-plugin-fleetingd), plus a filesystem on the host.

## Why

Each VM's disk today is a qcow2 overlay on the class's prebuilt image:
created in 7 ms, holding only what the VM writes. Cloud Hypervisor's qcow2
path is the slow part of a job's disk I/O, and io_uring doesn't change that.
A raw disk is 2–3× faster at writing. Copying the 20 GB image for each VM
takes 1.6 s, too slow for single-use VMs. A reflink clone (`cp --reflink`,
the `FICLONE` ioctl) takes 4–12 ms: the clone shares every block with the
image until the VM writes to it, so it costs no space and no copy.

## Measured

One machine (16 threads, NVMe, ext4 root), a 2 vCPU / 4 GB VM, Cloud
Hypervisor v54, Ubuntu 26.04. fio in the guest: direct I/O on a 2 GB file,
15 s per test; untar is 1.4 GB, 60,823 files, then `sync`, caches dropped
first. 1–2 runs each: read them as direction, not exact figures.

| Disk | Random write 4k qd32 | Sequential write 1M | Untar + sync |
| --- | --- | --- | --- |
| qcow2 overlay (today) | 29–31k IOPS | 1.32–1.38 GB/s | 3.5–4.4 s |
| raw, full copy, no io_uring | 44k IOPS | 2.8 GB/s | 2.8–3.7 s |
| raw, full copy, io_uring | 81k IOPS | 3.1 GB/s | 3.3–4.1 s |
| **raw, XFS reflink clone, io_uring** | **90k IOPS** | **3.8 GB/s** | **3.3–4.4 s** |

Random reads were too noisy to compare (146k–263k IOPS across runs, mostly
from the host's page cache). io_uring on qcow2 made no measurable
difference.

Making a VM's disk:

| How | Time | Space |
| --- | --- | --- |
| qcow2 overlay on ext4 (today) | 7 ms | what the VM writes |
| reflink clone of a 20 GB raw image, XFS | 11–13 ms | 10 clones: 0 MB, then what each VM writes |
| reflink clone, btrfs | 3–4 ms | 10 clones: 1 MB |
| full sparse copy on ext4 | 1,619 ms | 2.2 GB per VM, more as it writes |
| qcow2 image to raw, once per image | 1.7–2.1 s | 2.2 GB of 20 GB (sparse) |

The XFS and btrfs here were loop-mounted files on the ext4 root. A real
partition removes that layer, so it should be at least as fast.

The clone grew to 7.4 GB after the benchmark: the job's 2 GB fio file and
untar runs. That space is freed when the VM's disk is deleted.

## What the host needs

Reflinks only work within one filesystem. The images and the VMs' disks
(fleetingd's `vm_image_directory` and `vm_disk_directory`, under fleetci's
`state_dir`) must be on the same XFS or btrfs filesystem. ext4 can't
reflink.

| Host | Setup | Effort |
| --- | --- | --- |
| a spare disk or partition | `mkfs.xfs /dev/X`, mount at `/var/lib/fleetci`, add to `/etc/fstab` | 5 minutes |
| LVM with free space | `lvcreate -L 200G -n fleetci vg && mkfs.xfs /dev/vg/fleetci`, mount and fstab | 5 minutes |
| a cloud VM | attach a volume, or use the instance's local NVMe, as XFS (cloud-init `disk_setup`, `fs_setup`, `mounts`) | one block in the host's cloud-init |
| root is already XFS (RHEL, Rocky, Amazon Linux) or btrfs (Fedora, openSUSE) | nothing | none |
| only an ext4 root, no free space (this laptop) | a loop file: `truncate -s 200G /var/lib/fleetci.xfs && mkfs.xfs /var/lib/fleetci.xfs`, mounted at boot by a systemd `.mount` unit | 10 minutes; a filesystem on top of a filesystem, but measured above at full speed |
| shrink ext4 for a new partition | offline resize from a live USB | an hour, with some risk; not worth it when a loop file works |

`mkfs.xfs` enables reflink by default since xfsprogs 5.1 (2019). Check it
with `xfs_info /var/lib/fleetci | grep reflink=1`. On btrfs, reflinks are
always on.

`fleetci check` should test the host: clone a small file in
`vm_disk_directory` with `FICLONE`, and say whether disks will be raw clones
or qcow2 overlays.

## What fleetingd changes

All in fleeting-plugin-fleetingd; a few hundred lines with tests.

1. **Detect.** At start, try `FICLONE` (`unix.IoctlFileClone`) between two
   files in `vm_disk_directory`. If it works, disks are raw clones. If not
   (`EOPNOTSUPP` on ext4, `EXDEV` across filesystems), keep the qcow2
   overlays as today. A `vm_disk_format = "auto" | "qcow2" | "raw"`
   setting can force either.
2. **Base image.** After the prebuild or download, convert it once to
   `prebuilt-<key>.raw` with `qemu-img convert -O raw`, sized to
   `vm_disk_size_gb`, sparse. That takes about 2 s per image. The cleanup of
   old images removes the `.raw` file with its `.qcow2`.
3. **Per VM.** `makeDisk` clones the raw image instead of
   `qemu-img create -b` (vm_images.go). The disk argument becomes
   `image_type=raw`, with no `backing_files=on`. The existing
   `vm_full_disk_copy` path becomes the clone path: a copy that is now
   instant.
4. **Snapshots.** The template's disk is a raw clone, and each restore
   clones the template's disk. The check in snapshot.go that a template
   disk is an overlay on the current image needs a raw form, such as a
   marker file naming the image it was cloned from, since a raw file has no
   backing header.
5. **io_uring.** Raw disks gain from io_uring (44k → 81k random-write IOPS),
   so fleetingd inside a container needs a seccomp profile that allows
   `io_uring_setup`, `io_uring_enter` and `io_uring_register`. Docker's
   default blocks them, and Cloud Hypervisor then falls back to a slower
   synchronous backend. On a host, outside a container, nothing is needed.

The guest can't free space with `fstrim`: Cloud Hypervisor warns
"sparse=on requested but backend does not support sparse operations" for
these disks. For single-use VMs that doesn't matter, since the whole file is
deleted with the VM.

## The cache disk

The per-project cache disk on `feat/cache-trees` (`/cache/docker`,
`/cache/containerd`) is a chain of qcow2 layers: a new, empty layer for each
job's VM, kept afterwards, and sent between workers layer by layer. That is
where image pulls and layer unpacking write, often the most write-heavy part
of a job. A raw form of it is a bigger change than the root disk, because
the layers are the unit both of keeping and of sharing:

- on one worker, a cache could be a raw file cloned per VM, the VM's
  clone kept as the next version, and older versions dropped (no chain);
- between workers, a raw cache has no layer to send. It could send the
  blocks that changed (`FIEMAP` against the clone's parent), or keep qcow2
  for transfer and convert on arrival.

Do the root disk first, and measure what a raw cache disk gains on real
jobs before redesigning it.

Measured since, on the lab's real caches: a raw reflink clone on XFS in a
file was no faster than the qcow2 chain, and 18% slower at synced writes
([measurements](../measurements.md#the-cache-disk-qcow2-layers-a-raw-copy-a-reflink-clone)).
The cache disk stays qcow2.
