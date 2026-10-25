// Main-thread WebUSB host.
//
// WebUSB is Window-only and requestDevice() must run inside a user gesture, so
// the device itself lives here and the worker's Go core drives it through RPC.
// This module only moves bytes; every protocol decision is in Go.

export const ROCKCHIP_VENDOR_ID = 0x2207;

/**
 * findRockusbInterface picks the interface a rockusb device exposes: a bulk IN
 * + bulk OUT pair, preferring the vendor-specific alt setting the real
 * 2207:350b maskrom describes (class 0xff / 0x06 / 0x05).
 */
export function findRockusbInterface(device) {
  const configs = device.configuration ? [device.configuration] : device.configurations;
  for (const config of configs) {
    const candidates = [];
    for (const iface of config.interfaces) {
      for (const alt of iface.alternates) {
        let bulkIn = null;
        let bulkOut = null;
        for (const ep of alt.endpoints) {
          if (ep.type !== 'bulk') continue;
          if (ep.direction === 'in' && (!bulkIn || ep.packetSize > bulkIn.packetSize)) bulkIn = ep;
          if (ep.direction === 'out' && (!bulkOut || ep.packetSize > bulkOut.packetSize)) bulkOut = ep;
        }
        if (bulkIn && bulkOut) {
          candidates.push({
            interfaceNumber: iface.interfaceNumber,
            alternateSetting: alt.alternateSetting,
            interfaceClass: alt.interfaceClass,
            bulkIn: bulkIn.endpointNumber,
            bulkOut: bulkOut.endpointNumber,
            vendorSpecific: alt.interfaceClass === 0xff,
          });
        }
      }
    }
    const preferred = candidates.find((c) => c.vendorSpecific) ?? candidates[0];
    if (preferred) return { ...preferred, configurationValue: config.configurationValue };
  }
  return null;
}

/** describeDevice turns a USBDevice into the RawInfo the Go core expects. */
export function describeDevice(device, handle) {
  const iface = findRockusbInterface(device);
  return {
    vendorId: device.vendorId,
    productId: device.productId,
    usbVersionMajor: device.usbVersionMajor,
    usbVersionMinor: device.usbVersionMinor,
    usbVersionSubminor: device.usbVersionSubminor,
    productName: device.productName ?? '',
    serialNumber: device.serialNumber ?? '',
    interfaceNumber: iface ? iface.interfaceNumber : -1,
    interfaceClass: iface ? iface.interfaceClass : 0,
    bulkIn: iface ? iface.bulkIn : 0,
    bulkOut: iface ? iface.bulkOut : 0,
    handle,
  };
}

const CONTROL_SETUP = {
  0x40: { requestType: 'vendor', recipient: 'device' }, // vendor, host-to-device
};

/**
 * createWebUSBHost returns the host surface bridge.js expects. `sessions` keeps
 * the opened device per DevNo; the handle (not the DevNo) decides which
 * physical device is opened, so a reordered device list cannot open the wrong
 * board.
 */
export function createWebUSBHost(usb = globalThis.navigator?.usb) {
  const handles = new Map();
  const sessions = new Map();

  function requireUsb() {
    if (!usb) throw new Error('WebUSB is not available in this browser');
  }

  async function list() {
    requireUsb();
    const devices = (await usb.getDevices()).filter((d) => d.vendorId === ROCKCHIP_VENDOR_ID);
    handles.clear();
    const seen = new Map();
    const out = [];
    for (const device of devices) {
      const base = `${device.vendorId}:${device.productId}:${device.serialNumber ?? ''}`;
      const index = seen.get(base) ?? 0;
      seen.set(base, index + 1);
      const handle = `${base}#${index}`;
      handles.set(handle, device);
      out.push(describeDevice(device, handle));
    }
    return out;
  }

  async function open(devNo, handle) {
    requireUsb();
    let device = handles.get(handle);
    if (!device) {
      await list();
      device = handles.get(handle);
    }
    if (!device) {
      throw new Error(`设备不在已授权列表里（handle ${handle}）；设备重新枚举后需要再次点“授权设备”`);
    }
    const iface = findRockusbInterface(device);
    if (!iface) throw new Error('设备没有 bulk IN + bulk OUT 接口对，不像 rockusb 设备');

    await device.open();
    if (!device.configuration) {
      await device.selectConfiguration(iface.configurationValue);
    }
    await device.claimInterface(iface.interfaceNumber);
    if (iface.alternateSetting) {
      await device.selectAlternateInterface(iface.interfaceNumber, iface.alternateSetting);
    }
    sessions.set(devNo, { device, iface });
  }

  function session(devNo) {
    const entry = sessions.get(devNo);
    if (!entry) throw new Error(`DevNo=${devNo} 没有打开的会话`);
    return entry;
  }

  async function close(devNo) {
    const entry = sessions.get(devNo);
    if (!entry) return;
    sessions.delete(devNo);
    await entry.device.releaseInterface(entry.iface.interfaceNumber).catch(() => {});
    await entry.device.close().catch(() => {});
  }

  async function controlOut(devNo, requestType, request, value, index, data) {
    const setup = CONTROL_SETUP[requestType];
    if (!setup) throw new Error(`unsupported bmRequestType 0x${requestType.toString(16)}`);
    const { device } = session(devNo);
    const result = await device.controlOut({ ...setup, request, value, index }, data);
    return { bytesWritten: result.bytesWritten };
  }

  async function bulkOut(devNo, endpoint, data) {
    const { device } = session(devNo);
    const result = await device.transferOut(endpoint, data);
    if (result.status !== 'ok') throw new Error(`bulk OUT endpoint ${endpoint}: ${result.status}`);
    return { bytesWritten: result.bytesWritten };
  }

  async function bulkIn(devNo, endpoint, length) {
    const { device } = session(devNo);
    const result = await device.transferIn(endpoint, length);
    if (result.status !== 'ok') throw new Error(`bulk IN endpoint ${endpoint}: ${result.status}`);
    if (!result.data || result.data.byteLength === 0) {
      return { data: new ArrayBuffer(0) };
    }
    const view = new Uint8Array(result.data.buffer, result.data.byteOffset, result.data.byteLength);
    return { data: view.slice().buffer };
  }

  async function reset(devNo) {
    const entry = sessions.get(devNo);
    if (!entry) return;
    await entry.device.reset().catch(() => {});
  }

  return { list, open, close, controlOut, bulkOut, bulkIn, reset };
}
