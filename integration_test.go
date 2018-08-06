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
	"encoding/json"
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

func TestInQemu(t *testing.T) {
	if inQemu {
		t.Skipf("already in qemu")
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

	qmpSockPath := filepath.Join(td, "qmpsock")

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
			mon, err := net.Dial("unix", qmpSockPath)
			if err != nil {
				c.Close()
				log.Printf("dial unix to monitor failed: %v", err)
				return
			}
			go func() {
				defer c.Close()
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
			}()
		}
	}()
	cmd := exec.Command("qemu-system-x86_64",
		"-vga", "none",
		"-nographic",
		"-m", "256",
		"-display", "none",
		"-qmp", "unix:"+qmpSockPath+",server,nowait",
		"-device", "virtio-net,netdev=net0",
		"-netdev", "user,id=net0,guestfwd=tcp:10.0.2.100:1234-tcp:"+ln.Addr().String(),
		"-device", "virtio-serial",
		"-device", "virtio-scsi-pci,id=scsi",
		"-kernel", kernelPath,
		"-initrd", initrdPath,
		"-no-reboot",
		"-append", "console=ttyS0,115200 panic=-1 ip=dhcp")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	t.Logf("Run = %v", err)
}

// test hitting the QMP server from the guest
func TestQMP(t *testing.T) {
	if !inQemu {
		t.Skipf("not in VM")
	}
	c, err := net.Dial("tcp", "10.0.2.100:1234")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	jd := json.NewDecoder(c)
	var msg map[string]interface{}
	if err := jd.Decode(&msg); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("Got: %#v\n", msg)

	io.WriteString(c, `{ "execute": "qmp_capabilities" }`)
	msg = nil
	if err := jd.Decode(&msg); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("Got2: %#v\n", msg)

	io.WriteString(c, `{ "execute": "query-block" }`)
	msg = nil
	if err := jd.Decode(&msg); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("Got3: %#v\n", msg)

	//io.WriteString(c, `{ "execute": "quit" }`)

}

// test running lsblk in the guest
func TestLsblk(t *testing.T) {
	if !inQemu {
		t.Skipf("not in VM")
	}
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
