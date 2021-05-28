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
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	lvmGPTTypeID       = "E6D6D379-F507-44C2-A23C-238F2A3DF928"
	rootx8664GPTTypeID = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
	linuxGPTTypeID     = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
)

type partitionResizer string // "/dev/sda3"

// diskDev maps "/dev/sda3" to "/dev/sda".
func diskDev(partDev string) string {
	if !strings.HasPrefix(partDev, "/dev/") {
		panic("bogus partition dev " + partDev)
	}
	if strings.HasPrefix(partDev, "/dev/sd") {
		return strings.TrimRight(partDev, "0123456789")
	}
	if strings.HasPrefix(partDev, "/dev/mmcblk") {
		v := strings.TrimRight(partDev, "0123456789")
		v = strings.TrimSuffix(v, "p")
		return v
	}
	if strings.HasPrefix(partDev, "/dev/nvme") {
		chopP := regexp.MustCompile(`p\d+$`)
		if !chopP.MatchString(partDev) {
			panic(fmt.Sprintf("partition %q doesn't look like an nvme partition", partDev))
		}
		return chopP.ReplaceAllString(partDev, "")
	}
	panic(fmt.Sprintf("Unsupport device %q; TODO: handle other device types; ask kernel", partDev))
}

func (p partitionResizer) String() string { return fmt.Sprintf("partition %s", string(p)) }

func (p partitionResizer) State() (string, error) {
	n, err := readInt64File(fmt.Sprintf("/sys/class/block/%s/size", filepath.Base(string(p))))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d sectors", n), nil
}

func (p partitionResizer) DepResizer() (Resizer, error) { return nil, nil }

func (p partitionResizer) Resize() error {
	vlogf("Resizing partition %q ...", string(p))
	partDev := string(p)
	diskDev := diskDev(partDev)
	vlogf("Getting partition table for %q ...", diskDev)
	pt := getPartitionTable(diskDev)
	if len(pt.parts) == 0 {
		log.Fatalf("device %q has no partitions", diskDev)
	}
	vlogf("Device %q has %d partitions.", diskDev, len(pt.parts))
	var isGPT bool
	switch t := pt.Meta("label"); t {
	case "dos":
	case "gpt":
		isGPT = true
	case "":
		// Old version of sfdisk? See https://github.com/google/embiggen-disk/issues/6
		// Use blkid to figure out what it is.
		// But only trust the value "dos", because if it's gpt and sfdisk
		// is old and doesn't support gpt, we don't want to use that old sfdisk
		// to manipulate the gpt tables.
		out, err := exec.Command("blkid", "-o", "export", diskDev).Output()
		if err != nil {
			return fmt.Errorf("error running blkid: %v", execErrDetail(err))
		}
		m := regexp.MustCompile(`(?m)^PTTYPE=(.+)\n`).FindSubmatch(out)
		if m == nil {
			return fmt.Errorf("`blkid -o export %s` lacked PTTYPE line, got: %s", diskDev, out)
		}
		if got := string(m[1]); got != "dos" {
			return fmt.Errorf("Old sfdisk and `blkid -o export %s` reports unexpected PTTYPE=%s", diskDev, got)
		}
	default:
		// It might work, but fail as a precaution. Untested.
		return fmt.Errorf("unsupported partition table type %q on %s", t, diskDev)
	}

	part, ok := pt.lastNonZeroPartition()
	if !ok {
		return fmt.Errorf("no non-zero partition found on %s", diskDev)
	}
	partDev = part.dev
	lastType := part.Type()

	if isGPT {
		switch lastType {
		case lvmGPTTypeID, rootx8664GPTTypeID, linuxGPTTypeID:
		default:
			return fmt.Errorf("unknown GPT partition type %q for %s", lastType, part.dev)
		}
	} else {
		switch lastType {
		case "83":
		default:
			return fmt.Errorf("unknown MBR partition type %q for %s", lastType, part.dev)
		}
	}

	if *verbose {
		fmt.Printf("Current partition table:\n")
		pt.Write(os.Stdout)
		fmt.Println()
	}

	size, err := readInt64File("/sys/block/" + filepath.Base(diskDev) + "/size")
	if err != nil {
		return err
	}
	end := part.Start() + part.Size()
	remain := size - end
	if *verbose {
		fmt.Printf("Cur size: %d\n", size)
		fmt.Printf("Part start: %d\n", part.Start())
		fmt.Printf("Part size: %d\n", part.Size())
		fmt.Printf("Part end: %d\n", end)
		fmt.Printf("Remaining after final partition: %d\n", remain)
	}
	sectorSize := 512 // TODO: get from /sys/block/sda/queue/hw_sector_size
	endReserve := int64(1<<20) / int64(sectorSize)
	if remain <= endReserve {
		// partition at max size; no need to extend
		return nil
	}

	extend := remain - endReserve
	part.SetSize(part.Size() + extend)
	pt.RemoveMeta("last-lba") // or sfdisk complains

	if *verbose {
		fmt.Printf("Need to extend disk by %d sectors (%d bytes, %0.03f GiB)\n", extend, extend*512, float64(extend)*512/(1<<30))
		fmt.Printf("New partition table to write:\n")
	}

	var newPart bytes.Buffer
	pt.Write(&newPart)
	if *verbose {
		fmt.Printf("%s\n", newPart.Bytes())
	}

	if *dry {
		fmt.Printf("[dry-run] would've run sfdisk -f to set new partition table\n")
		return nil
	}

	if *verbose {
		fmt.Println("Setting new partition table...")
	}
	cmd := exec.Command("/sbin/sfdisk", "-f", "--no-reread", "--no-tell-kernel", diskDev)
	cmd.Stdin = bytes.NewReader(newPart.Bytes())
	var outBuf bytes.Buffer
	if *verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = &outBuf
		cmd.Stderr = &outBuf
	}
	if err := cmd.Run(); err != nil {
		log.Fatalf("sfdisk: %v: %s", err, outBuf.Bytes())
	}

	// Tell the kernel.
	if err := updateKernelPartition(diskDev, part); err != nil {
		return fmt.Errorf("updating kernel of %s partition change: %v", partDev, err)
	}
	return nil
}

