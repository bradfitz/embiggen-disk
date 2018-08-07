// +build linux

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
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/u-root/u-root/pkg/cpio"
)

const (
	// kernelRef is the git reference in the github.com/google/embiggen-disk repo
	// that contains the Linux kernel blob used by the qemu integration test.
	// It's in a separate ref so normal "go get" users don't need to download
	// a few MB for a test they likely won't run. Instead, we fetch it at runtime
	// as needed.
	kernelRef = "refs/data/linux-kernel-001"

	// kernelObj is the Linux kernel object referenced by kernelRef.
	// Its kernel config is stored in notes/test-kernel-config.txt
	kernelObj = "07f3c3a7d08340bdc292cf483cad9ff6cd0938d7"
)

var inQemu bool

func init() {
	if os.Getpid() != 1 {
		// Not in qemu.
		return
	}
	inQemu = true

	fmt.Println(":: hello from userspace")

	// Mount things expected by various tools (sfdisk, lsblk, etc).
	for _, mnt := range []struct {
		dev, path, fstype string
	}{
		{"sysfs", "/sys", "sysfs"},
		{"proc", "/proc", "proc"},
		{"udev", "/dev", "devtmpfs"},
	} {
		if err := unix.Mount(mnt.dev, mnt.path, mnt.fstype, 0, ""); err != nil {
			log.Fatalf("failed to mount %s: %v", mnt.path, err)
		}
	}

	// Now that /proc is mounted, get our arguments we passed to the kernel.
	if all, err := ioutil.ReadFile("/proc/cmdline"); err != nil {
		log.Fatal(err)
	} else {
		kernelCmdLineFields = strings.Fields(string(all))
	}

	// Connect to the monitor.
	var err error
	monc, err = dialMon()
	if err != nil {
		log.Fatalf("dialing monitor: %v", err)
	}

	// Be verbose in the qemu guest. We'll filter it out in the parent.
	flag.Lookup("test.v").Value.Set("true")
	flag.Lookup("test.run").Value.Set(kernelParam("goTestRun"))
}

func TestMain(m *testing.M) {
	status := m.Run()
	if inQemu {
		fmt.Printf(":: exit=%d\n", status)
		monc.run("quit") // to avoid a kernel panic with init exiting
	}
	os.Exit(status)
}

// QemuTest is the type on which all the methods for tests that run
// under Qemu as root/PID=1 run.
type QemuTest struct{}

func TestInQemu(t *testing.T) {
	if inQemu {
		rv := reflect.ValueOf(QemuTest{})
		tv := rv.Type()
		for i := 0; i < rv.NumMethod(); i++ {
			t.Run(tv.Method(i).Name, rv.Method(i).Interface().(func(*testing.T)))
		}
		return
	}
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skipf("skipping test due to qemu-system-x86_64 not found: %v", err)
	}
	td, err := ioutil.TempDir("", "embiggen-disk-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(td)

	kernelPath := filepath.Join(td, "bzImage")
	if err := downloadKernel(kernelPath); err != nil {
		t.Fatalf("failed to download linux kernel: %v", err)
	}

	initrdPath := filepath.Join(td, "initrd")
	if err := genRootFS(initrdPath); err != nil {
		t.Fatalf("failed to generate/write initrd: %v", err)
	}

	monSockPath := filepath.Join(td, "monsock")

	// Create some disks to work with.
	for _, name := range []string{"foo"} {
		err := exec.Command("qemu-img", "create", "-f", "qcow2", filepath.Join(td, name+".qcow2"), "10G").Run()
		if err != nil {
			t.Fatalf("creating %s qcow2: %v", name, err)
		}
	}

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mon, err := net.Dial("unix", monSockPath)
			if err != nil {
				c.Close()
				log.Printf("dial unix to monitor failed: %v", err)
				return
			}
			go func() {
				errc := make(chan error, 1)
				go func() {
					_, err := io.Copy(c, mon)
					errc <- err
				}()
				go func() {
					_, err := io.Copy(mon, c)
					errc <- err
				}()
				<-errc
				c.Close()
				mon.Close()
			}()
		}
	}()
	cmd := exec.Command("qemu-system-x86_64",
		"-vga", "none",
		"-nographic",
		"-m", "256",
		"-display", "none",
		"-monitor", "unix:"+monSockPath+",server,nowait",
		"-device", "virtio-net,netdev=net0",
		"-netdev", "user,id=net0,guestfwd=tcp:10.0.2.100:1234-tcp:"+ln.Addr().String(),
		"-device", "virtio-serial",
		"-device", "virtio-scsi-pci,id=scsi",
		"-kernel", kernelPath,
		"-initrd", initrdPath,
		"-no-reboot",
		"-append", "console=ttyS0,115200 panic=-1 acpi=off nosmp ip=dhcp "+
			"parentTempDir="+td+" goTestRun="+flag.Lookup("test.run").Value.String())
	var out bytes.Buffer
	var std io.Writer = &out
	const verbose = true
	if verbose {
		std = io.MultiWriter(std, os.Stderr)
	}
	cmd.Stdout = std
	cmd.Stderr = std
	err = cmd.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("\n:: exit=0")) {
		t.Error("non-zero exit status")
	}
}

