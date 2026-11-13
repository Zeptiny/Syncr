package localfs

import (
	"os"
	"syscall"
)

// Magic numbers of Linux pseudo filesystems (linux/magic.h, statfs(2)):
// they hold kernel state, not files on a disk, and some report absurd
// sizes (/proc/kcore is as large as the address space).
const (
	magicProc       = 0x9fa0
	magicSysfs      = 0x62656572
	magicDevpts     = 0x1cd1
	magicCgroup     = 0x27e0eb
	magicCgroup2    = 0x63677270
	magicDebugfs    = 0x64626720
	magicTracefs    = 0x74726163
	magicSecurityfs = 0x73636673
	magicSelinuxfs  = 0xf97cff8c
	magicPstore     = 0x6165676c
	magicBPF        = 0xcafe4a11
	magicConfigfs   = 0x62656570
	magicEfivarfs   = 0xde5e81e4
	magicMqueue     = 0x19800202
	magicBinfmt     = 0x42494e4d
	magicFusectl    = 0x65735543
	magicNsfs       = 0x6e736673
	magicHugetlbfs  = 0x958458f6
)

// isPseudoType reports whether a statfs filesystem type is a pseudo
// filesystem's.
func isPseudoType(t int64) bool {
	switch t {
	case magicProc, magicSysfs, magicDevpts, magicCgroup, magicCgroup2, magicDebugfs, magicTracefs,
		magicSecurityfs, magicSelinuxfs, magicPstore, magicBPF, magicConfigfs, magicEfivarfs,
		magicMqueue, magicBinfmt, magicFusectl, magicNsfs, magicHugetlbfs:
		return true
	}
	return false
}

// statfsType widens statfs's f_type, whose Go type differs between
// architectures.
func statfsType[T ~int32 | ~int64 | ~uint32](t T) int64 { return int64(t) }

// pseudoFS reports whether the opened folder d is on a Linux pseudo
// filesystem (proc, sysfs, cgroup and the like), by the filesystem's type
// (fstatfs), wherever it is mounted. A folder whose type cannot be read is
// not one.
func pseudoFS(d dirReader) bool {
	f, ok := d.(*os.File)
	if !ok {
		return false
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var st syscall.Statfs_t
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.Fstatfs(int(fd), &st) }); err != nil || serr != nil {
		return false
	}
	return isPseudoType(statfsType(st.Type))
}
