package imageplan

// The parameter partition in the write plan.
//
// The parameter block is not written to a partition from mtdparts: the host
// writes it into a reserved area at the start of the disk (replicated), and the
// device's loader turns it into the GUID partition table. It therefore belongs
// to the gpt stage, next to the partition table it defines, and never becomes a
// `wl` item.

import (
	"fmt"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
)

// buildParameterPlan locates the PARM block in the image and lays out its
// copies. It returns nil when the image has no parameter entry, and an error
// when the entry does not carry what the placement needs: a guessed placement
// would write the partition table somewhere the device does not read.
func buildParameterPlan(img *rkfw.Image, doc *parameter.Document, opts Options) (*ParameterPlan, error) {
	it := img.Parameter
	if it == nil {
		return nil, nil
	}
	size := int64(it.Size64)
	if size <= 0 {
		size = int64(it.Size)
	}
	if size <= 0 {
		return nil, fmt.Errorf("the parameter entry is empty; cannot plan the parameter copies")
	}
	copyBytes := int64(it.PaddedSize)
	if copyBytes < size {
		copyBytes = size
	}
	copyBytes = alignUp(copyBytes, parameter.SectorSize)

	replicas, err := parameter.ReplicaLayout(it.NandAddr, it.NandSize, !opts.NoFirst4Access, copyBytes)
	if err != nil {
		return nil, fmt.Errorf("parameter entry (flash offset 0x%x, reserved area %d sectors): %w",
			it.NandAddr, it.NandSize, err)
	}
	return &ParameterPlan{
		Item:         it.Name,
		FileOffset:   img.ItemOffset(*it),
		Bytes:        size,
		CopyBytes:    copyBytes,
		Copies:       len(replicas),
		First4Access: !opts.NoFirst4Access,
		Replicas:     replicas,
		CRCOK:        doc != nil && doc.CRCOK,
	}, nil
}
