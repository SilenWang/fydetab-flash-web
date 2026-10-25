// In-memory rockusb device for the page's demo mode and for the node tests.
//
// It behaves like the two states stage 2 measured on real hardware:
//
//   - MASKROM: bulk OUT of a 31-byte CBW fails with a WebUSB transfer error,
//     exactly like the real BootROM did for all five read commands.
//   - LOADER: CBWs are answered with canned read-only data and a CSW.
//
// A Download Boot over EP0 vendor requests moves it from MASKROM to LOADER,
// which is what makes `ld → db → ld → info` demonstrable without a board.
//
// The mock is deliberately strict: it validates CBW framing (signature, length,
// LUN, CDB length), echoes the tag, and refuses anything outside the read-only
// opcode set. A bug in the Go encoder shows up here instead of only on
// hardware.

export const CBW_LENGTH = 31;
export const CSW_LENGTH = 13;
export const CBW_SIGNATURE = 0x43425355;
export const CSW_SIGNATURE = 0x53425355;
export const ROCKCHIP_VENDOR_ID = 0x2207;
export const MOCK_PRODUCT_ID = 0x350b;
export const VENDOR_REQUEST_471 = 0x0471;
export const VENDOR_REQUEST_472 = 0x0472;

const READ_OPS = new Set([0x00, 0x1b, 0x1a, 0xaa, 0x2b]);

const STALL_MESSAGE = "Failed to execute 'transferOut' on 'USBDevice': A transfer error has occurred.";

function readOnlyData(op) {
  switch (op) {
    case 0x1b: {
      const chip = new Uint8Array(16);
      chip.set([0x33, 0x35, 0x38, 0x38]); // "3588"
      return chip;
    }
    case 0x1a: {
      const flash = new Uint8Array(11);
      const view = new DataView(flash.buffer);
      view.setUint32(0, 0x00700000, true); // 3.5 GiB in 512 B sectors
      view.setUint16(4, 0x0400, true); // 512 KiB blocks
      flash[6] = 0x08; // 4 KiB pages
      flash[7] = 0x10; // 16 bit ECC
      flash[8] = 0x0a; // access time
      flash[9] = 0x01; // TOSHIBA
      flash[10] = 0x03; // CS0+CS1
      return flash;
    }
    case 0xaa:
      return new Uint8Array([0x0f, 0x01, 0, 0, 0, 0, 0, 0]);
    case 0x2b:
      return new Uint8Array([0x02, 0x02, 0, 0]); // eMMC (bit 1) + SPI NOR (bit 9)
    default:
      return new Uint8Array(0);
  }
}

/**
 * createMockHost returns a host object with the same surface as the WebUSB
 * host (see bridge.js), plus a few inspection helpers used by the UI/tests.
 */
