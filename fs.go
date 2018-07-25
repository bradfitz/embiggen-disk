package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func getFileSystemResizer(mnt string) (Resizer, error) {
	fs, err := statFS(mnt)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	switch fs.fstype {
	case "ext2", "ext3", "ext4":
		cmd = exec.Command("resize2fs", fs.dev)
		return fsResizer{fs, cmd}, nil
	case "xfs":
		cmd = exec.Command("xfs_growfs", "-d", fs.mnt)
		return fsResizer{fs, cmd}, nil
	case "btrfs":
		cmd = exec.Command("btrfs", "filesystem", "resize", "max", fs.mnt)
		return fsResizer{fs, cmd}, nil
	}
	return nil, fmt.Errorf("unsupported filesystem type %q", fs.fstype)
}

type fsResizer struct {
	fs  fsStat
	cmd *exec.Cmd
}

func (e fsResizer) String() string {
	return fmt.Sprintf("%s filesystem at %s", e.fs.fstype, e.fs.mnt)
}

func (e fsResizer) DepResizer() (Resizer, error) {
	// TODO: use /proc/devices instead and stat the thing to
	// figure out what it is, rather than using its name.
	if strings.HasPrefix(e.fs.dev, "/dev/sd") {
		return partitionResizer(e.fs.dev), nil
	}
	if strings.HasPrefix(e.fs.dev, "/dev/mapper") ||
		strings.HasPrefix(filepath.Base(e.fs.dev), "dm-") {
		return lvResizer(e.fs.dev), nil
	}
	return nil, fmt.Errorf("don't know how to resize block device %q", e.fs.dev)
}

func (e fsResizer) Resize() error {
	out, err := e.cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running %v %v: %v, %s", e.cmd.Path, e.cmd.Args, err, out)
	}
	return nil
}

func (e fsResizer) State() (string, error) {
	st, err := statFS(e.fs.mnt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v blocks", st.statfs.Blocks), nil
}

type fsStat struct {
	mnt    string
	dev    string
	fstype string
	statfs unix.Statfs_t
}

func statFS(mnt string) (fs fsStat, err error) {
	err = unix.Statfs(mnt, &fs.statfs)
	if err != nil {
		return
	}
	mounts, err := ioutil.ReadFile("/proc/mounts")
	if err != nil {
		return
	}
	bs := bufio.NewScanner(bytes.NewReader(mounts))
	for bs.Scan() {
		f := strings.Fields(bs.Text())
		if len(f) < 3 {
			continue
		}
		if f[1] == mnt {
			fs.mnt = mnt
			fs.dev = f[0]
			fs.fstype = f[2]
			return fs, nil
		}
	}
	return fs, errors.New("mount point not found")
}
