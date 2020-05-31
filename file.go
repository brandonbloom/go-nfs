package nfs

import (
	"io"
	"os"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/vmware/go-nfs-client/nfs/xdr"
)

// FileAttribute holds metadata about a filesystem object
type FileAttribute struct {
	Type                FileType
	FileMode            uint32
	Nlink               uint32
	UID                 uint32
	GID                 uint32
	Filesize            uint64
	Used                uint64
	SpecData            [2]uint32
	FSID                uint64
	Fileid              uint64
	Atime, Mtime, Ctime FileTime
}

// FileType represents a NFS File Type
type FileType uint32

// Enumeration of NFS FileTypes
const (
	FileTypeRegular FileType = iota + 1
	FileTypeDirectory
	FileTypeBlock
	FileTypeCharacter
	FileTypeLink
	FileTypeSocket
	FileTypeFIFO
)

func (f FileType) String() string {
	switch f {
	case FileTypeRegular:
		return "Regular"
	case FileTypeDirectory:
		return "Directory"
	case FileTypeBlock:
		return "Block Device"
	case FileTypeCharacter:
		return "Character Device"
	case FileTypeLink:
		return "Symbolic Link"
	case FileTypeSocket:
		return "Socket"
	case FileTypeFIFO:
		return "FIFO"
	default:
		return "Unknown"
	}
}

// Mode provides the OS interpreted mode of the file attributes
func (f *FileAttribute) Mode() os.FileMode {
	return os.FileMode(f.FileMode)
}

// FileCacheAttribute is the subset of FileAttribute used by
// wcc_attr
type FileCacheAttribute struct {
	Filesize     uint64
	Mtime, Ctime FileTime
}

// AsCache provides the wcc view of the file attributes
func (f FileAttribute) AsCache() *FileCacheAttribute {
	wcc := FileCacheAttribute{
		Filesize: f.Filesize,
		Mtime:    f.Mtime,
		Ctime:    f.Ctime,
	}
	return &wcc
}

// ToFileAttribute creates an NFS fattr3 struct from an OS.FileInfo
func ToFileAttribute(info os.FileInfo) FileAttribute {
	f := FileAttribute{}

	m := info.Mode()
	f.FileMode = uint32(m)
	if info.IsDir() {
		f.Type = FileTypeDirectory
	} else if m&os.ModeSymlink != 0 {
		f.Type = FileTypeLink
	} else if m&os.ModeCharDevice != 0 {
		f.Type = FileTypeCharacter
		// TODO: set major/minor dev number
		//f.SpecData = 0,0
	} else if m&os.ModeDevice != 0 {
		f.Type = FileTypeBlock
		// TODO: set major/minor dev number
		//f.SpecData = 0,0
	} else if m&os.ModeSocket != 0 {
		f.Type = FileTypeSocket
	} else if m&os.ModeNamedPipe != 0 {
		f.Type = FileTypeFIFO
	} else {
		f.Type = FileTypeRegular
	}
	// The number of hard links to the file.
	f.Nlink = 1

	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		f.Nlink = uint32(s.Nlink)
		f.UID = s.Uid
		f.GID = s.Gid
	}

	f.Filesize = uint64(info.Size())
	f.Used = uint64(info.Size())
	f.Atime = ToNFSTime(info.ModTime())
	f.Mtime = f.Atime
	f.Ctime = f.Atime
	return f
}

// WritePostOpAttrs writes the `post_op_attr` representation of a files attributes
func WritePostOpAttrs(writer io.Writer, fs billy.Filesystem, path []string) {
	attrs, err := fs.Stat(fs.Join(path...))
	if err != nil {
		_ = xdr.Write(writer, uint32(0))
	}
	_ = xdr.Write(writer, uint32(1))
	_ = xdr.Write(writer, ToFileAttribute(attrs))
}

// SetFileAttributes represents a command to update some metadata
// about a file.
type SetFileAttributes struct {
	SetMode  *uint32
	SetUID   *uint32
	SetGID   *uint32
	SetSize  *uint64
	SetAtime *time.Time
	SetMtime *time.Time
}

// Apply uses a `Change` implementation to set defined attributes on a
// provided file.
func (s *SetFileAttributes) Apply(changer billy.Change, fs billy.Filesystem, file string) error {
	cur := func() *FileAttribute {
		curOS, err := fs.Lstat(file)
		if err != nil {
			return nil
		}
		curr := ToFileAttribute(curOS)
		return &curr
	}

	if s.SetMode != nil {
		mode := os.FileMode(*s.SetMode) & os.ModePerm
		if err := changer.Chmod(file, mode); err != nil {
			return err
		}
	}
	if s.SetUID != nil || s.SetGID != nil {
		curr := cur()
		euid := curr.UID
		if s.SetUID != nil {
			euid = *s.SetUID
		}
		egid := curr.GID
		if s.SetGID != nil {
			egid = *s.SetGID
		}
		if err := changer.Lchown(file, int(euid), int(egid)); err != nil {
			return err
		}
	}
	if s.SetSize != nil {
		if cur().Mode()&os.ModeSymlink != 0 {
			return &NFSStatusError{NFSStatusNotSupp}
		}
		fp, err := fs.Open(file)
		if err != nil {
			return err
		}
		if err := fp.Truncate(int64(*s.SetSize)); err != nil {
			return err
		}
		if err := fp.Close(); err != nil {
			return err
		}
	}

	if s.SetAtime != nil || s.SetMtime != nil {
		curr := cur()
		atime := curr.Atime.Native()
		if s.SetAtime != nil {
			atime = s.SetAtime
		}
		mtime := curr.Mtime.Native()
		if s.SetMtime != nil {
			mtime = s.SetMtime
		}
		if err := changer.Chtimes(file, *atime, *mtime); err != nil {
			return err
		}
	}
	return nil
}

// Mode returns a mode if specified or the provided default mode.
func (s *SetFileAttributes) Mode(def os.FileMode) os.FileMode {
	if s.SetMode != nil {
		return os.FileMode(*s.SetMode) & os.ModePerm
	}
	return def
}

// ReadSetFileAttributes reads an sattr3 xdr stream into a go struct.
func ReadSetFileAttributes(r io.Reader) (*SetFileAttributes, error) {
	attrs := SetFileAttributes{}
	hasMode, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if hasMode != 0 {
		mode, err := xdr.ReadUint32(r)
		if err != nil {
			return nil, err
		}
		attrs.SetMode = &mode
	}
	hasUID, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if hasUID != 0 {
		uid, err := xdr.ReadUint32(r)
		if err != nil {
			return nil, err
		}
		attrs.SetUID = &uid
	}
	hasGID, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if hasGID != 0 {
		gid, err := xdr.ReadUint32(r)
		if err != nil {
			return nil, err
		}
		attrs.SetGID = &gid
	}
	hasSize, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if hasSize != 0 {
		var size uint64
		attrs.SetSize = &size
		if err := xdr.Read(r, size); err != nil {
			return nil, err
		}
	}
	aTime, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if aTime == 1 {
		now := time.Now()
		attrs.SetAtime = &now
	} else if aTime == 2 {
		t := FileTime{}
		if err := xdr.Read(r, t); err != nil {
			return nil, err
		}
		attrs.SetAtime = t.Native()
	}
	mTime, err := xdr.ReadUint32(r)
	if err != nil {
		return nil, err
	}
	if mTime == 1 {
		now := time.Now()
		attrs.SetMtime = &now
	} else if mTime == 2 {
		t := FileTime{}
		if err := xdr.Read(r, t); err != nil {
			return nil, err
		}
		attrs.SetMtime = t.Native()
	}
	return &attrs, nil
}