export function createMockHost({ handle = 'mock:1', onLog = () => {} } = {}) {
  const state = {
    mode: 'maskrom', // maskrom | loader
    opened: false,
    pending: null,
    controls: [],
    cbws: [],
    bulkInReads: 0,
    modeFlips: 0,
  };

  function assertDevNo(devNo) {
    if (devNo !== 1) throw new Error(`mock: DevNo=${devNo} does not exist (only DevNo=1)`);
  }

  function rawInfo() {
    return {
      vendorId: ROCKCHIP_VENDOR_ID,
      productId: MOCK_PRODUCT_ID,
      usbVersionMajor: 2,
      usbVersionMinor: 0,
      usbVersionSubminor: state.mode === 'loader' ? 1 : 0, // bit0 => LOADER
      productName: state.mode === 'loader' ? 'RK3588 mock (loader)' : 'RK3588 mock (maskrom)',
      serialNumber: '',
      interfaceNumber: 0,
      interfaceClass: 0xff,
      bulkIn: 1,
      bulkOut: 2,
      handle,
    };
  }

  function cswFor(tag, status = 0) {
    const csw = new Uint8Array(CSW_LENGTH);
    const view = new DataView(csw.buffer);
    view.setUint32(0, CSW_SIGNATURE, true);
    view.setUint32(4, tag >>> 0, true);
    view.setUint32(8, 0, true);
    csw[12] = status;
    return csw;
  }

  return {
    // --- host API ---------------------------------------------------------
    async list() {
      return [rawInfo()];
    },
    async open(devNo) {
      assertDevNo(devNo);
      state.opened = true;
    },
    async close(devNo) {
      assertDevNo(devNo);
      state.opened = false;
      state.pending = null;
    },
    async controlOut(devNo, requestType, request, _value, index, data) {
      assertDevNo(devNo);
      const bytes = data instanceof Uint8Array ? data : new Uint8Array(data ?? 0);
      state.controls.push({ requestType, request, index, length: bytes.length });
      if (state.mode !== 'maskrom') {
        // A loader device has no reason to take BootROM downloads; accept
        // and ignore so a stray call cannot wedge the demo.
        return { bytesWritten: bytes.length };
      }
      if (requestType !== 0x40 || request !== 0x0c) {
        throw new Error(`mock: unexpected vendor request type=0x${requestType.toString(16)} req=0x${request.toString(16)}`);
      }
      if (index !== VENDOR_REQUEST_471 && index !== VENDOR_REQUEST_472) {
        throw new Error(`mock: vendor index 0x${index.toString(16)} is not 0x0471/0x0472`);
      }
      // The last chunk of the usbplug segment is shorter than the 4096 byte
      // packet size; that is when a real device re-enumerates as LOADER.
      if (index === VENDOR_REQUEST_472 && bytes.length < 4096) {
        state.mode = 'loader';
        state.modeFlips += 1;
        onLog('mock device re-enumerated as LOADER after the usbplug segment');
      }
      return { bytesWritten: bytes.length };
    },
    async bulkOut(devNo, endpoint, data) {
      assertDevNo(devNo);
      const bytes = data instanceof Uint8Array ? data : new Uint8Array(data ?? 0);
      if (state.mode !== 'loader') {
        // The stage-2 hardware finding, reproduced on purpose.
        throw new Error(STALL_MESSAGE);
      }
      if (endpoint !== 2) throw new Error(`mock: bulk OUT on endpoint ${endpoint}, expected 2`);
      if (bytes.length !== CBW_LENGTH) throw new Error(`mock: bulk OUT of ${bytes.length} bytes is not a CBW`);
      state.cbws.push(Array.from(bytes));
      const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
      if (view.getUint32(0, true) !== CBW_SIGNATURE) throw new Error('mock: bad CBW signature');
      if (bytes[14] !== 0x06) throw new Error(`mock: unexpected CBWCB length ${bytes[14]}`);
      const op = bytes[15];
      if (!READ_OPS.has(op)) throw new Error(`mock: opcode 0x${op.toString(16)} is not a read-only command`);
      const tag = view.getUint32(4, true);
      const transferLength = view.getUint32(8, true);
      state.pending = { tag, op, transferLength, data: readOnlyData(op), sent: false };
      return { bytesWritten: bytes.length };
    },
    async bulkIn(devNo, endpoint, length) {
      assertDevNo(devNo);
      if (endpoint !== 1) throw new Error(`mock: bulk IN on endpoint ${endpoint}, expected 1`);
      state.bulkInReads += 1;
      const pending = state.pending;
      if (!pending) throw new Error('mock: bulk IN without a pending CBW');
      if (pending.transferLength > 0 && !pending.sent) {
        pending.sent = true;
        const out = new Uint8Array(length);
        out.set(pending.data.subarray(0, Math.min(length, pending.data.length)));
        return { data: out };
      }
      state.pending = null;
      return { data: cswFor(pending.tag) };
    },
    async reset(devNo) {
      assertDevNo(devNo);
      state.pending = null;
    },

    // --- inspection (UI + tests) -----------------------------------------
    inspect() {
      return {
        mode: state.mode,
        opened: state.opened,
        controls: state.controls.length,
        controlLog: state.controls.map((c) => ({ ...c })),
        cbws: state.cbws.map((bytes) => ({ op: bytes[15], length: bytes.length })),
        modes: state.modeFlips,
      };
    },
  };
}
