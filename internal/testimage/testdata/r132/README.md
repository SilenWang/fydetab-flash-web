# Real-image slices used by the tests

These files are byte-for-byte captures from one real Rockchip update image. They
are not synthetic fixtures and are **not** FydeOS v23.0-SP1 data.

| | |
|---|---|
| source | `/mnt/raid/openfyde/rk3588-image-maker/openfyde.r132.img` (test machine) |
| product | openFyde **r132** (Chromium 132 / platform 16093) |
| size | 8,348,478,026 bytes |
| sha256 | `01fe1c0452606fc097d3816a73325c773013019954ed4f18fa55677842670ecc` |
| md5 | `859643405c1bb4ec87d6e8cb29887b38` |
| cut by | SIL-520, `extract-rk-slices.sh` |

The official FydeOS v23.0-SP1 image was not on the test machine, so no
v23.0-SP1 expectation may be written from these numbers.

| file | absolute offset | bytes | content |
|---|---:|---:|---|
| `rkfw-head.bin` | 0 | 102 | RKFW header (`head_len` 102, `image_offset` 412198) |
| `rkaf-head.bin` | 412198 | 2048 | RKAF header + 10-entry item table |
| `parameter.bin` | 829990 | 590 | `parameter` item: `PARM` + length 578 + text + CRC |
| `rkaf-crc-trailer.bin` | 4053510694 | 4 | bytes at `image_offset + rkaf.length`: `00 00 00 00` |
| `md5-tail.bin` | 8348477994 | 32 | ASCII `a2329d6f0af98a7379bec6faafbdcfe1` at EOF-32 |

`parameter` is item 2 of the table (`pos` 417792) and its slot inside the archive
runs to the next item at 419840, i.e. 2048 bytes; only its 590 payload bytes were
captured.

`internal/testimage` reassembles these slices into a sparse file of the real
8,348,478,026-byte size, so tests exercise the real layout (offsets, archive
length, the 4 GiB of trailing padding) instead of a miniature fixture.
