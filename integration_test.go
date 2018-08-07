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
	"sort"
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

func init() {
	if os.Getpid() != 1 {
		// Not in qemu.
		return
	}

	fmt.Println(":: hello from userspace")

	// Be verbose in the qemu guest. We'll filter it out in the parent.
	flag.Lookup("test.v").Value.Set("true")

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
}

var inQemu = os.Getpid() == 1

func TestMain(m *testing.M) {
	status := m.Run()
	if inQemu {
		fmt.Printf(":: exit=%d\n", status)
	}
	os.Exit(status)
}

var qemuTests []string
var qemuTest = map[string]func(*testing.T){}

func addQemuTest(name string, fn func(*testing.T)) {
	if _, dup := qemuTest[name]; dup {
		panic(fmt.Sprintf("duplicate qemu test %q", name))
	}
	qemuTests = append(qemuTests, name)
	qemuTest[name] = fn
}

func TestInQemu(t *testing.T) {
	if inQemu {
		for _, name := range qemuTests {
			t.Run(name, qemuTest[name])
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
		"-append", "console=ttyS0,115200 panic=-1 acpi=off nosmp ip=dhcp parentTempDir="+td)
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

var monClientCache *monClient

func dialMon(t *testing.T) *monClient {
	if mc := monClientCache; mc != nil {
		return mc
	}
	if os.Getpid() != 1 {
		panic("dialMon is meant for tests in qemu")
	}
	c, err := net.Dial("tcp", "10.0.2.100:1234")
	if err != nil {
		t.Fatal(err)
	}
	mc := &monClient{c: c}
	if _, err := mc.readToPrompt(); err != nil {
		t.Fatalf("readToPrompt: %v", err)
	}
	monClientCache = mc
	return mc
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
	tempDir := parentTempDir(t)
	out, err := mc.run("drive_add 0 file=" + filepath.Join(tempDir, diskBase+".qcow2") + ",if=none,id=" + diskBase)
	if err != nil {
		t.Fatalf("drive_add %q: %v", diskBase, err)
	}
	if out != "OK" {
		t.Fatalf("drive_add %q: %s", diskBase, out)
	}

	out, err = mc.run("device_add scsi-hd,drive=" + diskBase + ",id=" + diskBase)
	if err != nil {
		t.Fatalf("device_add %q: %v", diskBase, err)
	}
	if len(out) > 0 {
		t.Logf("device_add %q: %s", diskBase, out)
	}
}

func (mc *monClient) removeDisk(t *testing.T, diskBase string) {
	out, err := mc.run("device_del " + diskBase)
	if err != nil {
		t.Fatalf("device_del %q: %v", diskBase, err)
	}
	if len(out) > 0 {
		t.Logf("device_del %q: %s", diskBase, out)
	}
}

func init() {
	addQemuTest("Mon", testMon)
}
func testMon(t *testing.T) {
	mc := dialMon(t)
	if out, err := mc.run("info block"); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("info block: %q", out)
	}
}

func init() {
	addQemuTest("Mke2fs", testMke2fs)
}
func testMke2fs(t *testing.T) {
	mc := dialMon(t)
	mc.addDisk(t, "foo")
	defer mc.removeDisk(t, "foo")
	out, err := exec.Command("/bin/lsblk").CombinedOutput()
	if err != nil {
		t.Fatalf("lsblk error: %v, %s", err, out)
	}
	if len(out) == 0 {
		t.Errorf("empty lsblk output")
	}
	t.Logf("lsblk: %s", out)
}

func init() {
	addQemuTest("Lsblk", testLsblk)
}
func testLsblk(t *testing.T) {
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

var parentTempDirCached string

func parentTempDir(t *testing.T) string {
	if v := parentTempDirCached; v != "" {
		return v
	}
	all, err := ioutil.ReadFile("/proc/cmdline")
	if err != nil {
		t.Fatalf("looking up parentTempDir: %v", err)
	}
	fs := strings.Fields(string(all))
	const pfx = "parentTempDir="
	for _, f := range fs {
		if strings.HasPrefix(f, pfx) {
			v := f[len(pfx):]
			parentTempDirCached = v
			return v
		}
	}
	t.Fatal("failed to find parentTempDir kernel command line")
	return ""
}