func updateKernelPartition(diskDev string, part sfdiskLine) error {
	devf, err := os.Open(diskDev)
	if err != nil {
		return err
	}
	defer devf.Close()
	arg := &unix.BlkpgIoctlArg{
		Op: unix.BLKPG_RESIZE_PARTITION,
		Data: (*byte)(unsafe.Pointer(&unix.BlkpgPartition{
			Start:  part.Start() * 512,
			Length: part.Size() * 512,
			Pno:    int32(part.pno),
		})),
	}

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(devf.Fd()), unix.BLKPG, uintptr(unsafe.Pointer(arg))); e != 0 {
		return syscall.Errno(e)
	}
	return nil
}

type partitionTable struct {
	meta  []string // without newlines
	parts []sfdiskLine
}

func (pt *partitionTable) Meta(k string) string {
	for _, row := range pt.meta {
		if strings.HasPrefix(row, k) &&
			strings.HasPrefix(row, k+":") {
			return strings.TrimSpace(row[len(k)+1:])
		}
	}
	return ""
}

func (pt *partitionTable) RemoveMeta(key string) {
	var newMeta []string
	for _, meta := range pt.meta {
		if strings.HasPrefix(meta, key) &&
			strings.HasPrefix(meta, key+": ") {
			continue
		}
		newMeta = append(newMeta, meta)
	}
	pt.meta = newMeta
}

