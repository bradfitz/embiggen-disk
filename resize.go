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

// The resize-vm-disk command live resizes a filesystem and LVM objects
// and partition tables as needed. It's useful within a VM guest to make
// its filesystem bigger when the hypervisor live resizes the underlying
// block device.
package main

// TODO: test/fix on disks with non-512 byte sectors ( /sys/block/sda/queue/hw_sector_size)

import (
	"flag"
	"fmt"
	"log"
)

var (
	dry     = flag.Bool("dry-run", false, "don't make changes")
	verbose = flag.Bool("verbose", false, "verbose output")
)

func fatalf(format string, args ...interface{}) {
	log.SetFlags(0)
	log.Fatalf(format, args...)
}

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		fatalf("Usage: resize-vm-disk [flags] <mount_point_to_resize>")
	}

	mnt := flag.Arg(0)
	e, err := getFileSystemResizer(mnt)
	if err != nil {
		fatalf("error preparing to enlarge %s: %v", mnt, err)
	}
	changes, err := Resize(e)
	if len(changes) > 0 {
		fmt.Printf("Changes made:\n")
		for _, c := range changes {
			fmt.Printf("  * %s\n", c)
		}
	} else if err == nil {
		fmt.Printf("No changes made.\n")
	}
	if err != nil {
		fatalf("enlarging %s: %v", mnt, err)
	}
}

// An Resizer is anything that can enlarge something and describe its state.
// An Resizer can depend on another Resizer to run first.
type Resizer interface {
	String() string                       // "ext4 filesystem at /", "LVM PV foo"
	State() (string, error)               // "534 blocks"
	Resize() error                        // both may be non-zero
	DepResizer() (dep Resizer, err error) // can return (nil, nil) for none
}

// Resize resizes e's dependencies and then resizes e.
func Resize(e Resizer) (changes []string, err error) {
	s0, err := e.State()
	if err != nil {
		return
	}
	dep, err := e.DepResizer()
	if err != nil {
		return
	}
	if dep != nil {
		changes, err = Resize(dep)
		if err != nil {
			return
		}
	}
	err = e.Resize()
	if err != nil {
		return
	}
	s1, err := e.State()
	if err != nil {
		err = fmt.Errorf("error after successful resize of %v: %v", e, err)
		return
	}
	if s0 != s1 {
		changes = append(changes, fmt.Sprintf("%v: before: %v, after: %v", e, s0, s1))
	}
	return
}
