"""How much would block deduplication save on a set of cache layers?

    python3 -I dedup.py hash <out> <group> <layer.qcow2>...
    python3 -I dedup.py report <out>...

hash reads the layers in 4, 64 and 128 KiB blocks, as a host filesystem
that deduplicates (btrfs or XFS with duperemove or bees, ZFS with dedup)
would see them, and writes each size's block hashes to <out>, sorted and
with the group's own repeats dropped, and how many blocks it read. All-zero
blocks are skipped: qcow2 doesn't store them. qcow2 keeps the guest's
blocks at their offsets within each 64 KiB cluster, so the guest's ext4
blocks are 4 KiB-aligned in the file too. A group is a label, such as the
language, to tell repeats within one project's chain of layers from those
across projects. Its hashes take 8 bytes a block, so the layers can be
deleted before the next group's are made.

report merges groups' hash files and prints, for each size: the data,
what is left after deduplication, and how much of what it saves is across
groups. Then what zlib level 1, a lower bound for zstd, takes off a sample.
"""
import array
import hashlib
import heapq
import json
import random
import sys
import zlib

SIZES = [4 << 10, 64 << 10, 128 << 10]
GB = 1 << 30


def hash_layers(out, group, paths):
    zero = {s: bytes(s) for s in SIZES}
    hashes = {s: array.array("Q") for s in SIZES}
    blocks = {s: 0 for s in SIZES}
    rng = random.Random(1)
    raw = packed = 0
    for path in paths:
        with open(path, "rb") as f:
            while True:
                big = f.read(128 << 10)
                if not big:
                    break
                for s in SIZES:
                    for off in range(0, len(big), s):
                        b = big[off:off + s]
                        if b == zero[s][:len(b)]:
                            continue
                        blocks[s] += 1
                        h = hashlib.blake2b(b, digest_size=8).digest()
                        hashes[s].append(int.from_bytes(h, "little"))
                        if s == 4 << 10 and rng.random() < 1 / 64:
                            raw += len(b)
                            packed += len(zlib.compress(b, 1))
    meta = {"group": group, "blocks": {}, "zlib": [raw, packed]}
    with open(out, "wb") as f:
        for s in SIZES:
            uniq = array.array("Q", sorted(set(hashes[s])))
            meta["blocks"][str(s)] = [blocks[s], len(uniq)]
            uniq.tofile(f)
    with open(out + ".json", "w") as f:
        json.dump(meta, f)
    b = meta["blocks"][str(4 << 10)]
    print(f"{group}: {b[0] * 4 / (1 << 20):.2f} GB in 4 KiB blocks, {b[1] * 4 / (1 << 20):.2f} GB of them unique")


def report(outs):
    groups = []
    for out in outs:
        with open(out + ".json") as f:
            groups.append((out, json.load(f)))
    raw = sum(m["zlib"][0] for _, m in groups)
    packed = sum(m["zlib"][1] for _, m in groups)
    for i, s in enumerate(SIZES):
        total = within = 0
        lists = []
        for out, m in groups:
            n, u = m["blocks"][str(s)]
            total += n
            within += u
            skip = sum(m["blocks"][str(t)][1] for t in SIZES[:i])
            a = array.array("Q")
            with open(out, "rb") as f:
                f.seek(skip * 8)
                a.fromfile(f, u)
            lists.append(a)
        # Each list is sorted and unique, so the union is a merge.
        left, last = 0, None
        for h in heapq.merge(*lists):
            if h != last:
                left, last = left + 1, h
        saved = total - left
        print(f"{s >> 10:>4} KiB blocks: data {total * s / GB:.2f} GB, after dedup {left * s / GB:.2f} GB "
              f"(saves {100 * saved / max(total, 1):.0f}%), "
              f"{100 * (within - left) / max(saved, 1):.0f}% of it across groups")
    print(f"zlib level 1 on a sample of {raw >> 20} MB: {100 * (1 - packed / max(raw, 1)):.0f}% smaller")
    print("each group, 4 KiB blocks:")
    for _, m in groups:
        n, u = m["blocks"][str(4 << 10)]
        print(f"  {m['group']}: {n * 4 / (1 << 20):.2f} GB, {u * 4 / (1 << 20):.2f} GB unique within it")


if __name__ == "__main__":
    if len(sys.argv) >= 4 and sys.argv[1] == "hash":
        hash_layers(sys.argv[2], sys.argv[3], sys.argv[4:])
    elif len(sys.argv) >= 3 and sys.argv[1] == "report":
        report(sys.argv[2:])
    else:
        sys.exit(__doc__)
