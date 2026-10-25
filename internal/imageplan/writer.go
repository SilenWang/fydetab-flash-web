package imageplan

import (
	"fmt"

	"github.com/SilenWang/rk3588-webflasher/internal/uf"
)

// WriterPlan projects the dry-run report onto the writer's plan. This is the
// only bridge between planning and writing: item order, target LBAs, the IDB
// byte count and the uncovered tails all come from BuildReport, so
// `uf --dry-run` and a real write cannot disagree about what is written.
//
// loader selects the blob Download Boot will push. It is normally the loader
// the image itself carries (see FromImage); a caller that lets the user pick a
// different file passes it explicitly and must say so, because a mismatched
// loader is exactly what real hardware rejected at index 0.
func WriterPlan(rep *Report, loader *EmbeddedLoader) (*uf.Plan, error) {
	plan := &uf.Plan{
		Image:      rep.Image,
		Chip:       rep.Container.Chip,
		NoReset:    rep.NoReset,
		TotalBytes: rep.TotalBytes,
	}
	if loader != nil && loader.Length > 0 {
		source := loader.Source
		if source == "" {
			source = LoaderSourceImage
		}
		plan.Loader = &uf.LoaderPlan{
			Source:     source,
			Name:       loader.Name,
			FileOffset: loader.FileOffset,
			Length:     loader.Length,
			SHA256:     loader.SHA256,
			Chip:       loader.Chip,
			BuildTime:  loader.BuildTime,
			Version:    loader.Version,
			Bytes:      loader.Bytes,
			NonImage:   source != LoaderSourceImage,
		}
	}
	for _, st := range rep.Stages {
		if st.Phase == "gpt" {
			plan.IDB = uf.IDBPlan{LBASector: uf.IDBDefaultLBA, Bytes: st.Bytes, Rewrite: rep.IDBRewrite}
			break
		}
	}
	for _, it := range rep.Items {
		if it.Action != ActionWrite || !it.TargetKnown {
			continue
		}
		plan.Items = append(plan.Items, uf.Item{
			Name:       it.Name,
			Partition:  it.Partition,
			FileOffset: it.DataOffset,
			Size:       it.Size,
			PaddedSize: it.PaddedSize,
			TargetLBA:  uint32(it.TargetLBA),
		})
	}
	for _, u := range rep.Uncovered {
		plan.Tails = append(plan.Tails, uf.Tail{
			Partition: u.Partition,
			FirstLBA:  u.FirstLBA,
			Bytes:     u.Bytes,
		})
	}
	if rep.GPT != nil {
		plan.DiskSectors = rep.GPT.DiskSectors
		plan.DiskKnown = rep.GPT.DiskKnown
		plan.DiskDerived = rep.GPT.DiskDerived
	}
	return plan, plan.Validate()
}

// ApplyEraseDecision makes the "erase or not" choice visible in the dry-run
// report itself, instead of leaving it to prose in the notes. The browser shows
// the same sentence before a write, which is how "the 4 GiB STATE tail was
// handled explicitly" becomes a checklist item rather than an assumption.
func ApplyEraseDecision(rep *Report, mode uf.EraseMode) {
	dec := &EraseDecision{Mode: string(mode), Detail: mode.Describe(), Tails: len(rep.Uncovered)}
	for _, u := range rep.Uncovered {
		dec.Bytes += u.Bytes
	}
	rep.Erase = dec
	rep.Notes = append(rep.Notes, fmt.Sprintf("- 擦除语义（--erase-tail=%s）：%s；未覆盖区间共 %s。", mode, mode.Describe(), HumanBytes(dec.Bytes)))
}