type monClient struct {
	c   net.Conn
	buf bytes.Buffer
}

var monc *monClient

func dialMon() (*monClient, error) {
	if !inQemu {
		panic("dialMon is meant for tests in qemu")
	}
	c, err := net.Dial("tcp", "10.0.2.100:1234")
	if err != nil {
		return nil, err
	}
	mc := &monClient{c: c}
	if _, err := mc.readToPrompt(); err != nil {
		return nil, err
	}
	return mc, nil
}

var (
	qemuPrompt = []byte("\r\n(qemu) ")
	ansiCSI_K  = []byte("\x1b[K") // CSI K: Erase in Line; n == 0: clear from cursor to the end of the line
)

func (mc *monClient) readToPrompt() (pre string, err error) {
	buf := make([]byte, 100)
	for {
		n, err := mc.c.Read(buf)
		if err != nil {
			return "", err
		}
		mc.buf.Write(buf[:n])
		have := mc.buf.Bytes()
		if bytes.HasSuffix(have, qemuPrompt) {
			mc.buf.Reset()
			ret := bytes.TrimSuffix(have, qemuPrompt)
			if i := bytes.LastIndex(ret, ansiCSI_K); i != -1 {
				ret = ret[i+len(ansiCSI_K):]
			}
			return strings.TrimSpace(strings.Replace(string(ret), "\r\n", "\n", -1)), nil
		}
	}
}

func (mc *monClient) run(cmd string) (out string, err error) {
	if _, err := fmt.Fprintf(mc.c, "%s\n", cmd); err != nil {
		return "", err
	}
	return mc.readToPrompt()
}

func (mc *monClient) addDisk(t *testing.T, diskBase string) {
	tempDir := kernelParam("parentTempDir")
	if tempDir == "" {
		t.Fatal("missing kernel parameter parentTempDir")
	}
	out, err := monc.run("drive_add 0 file=" + filepath.Join(tempDir, diskBase+".qcow2") + ",if=none,id=" + diskBase)
	if err != nil {
		t.Fatalf("drive_add %q: %v", diskBase, err)
	}
	if out != "OK" {
		t.Fatalf("drive_add %q: %s", diskBase, out)
	}

	out, err = monc.run("device_add scsi-hd,drive=" + diskBase + ",id=" + diskBase)
	if err != nil {
		t.Fatalf("device_add %q: %v", diskBase, err)
	}
	if len(out) > 0 {
		t.Logf("device_add %q: %s", diskBase, out)
	}
}

func (mc *monClient) removeDisk(t *testing.T, diskBase string) {
	out, err := monc.run("device_del " + diskBase)
	if err != nil {
		t.Fatalf("device_del %q: %v", diskBase, err)
	}
	if len(out) > 0 {
		t.Logf("device_del %q: %s", diskBase, out)
	}
}

func (QemuTest) Mon(t *testing.T) {
	if out, err := monc.run("info block"); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("info block: %q", out)
	}
}

func (QemuTest) Mke2fs(t *testing.T) {
	monc.addDisk(t, "foo")
	defer monc.removeDisk(t, "foo")

	st := lsblk(t)
	if !st.contains("sda") {
		t.Fatalf("expected lsblk to contain sda: got: %s", st)
	}

	// Generate partition
	cmd := exec.Command("/sbin/sfdisk", "-f", "/dev/sda")
	cmd.Stdin = strings.NewReader("start=2048, type=83")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sfdisk: %v, %s", err, out)
	}

	st = lsblk(t)
	if !st.contains("sda1") {
		t.Fatalf("expected lsblk to contain sda1: got: %s", st)
	}
	if len(st) != 2 {
		t.Fatalf("wanted 2 devices, got: %s", st)
	}

	// Make a filesystem on it!
	if out, err := exec.Command("/sbin/mke2fs", "/dev/sda1").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v, %s", err, out)
	}

	// Mount it.
	if err := unix.Mount("/dev/sda1", "/mnt/a", "ext4", 0, ""); err != nil {
		t.Fatalf("mount: %v", err)
	}

	t.Logf("Final state: %s", lsblk(t))

	if err := unix.Unmount("/mnt/a/", 0); err != nil {
		t.Fatalf("unmount: %v", err)
	}
}

type lsblkItem struct {
	Name  string
	Size  int64
	Type  string
	Mount string
}

type lsblkState []lsblkItem

