/*
Copyright 2018 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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
	dev := e.fs.dev
	if dev == "/dev/root" {
		return nil, errors.New("unexpected device /dev/root from statFS")
	}
	if (strings.HasPrefix(dev, "/dev/sd") ||
		strings.HasPrefix(dev, "/dev/vd") ||
		strings.HasPrefix(dev, "/dev/mmcblk") ||
		strings.HasPrefix(dev, "/dev/nvme")) &&
		devEndsInNumber(dev) {
		vlogf("fsResizer.DepResizer: returning partitionResizer(%q)", dev)
		return partitionResizer(dev), nil
	}
	if strings.HasPrefix(dev, "/dev/mapper") ||
		strings.HasPrefix(filepath.Base(dev), "dm-") {
		return lvResizer(dev), nil
	}
	return nil, fmt.Errorf("don't know how to resize block device %q", dev)
}

func (e fsResizer) Resize() error {
	if *dry {
		fmt.Printf("[dry-run] would've run %v %v\n", e.cmd.Path, e.cmd.Args)
		return nil
	}
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
		if f[0] == "rootfs" {
			// See https://github.com/google/embiggen-disk/issues/6
			continue
		}
		if f[1] == mnt {
			fs.mnt = mnt
			fs.dev = f[0]
			fs.fstype = f[2]
			if fs.dev == "/dev/root" {
				dev, err := findDevRoot()
				if err != nil {
					return fs, fmt.Errorf("failed to map /dev/root to real device: %v", err)
				}
				fs.dev = dev
			}
			return fs, err
		}
	}
	return fs, errors.New("mount point not found")
}

// findDevRoot finds which block device (e.g. "/dev/nvme0n1p1") patches the device number of /dev/root.
func findDevRoot() (string, error) {
	fis, err := ioutil.ReadDir("/dev")
	if err != nil {
		return "", err
	}
	dev := map[string]uint64{}
	for _, fi := range fis {
		if fi.Mode()&os.ModeDevice == 0 || fi.Mode()&os.ModeCharDevice != 0 {
			continue
		}
		dev[fi.Name()] = fi.Sys().(*syscall.Stat_t).Rdev
	}
	wantDevnum, ok := dev["root"]
	if !ok {
		return "", errors.New("/dev/root not found in /dev")
	}
	for baseName, devNum := range dev {
		if devNum == wantDevnum && baseName != "root" {
			return "/dev/" + baseName, nil
		}
	}
	return "", errors.New("no block device in /dev had device number like /dev/root")
}
