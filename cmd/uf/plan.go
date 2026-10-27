package main

// The planner moved to internal/imageplan so the browser core can build the
// same plan from the same bytes (cmd/wasm reads the image through
// rkfw.OpenReaderAt). This file only keeps the names the CLI and its tests use.

import (
	"github.com/SilenWang/fydetab-flash-web/internal/gpt"
	"github.com/SilenWang/fydetab-flash-web/internal/imageplan"
	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
	"github.com/SilenWang/fydetab-flash-web/internal/rkfw"
)

const (
	// maxParameterBytes bounds the parameter item we are willing to load.
	// Real parameter partitions are 64 KiB or less.
	maxParameterBytes = imageplan.MaxParameterBytes
	// idbSector is where Rockchip's loader expects the IDB copy on flash.
	idbSector = imageplan.IDBSector
	// Partition-table geometry: the primary region covers LBA 0..33, the
	// backup region the last 33 sectors of the flash.
	GPTPrimarySectors = gpt.PrimarySectors
	GPTBackupSectors  = gpt.BackupSectors
	GPTPrimaryBytes   = imageplan.GPTPrimaryBytes
	GPTBackupBytes    = imageplan.GPTBackupBytes
)

// Item actions in the plan.
const (
	actionWrite     = imageplan.ActionWrite
	actionReserved  = imageplan.ActionReserved
	actionNoTarget  = imageplan.ActionNoTarget
	actionEmpty     = imageplan.ActionEmpty
	actionParameter = imageplan.ActionParameter
)

type (
	// Options are the knobs of the dry-run plan.
	Options = imageplan.Options
	// VerificationInfo carries the checksum results for the report.
	VerificationInfo = imageplan.VerificationInfo
	// ContainerInfo summarises the parsed container.
	ContainerInfo = imageplan.ContainerInfo
	// ParameterInfo summarises the parameter partition.
	ParameterInfo = imageplan.ParameterInfo
	// Stage is one phase of the projected uf run.
	Stage = imageplan.Stage
	// ItemPlan is one RKAF item mapped onto the flash.
	ItemPlan = imageplan.ItemPlan
	// PartitionPlan is one GPT partition with its payload coverage.
	PartitionPlan = imageplan.PartitionPlan
	// UncoveredRange is the tail of a partition that the image does not fill.
	UncoveredRange = imageplan.UncoveredRange
	// Report is the complete dry-run plan.
	Report = imageplan.Report
	// EraseDecision records what a run would do with uncovered tails.
	EraseDecision = imageplan.EraseDecision
	// EmbeddedLoader is the loader a Download Boot would send.
	EmbeddedLoader = imageplan.EmbeddedLoader
)

// BuildReport maps image items onto parameter partitions and projects the uf
// execution phases. It performs no I/O.
func BuildReport(img *rkfw.Image, doc *parameter.Document, ver VerificationInfo, opts Options) (*Report, error) {
	return imageplan.BuildReport(img, doc, ver, opts)
}