func lsblk(t *testing.T) lsblkState {
	t.Helper()
	out, err := exec.Command("/bin/lsblk", "-b", "-l").CombinedOutput()
	if err != nil {
		t.Fatalf("lsblk error: %v, %s", err, out)
	}
	lines := strings.Split(string(out), "\n")
	var st lsblkState
	if len(lines) == 0 {
		return nil
	}
	/* Parse:
	NAME         MAJ:MIN RM         SIZE RO TYPE  MOUNTPOINT
	sda            8:0    0 107374182400  0 disk
	sda1           8:1    0    254803968  0 part  /boot
	sda2           8:2    0         1024  0 part
	*/
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		it := lsblkItem{
			Name: f[0],
			Type: f[5],
		}
		if len(f) == 7 {
			it.Mount = f[6]
		}
		it.Size, _ = strconv.ParseInt(f[3], 10, 64)
		st = append(st, it)
	}
	return st
}

func (s lsblkState) contains(dev string) bool {
	for _, it := range s {
		if it.Name == dev {
			return true
		}
	}
	return false
}

func (s lsblkState) String() string {
	var buf bytes.Buffer
	for _, it := range s {
		fmt.Fprintf(&buf, "%s %s %d", it.Type, it.Name, it.Size)
		if it.Mount != "" {
			fmt.Fprintf(&buf, " at %s", it.Mount)
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

func (QemuTest) Lsblk(t *testing.T) {
	out, err := exec.Command("/bin/lsblk").CombinedOutput()
	if err != nil {
		t.Fatalf("lsblk error: %v, %s", err, out)
	}
	if len(out) > 0 {
		t.Errorf("unexpected lsblk output: %s", out)
	}
}

func downloadKernel(dst string) error {
	out, err := exec.Command("git", "fetch", "https://github.com/google/embiggen-disk.git", kernelRef).Output()
	if err != nil {
		return fmt.Errorf("git fetch: %v, %s", err, out)
	}
	out, err = exec.Command("git", "cat-file", "-p", kernelObj).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git cat-file: %v, %s", err, out)
	}
	if err := ioutil.WriteFile(dst, out, 0644); err != nil {
		return err
	}
	return nil
}

func genRootFS(dst string) error {
	initProg, err := os.Executable()
	if err != nil {
		return err
	}
	files := rootFSFiles(initProg)

	f, err := os.Create(dst)
	if err != nil {
		log.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	recw := cpio.Newc.Writer(bw)

	for _, file := range files {
		rec, err := cpio.GetRecord(file)
		if err != nil {
			log.Fatalf("GetRecord(%q): %v", file, err)
		}
		if file == initProg {
			rec.Info.Name = "init"
		} else {
			rec.Info.Name = cpio.Normalize(rec.Info.Name)

		}
		if err := recw.WriteRecord(rec); err != nil {
			return err
		}
	}

	extraRec := []cpio.Record{
		cpio.Directory("proc", 0755),
		cpio.Directory("sys", 0755),
		cpio.Directory("dev", 0755),
		cpio.Directory("mnt", 0755),
		cpio.Directory("mnt/a", 0755),
		cpio.Directory("mnt/b", 0755),
		cpio.Directory("mnt/c", 0755),
	}
	for _, rec := range extraRec {
		if err := recw.WriteRecord(rec); err != nil {
			return err
		}
	}

	if err := bw.Flush(); err != nil {
		return err
	}
	return f.Close()
}

func rootFSFiles(initProg string) []string {
	set := map[string]bool{}

	var add func(string)
	add = func(f string) {
		if f == "/" {
			return
		}
		if set[f] {
			return
		}
		fi, err := os.Lstat(f)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			log.Fatal(err)
		}
		set[f] = true
		add(filepath.Dir(f))
		if fi.IsDir() {
			return
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(f)
			if err != nil {
				log.Fatal(err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(f), target)
			}
			add(target)
			return
		}
		out, _ := exec.Command("ldd", f).Output()
		for _, f := range strings.Fields(string(out)) {
			if strings.HasPrefix(f, "/") {
				add(f)
			}
		}
	}

	add(initProg)

	// libc-bin:
	add("/etc/ld.so.conf")
	filepath.Walk("/etc/ld.so.conf.d", func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			log.Fatal(err)
		}
		add(path)
		return nil
	})

	add("/sbin/sfdisk")    // util-linux
	add("/bin/lsblk")      // util-linux
	add("/sbin/mke2fs")    // e2fsprogs
	add("/sbin/resize2fs") // e2fsprogs
	var files []string
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

var kernelCmdLineFields []string

func kernelParam(k string) string {
	for _, f := range kernelCmdLineFields {
		if strings.HasPrefix(f, k) &&
			len(f) > len(k)+1 &&
			f[len(k)] == '=' {
			return f[len(k)+1:]
		}
	}
	return ""
}
