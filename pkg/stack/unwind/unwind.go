// Copyright 2021 The Parca Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package unwind

import (
	"debug/elf"
	"errors"
	"fmt"
	"path"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/parca-dev/parca-agent/pkg/maps"
	"github.com/parca-dev/parca-agent/pkg/stack/frame"
)

type Unwinder struct {
	logger    log.Logger
	fileCache *maps.PidMappingFileCache
}

type Op int // TODO(kakkoyun): A better type?

// TODO(kakkoyun): Clean up comments.
const (
	// This type of register is not supported.
	OpUnimplemented Op = iota
	// Undefined register. The value will be defined at some later IP in the same DIE.
	OpUndefined
	// Value stored at some offset from `CFA`.
	OpCfaOffset
	// Value of a machine register plus offset.
	OpRegister
)

type Instruction struct {
	Op  Op
	Reg uint64
	Off int64
}

type PlanTableRow struct {
	Begin, End uint64
	RIP, RSP   Instruction
}

type PlanTable []PlanTableRow

func NewUnwinder(logger log.Logger, fileCache *maps.PidMappingFileCache) *Unwinder {
	return &Unwinder{logger: logger, fileCache: fileCache}
}

func (u *Unwinder) UnwindTableForPid(pid uint32) (PlanTable, error) {
	mappings, err := u.fileCache.MappingForPid(pid)
	if err != nil {
		return nil, err
	}

	for _, m := range mappings {
		// TODO(brancz): These need special cases.
		if m.File == "[vdso]" || m.File == "[vsyscall]" {
			continue
		}

		abs := path.Join(fmt.Sprintf("/proc/%d/root", pid), m.File)
		fdes, err := readFDEs(abs, m.Start)
		if err != nil {
			level.Warn(u.logger).Log("msg", "failed to read section", "obj", abs, "err", err)
			continue
		}

		// Return first found unwind plan table.
		// Assumption is that only one unwind plan table is present per binary.
		// TODO(kakkoyun): We might have multiple processes and multiple libraries, so multiple plan tables?
		return buildTable(fdes), nil
	}

	return nil, errors.New("failed to find unwind plan table for given PID")
}

func readFDEs(path string, start uint64) (frame.FrameDescriptionEntries, error) {
	obj, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open elf: %w", err)
	}
	defer obj.Close()

	// TODO(kakkoyun): Consider using the following section as a fallback.
	// unwind, err := obj.Section(".debug_frame").Data()

	sec := obj.Section(".eh_frame")
	if sec == nil {
		return nil, fmt.Errorf("failed to find .eh_frame section")
	}

	ehFrame, err := sec.Data()
	if err != nil {
		return nil, fmt.Errorf("failed to read .eh_frame section: %w", err)
	}

	// TODO(kakkoyun): Cache the unwind plan table.
	// TODO(kakkoyun): Can we assume byte order of ELF file same with .eh_frame? We can, right?!
	fe, err := frame.Parse(ehFrame, obj.ByteOrder, start, pointerSize(obj.Machine), sec.Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse frame data: %w", err)
	}
	return fe, nil
}

func buildTable(fdes frame.FrameDescriptionEntries) PlanTable {
	table := make(PlanTable, 0, len(fdes))
	for _, fde := range fdes {
		table = append(table, buildTableRow(fde))
	}

	return table
}

func buildTableRow(fde *frame.FrameDescriptionEntry) PlanTableRow {
	// TODO(kakkoyun): Shall we directly build "Instruction"s?
	row := PlanTableRow{
		Begin: fde.Begin(),
		End:   fde.End(),
	}

	fc := frame.ExecuteDwarfProgram(fde)

	// RetAddrReg is populated by frame.ExecuteDwarfProgram executeCIEInstructions.
	// TODO(kakkoyun): Is this enough do we need to any arch specific look up?
	// - https://github.com/go-delve/delve/blob/master/pkg/dwarf/regnum
	rule, found := fc.Regs[fc.RetAddrReg]
	if found {
		switch rule.Rule {
		case frame.RuleOffset:
			row.RIP = Instruction{Op: OpCfaOffset, Off: rule.Offset}
		case frame.RuleUndefined:
			row.RIP = Instruction{Op: OpUndefined}
		default:
			row.RIP = Instruction{Op: OpUnimplemented}
		}
	} else {
		row.RIP = Instruction{Op: OpUnimplemented}
	}

	row.RSP = Instruction{Op: OpRegister, Reg: fc.CFA.Reg, Off: fc.CFA.Offset}

	return row
}

func pointerSize(arch elf.Machine) int {
	switch arch {
	case elf.EM_386:
		return 4
	case elf.EM_AARCH64, elf.EM_X86_64:
		return 8
	default:
		return 0
	}
}
