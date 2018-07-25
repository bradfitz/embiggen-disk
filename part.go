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
)

const (
	lvmGPTTypeID       = "E6D6D379-F507-44C2-A23C-238F2A3DF928"
	rootx8664GPTTypeID = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
)

type partitionResizer string // "/dev/sda3"

// diskDev maps "/dev/sda3" to "/dev/sda".
func diskDev(partDev string) string {
	if !strings.HasPrefix(partDev, "/dev/") {
		panic("bogus partition dev " + partDev)
	}
	if !strings.HasPrefix(partDev, "/dev/sd") {
		panic("TODO: handle other device types; ask kernel")
	}
	return strings.TrimRight(partDev, "0123456789")
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
	partDev := string(p)
	diskDev := diskDev(partDev)
	pt := getPartitionTable(diskDev)
	if len(pt.parts) == 0 {
		log.Fatalf("device %q has no partitions", diskDev)
	}
	var isGPT bool
	switch t := pt.Meta("label"); t {
	case "dos":
	case "gpt":
		isGPT = true
	default:
		// It might work, but fail as a precaution. Untested.
		return fmt.Errorf("unsupported partition table type %q on %s", t, diskDev)
	}

	part := pt.parts[len(pt.parts)-1]
	partDev = part.dev
	lastType := part.Type()

	if isGPT {
		switch lastType {
		case lvmGPTTypeID, rootx8664GPTTypeID:
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

	size, err := readInt64File("/sys/block/sda/size")
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
	const (
		IOCTL_BLKPG            = 0x1269 // #define BLKPG _IO(0x12,105)
		BLKPG_RESIZE_PARTITION = 3      // from linux/include/uapi/linux/blkpg.h
	)
	type blkg_partition struct {
		start         int64
		length        int64
		pno           int32 // partition number
		_unused_names [128]byte
	}
	type blkpg_ioctl_arg struct {
		op      int32
		flags   int32
		datalen int32
		part    *blkg_partition
	}
	var arg = &blkpg_ioctl_arg{
		op: BLKPG_RESIZE_PARTITION,
		part: &blkg_partition{
			start:  part.Start() * 512,
			length: part.Size() * 512,
			pno:    int32(part.pno),
		},
	}
	// TODO: remove these once all this in x/sys/unix:
	if g, w := unsafe.Sizeof(blkpg_ioctl_arg{}), 24; g != uintptr(w) {
		return fmt.Errorf("unsafe.Sizeof(blkg_ioctl_arg) = %v; want C's %v", g, w)
	}
	if g, w := unsafe.Sizeof(blkg_partition{}), 152; g != uintptr(w) {
		return fmt.Errorf("unsafe.Sizeof(blkg_partition) = %v; want C's %v", g, w)
	}

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(devf.Fd()), IOCTL_BLKPG, uintptr(unsafe.Pointer(arg))); e != 0 {
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
