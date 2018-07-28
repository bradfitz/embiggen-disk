# embiggen-disk

The **embiggen-disk** tool live-resizes a filesystem after first live-resizing
any necessary layers below it: an optional LVM LV and PV, and an MBR or GPT
partition table.

# Example

```
# embiggen-disk /
Changes made:
  * partition /dev/sda3: before: 8442546176 sectors, after: 8444643328 sectors
  * LVM PV /dev/sda3: before: sectors=8442544128, after: sectors=8444641280
  * LVM LV /dev/mapper/debvg-root: before: sectors=8442544128, after: sectors=8444641280
  * ext4 filesystem at /: before: 1038833256 blocks, after: 1039091312 blocks
```

Then again:

```
# embiggen-disk /
No changes made.
```

# Installing

```
$ go get github.com/google/embiggen-disk
```

# Requirements

* Go 1.7+
* Linux 3.6+ (for [BLKPG_RESIZE_PARTITION](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/commit/?id=c83f6bf98dc1f1a194118b3830706cebbebda8c4))

It's only been tested on 64-bit x86 Linux ("amd64"). It should work on
other Linux architectures.

# Disclaimer

This is not an officially supported Google product.

Audit the code and/or snapshot your disk before use if you're worried about losing data.
