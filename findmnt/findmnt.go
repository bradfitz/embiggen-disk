//go:generate ffjson $GOFILE

package findmnt

// Output is the output from findmnt.
type Output struct {
	// Filesystems is the list of found filesystems.
	Filesystems []Filesystem `json:"filesystems"`
}

// Filesystem is the filesystem output from findmnt.
type Filesystem struct {
	// Fstype is the filesystem type.
	Fstype string `json:"fstype"`
	// Source is the filesystem source path.
	Source string `json:"source"`
}