func (pt *partitionTable) Write(w io.Writer) error {
	var buf bytes.Buffer
	for _, meta := range pt.meta {
		buf.WriteString(meta)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	for _, part := range pt.parts {
		buf.WriteString(part.String())
		buf.WriteByte('\n')
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func (pt *partitionTable) lastNonZeroPartition() (part sfdiskLine, ok bool) {
	for i := len(pt.parts) - 1; i >= 0; i-- {
		part = pt.parts[i]
		if part.Type() == "0" && part.Start() == 0 && part.Size() == 0 {
			// Skip useless partitions.
			// See https://github.com/google/embiggen-disk/issues/6#issuecomment-429055087
			continue
		}
		return part, true
	}
	return
}

type sfdiskLine struct {
	dev  string   // "/dev/sda1"
	attr []string // key=value or key ("type=83", "bootable", "size=497664")
	pno  int      //partition number
}

func (sl sfdiskLine) String() string {
	return fmt.Sprintf("%s : %s", sl.dev, strings.Join(sl.attr, ", "))
}

func (sl sfdiskLine) Attr(key string) string {
	for _, attr := range sl.attr {
		if key == attr {
			return key // Attr("bootable") == "bootable", not "true" or empty string
		}
		if strings.HasPrefix(attr, key) &&
			strings.HasPrefix(attr, key+"=") {
			return strings.TrimSpace(attr[len(key)+1:])
		}
	}
	return ""
}

func (sl sfdiskLine) SetSize(size int64) {
	for i, attr := range sl.attr {
		if strings.HasPrefix(attr, "size=") {
			sl.attr[i] = fmt.Sprintf("size=%d", size)
			return
		}
	}
	panic("didn't find size attribute")
}

func (sl sfdiskLine) AttrInt64(key string) int64 {
	v := sl.Attr(key)
	if v == "" {
		log.Fatalf("device %q has no attribute %q", sl.dev, key)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Fatalf("device %q attribute %q is non-integer: %q", sl.dev, key, v)
	}
	return n
}

func (sl sfdiskLine) Type() string {
	// sfdisk from util-linux 2.29.2 on Debian under Proxmox/Qemu reports type:
	v := sl.Attr("type")
	if v != "" {
		return v
	}
	// But sfdisk from util-linux 2.23.2 on CentOS 7.5 on Azure uses "Id":
	return sl.Attr("Id")
}

func (sl sfdiskLine) Start() int64 { return sl.AttrInt64("start") }
func (sl sfdiskLine) Size() int64  { return sl.AttrInt64("size") }

func getPartitionTable(dev string) *partitionTable {
	pt := new(partitionTable)
	out, err := exec.Command("/sbin/sfdisk", "-d", dev).Output()
	if err != nil {
		log.Fatalf("running sfdisk -f %s: %v, %s", dev, err, out)
	}
	lines := strings.Split(string(out), "\n")
	var pno int
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			if pt.parts == nil {
				pt.parts = make([]sfdiskLine, 0)
			}
			continue
		}
		if pt.parts == nil {
			pt.meta = append(pt.meta, line)
		} else {
			f := strings.SplitN(string(line), ":", 2)
			if len(f) < 2 {
				log.Fatalf("unsupported sfdisk line %q", line)
			}
			dev := strings.TrimSpace(f[0])
			rest := strings.TrimSpace(f[1])
			pno++
			part := sfdiskLine{dev: dev, pno: pno}
			for _, attr := range strings.Split(rest, ",") {
				attr = strings.TrimSpace(attr)
				attr = eqRx.ReplaceAllString(attr, "=")
				part.attr = append(part.attr, attr)
			}
			pt.parts = append(pt.parts, part)
		}
	}
	return pt
}

var eqRx = regexp.MustCompile(`\s*=\s*`)

func readInt64File(f string) (int64, error) {
	x, err := ioutil.ReadFile(f)
	if err != nil {
		return 0, err
	}
	x = bytes.TrimSpace(x)
	n, err := strconv.ParseInt(string(x), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func devEndsInNumber(d string) bool {
	return len(d) > 0 && unicode.IsNumber(rune(d[len(d)-1]))
}

/*

Notes on sfdisk output:

can be GPT:

label: gpt
label-id: 841DBE6B-6A8D-43E1-93E1-D765373DDE3B
device: /dev/sda
unit: sectors
first-lba: 34
last-lba: 10485726

/dev/sda1 : start=        2048, size=      192512, type=21686148-6449-6E6F-744E-656564454649, uuid=D7F261B7-9D9A-4864-AB85-A68ED9CD7CF0
/dev/sda2 : start=      194560, size=      391168, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=B3EB025F-F682-4FE4-8F97-96974ADFD3BF
/dev/sda3 : start=      585728, size=     9897984, type=E6D6D379-F507-44C2-A23C-238F2A3DF928, uuid=654CE2C8-5871-4DBE-A829-F3C4D953BBB9

or MBR:

label: dos
label-id: 0xeba7536a
device: /dev/sda
unit: sectors

/dev/sda1 : start=        2048, size=      497664, type=83, bootable
/dev/sda2 : start=      501758, size=   209211394, type=5
/dev/sda5 : start=      501760, size=   209211392, type=83

*/
