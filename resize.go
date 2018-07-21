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

// The resize-vm-disk command resizes the final partition of a disk to
// match the newly enlarged size, growing the partition table, LVM,
// and filesystem as necessary. It handles MBR and GPT partition tables.
package main

import (
	"bytes"
	"flag"
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
)

const (
	lvmGPTTypeID       = "E6D6D379-F507-44C2-A23C-238F2A3DF928"
	rootx8664GPTTypeID = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
)

var dev = flag.String("dev", "", "device to enlarge; defaults to the only applicable disk if it's not ambiguous")

func main() {
	flag.Parse()
	if *dev == "" {
		names := devNames()
		if len(names) == 0 {
			log.Fatalf("no devices found")
		}
		if len(names) > 1 {
			log.Fatalf("No --dev value provided and it's ambiguous which you want expanded: %q", names)
		}
		*dev = names[0]
	}
	if !strings.Contains(*dev, "/") {
		*dev = "/dev/" + *dev
	}
	fmt.Printf("getting partition table for %q ...\n", *dev)
	pt := getPartitionTable(*dev)
	if len(pt.parts) == 0 {
		log.Fatalf("device %q has no partitions", *dev)
	}
	var isGPT bool
	switch t := pt.Meta("label"); t {
	case "dos":
	case "gpt":
		isGPT = true
	default:
		// It might work, but fail as a precaution. Untested.
		log.Fatalf("unsupported partition table type %q", t)
	}

	part := pt.parts[len(pt.parts)-1]
	lastType := part.Type()

	var needPVExtend bool
	if isGPT {
		switch lastType {
		case lvmGPTTypeID:
			fmt.Println("final partition is LVM PV")
			needPVExtend = true
		case rootx8664GPTTypeID:
		default:
			log.Fatalf("unknown GPT partition type %q for %s", lastType, part.dev)
		}
	} else {
		switch lastType {
		case "83":
		default:
			log.Fatalf("unknown MBR partition type %q for %s", lastType, part.dev)
		}
	}

	fmt.Printf("Partition table:\n")
	pt.Write(os.Stdout)
	fmt.Println()

	size := readInt64File("/sys/block/sda/size")
	fmt.Printf("Cur size: %d\n", size)

	fmt.Printf("Part start: %d\n", part.Start())
	fmt.Printf("Part size: %d\n", part.Size())
	end := part.Start() + part.Size()
	fmt.Printf("Part end: %d\n", end)
	remain := size - end
	fmt.Printf("Remaining after final partition: %d\n", remain)
	if remain <= 2048 {
		fmt.Println("partition is at max size")
		return
	}
	extend := size - end - 2048
	fmt.Printf("Need to extend disk by %d sectors (%d bytes, %0.03f GiB)\n", extend, extend*512, float64(extend)*512/(1<<30))
	if needPVExtend {
		fmt.Println("needs PV extend")
	}
}

func readInt64File(f string) int64 {
	x, err := ioutil.ReadFile(f)
	if err != nil {
		log.Fatal(err)
	}
	x = bytes.TrimSpace(x)
	n, err := strconv.ParseInt(string(x), 10, 64)
	if err != nil {
		log.Fatal(err)
	}
	return n
}

/*
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

type sfdiskLine struct {
	dev  string   // "/dev/sda1"
	attr []string // key=value or key ("type=83", "bootable", "size=497664")
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

func (sl sfdiskLine) Type() string { return sl.Attr("type") }
func (sl sfdiskLine) Start() int64 { return sl.AttrInt64("start") }
func (sl sfdiskLine) Size() int64  { return sl.AttrInt64("size") }

func getPartitionTable(dev string) *partitionTable {
	pt := new(partitionTable)
	out, err := exec.Command("/sbin/sfdisk", "-d", dev).Output()
	if err != nil {
		log.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
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
			part := sfdiskLine{dev: dev}
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

func devNames() (names []string) {
	fis, err := ioutil.ReadDir("/sys/block")
	if err != nil {
		log.Fatal(err)
	}
	for _, fi := range fis {
		name := filepath.Base(fi.Name())
		if name == "sr0" || strings.HasPrefix(name, "dm-") {
			continue
		}
		names = append(names, name)
	}
	return names
}

/*
[906416.656697] sda: detected capacity change from 161061273600 to 171798691840

root@kbase:~# lsblk
NAME                MAJ:MIN RM   SIZE RO TYPE MOUNTPOINT
sda                   8:0    0   160G  0 disk
├─sda1                8:1    0   243M  0 part /boot
├─sda2                8:2    0     1K  0 part
└─sda5                8:5    0 149.8G  0 part
  └─debian--vg-root 254:0    0 149.8G  0 lvm  /

root@kbase:~# lsblk -b
NAME                MAJ:MIN RM         SIZE RO TYPE MOUNTPOINT
sda                   8:0    0 171798691840  0 disk
├─sda1                8:1    0    254803968  0 part /boot
├─sda2                8:2    0         1024  0 part
└─sda5                8:5    0 160803323904  0 part
  └─debian--vg-root 254:0    0 160801226752  0 lvm  /


root@debian:~# sfdisk -d /dev/sda
label: gpt
label-id: 86CA60D6-FE21-41E7-96AB-42BB6372BA35
device: /dev/sda
unit: sectors
first-lba: 34
last-lba: 10485726

/dev/sda1 : start=        2048, size=     1048576, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, uuid=C9C1EF10-FE31-4B94-BDBC-C2750F8FE621
/dev/sda2 : start=     1050624, size=      499712, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=1C0A64F2-A6CA-4798-92BE-6F37C5E1C55B
/dev/sda3 : start=     1550336, size=     8933376, type=E6D6D379-F507-44C2-A23C-238F2A3DF928, uuid=183184BF-371C-4406-8B06-9B872F464487
root@debian:~# cat /sys/block/sda/size
10485760

>>> 1550336+8933376
10483712

>>> 10485760-10483712
2048



root@kbase:~# sfdisk -d /dev/sda
label: dos
label-id: 0x877f0a6b
device: /dev/sda
unit: sectors

/dev/sda1 : start=        2048, size=      497664, type=83, bootable
/dev/sda2 : start=      501758, size=   314068994, type=5
/dev/sda5 : start=      501760, size=   314068992, type=8e

Deps:
util-linux: /sbin/sfdisk
parted:

*/
