package fsstat

const (
	BlockSize         uint32 = 4096
	NameLen           uint32 = 255
	LogicalTotalBytes uint64 = 1 << 60
	LogicalFreeFiles  uint64 = 1 << 48
)

// Snapshot is a filesystem statistics view suitable for statfs responses.
type Snapshot struct {
	Blocks  uint64
	Bfree   uint64
	Bavail  uint64
	Files   uint64
	Ffree   uint64
	Bsize   uint32
	Frsize  uint32
	NameLen uint32
}

// FromUsage converts logical usage counters into a statfs snapshot.
func FromUsage(usedBytes, totalFiles uint64) Snapshot {
	if usedBytes > LogicalTotalBytes {
		usedBytes = LogicalTotalBytes
	}

	freeBytes := LogicalTotalBytes - usedBytes

	return Snapshot{
		Blocks:  ceilDiv(LogicalTotalBytes, uint64(BlockSize)),
		Bfree:   ceilDiv(freeBytes, uint64(BlockSize)),
		Bavail:  ceilDiv(freeBytes, uint64(BlockSize)),
		Files:   totalFiles + LogicalFreeFiles,
		Ffree:   LogicalFreeFiles,
		Bsize:   BlockSize,
		Frsize:  BlockSize,
		NameLen: NameLen,
	}
}

func ceilDiv(value, unit uint64) uint64 {
	if value == 0 {
		return 0
	}
	return (value + unit - 1) / unit
}
