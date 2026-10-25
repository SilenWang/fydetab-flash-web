# FyeOS v23.0-SP1 slices (official Fydetab Duo image)

These are byte-for-byte captures from the **official** image SIL-515 flashed
before the Fydetab Duo stopped booting. They are the bytes the 4 GiB archive
hole and the wrong-IDB findings were measured on.

| | |
|---|---|
| source | `/mnt/raid/openfyde/rk3588-image-maker/FydeOS_FydeTab_Duo_v23.0-SP1-com.img` |
| product | FydeOS **v23.0-SP1** (Fydetab Duo) |
| size | 8,360,540,746 bytes |
| sha256 | `5a3f79918a987160e355f680ad1e2b828c93d7f97a6c97aab57b04a197fe7010` |
| md5 | `987b168044d49a763888600de60ed52f` |
| cut by | SIL-520 (`extract-rk-slices.sh`), attached to that issue |

| file | absolute offset | bytes | content |
|---|---:|---:|---|
| `rkfw-head.bin` | 0 | 102 | RKFW header (`head_len` 102, `image_offset` 422438) |
| `rkaf-head.bin` | 422438 | 2048 | RKAF header + 10-entry item table |
| `parameter.bin` | 850470 | 590 | `parameter` item: `PARM` + length 578 + text + CRC |
| `rkaf-crc-trailer.bin` | 4065573414 | 4 | bytes at `image_offset + rkaf.length`: `00 00 00 00` |
| `md5-tail.bin` | 8360540714 | 32 | ASCII `25f8c3b35c0289cf147940ed5452584c` at EOF-32 |

The capture deliberately holds no payload bytes (the full image is 8.36 GB).
Consequences the tests spell out:

- the **hole size** (4 GiB) is derivable from the file size and the two 32-bit
  length fields, and is asserted;
- the **hole position** is not derivable without payload signatures, so the
  parser keeps the table `pos` and marks the offsets unverified, and
  `uf --dry-run` refuses to write. That refusal is the point: SIL-515 wrote
  zeros from the unshifted offsets and reported "7/7 partitions ok".

`internal/testimage` reassembles these slices into a sparse file of the real
8,360,540,746-byte size; no multi-GiB download is needed to run the tests.
