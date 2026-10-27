// M0 read-only WebUSB probe for Rockchip RK3588 / Fydetab Duo.
//
// Everything this page sends to the device is a 31-byte CBW for one of the five
// commands in protocol.js (chip info, flash info, capability, storage list,
// test-unit-ready). There is no write, erase, reset or storage-switch path in
// this file, and transferOut refuses any buffer whose length is not CBW_LENGTH.

import {
  CBW_LENGTH,
  COMMANDS,
  COMMANDS_BY_OP,
  CSW_LENGTH,
  ERROR_NAMES,
  ROCKCHIP_VENDOR_ID,
  STATUS_FAILED,
} from './protocol.js';
import {
  ProbeError,
  assertReadOnlyOp,
  decodeCommandData,
  decodeCSW,
  encodeCBW,
  hex,
  makeTag,
  modeFromDescriptor,
  packBCDUSB,
} from './rockusb.js';

const PROBE_VERSION = '0.1.0';
const TRANSFER_TIMEOUT_MS = 5000; // matches rkdeveloptool's RKU_Read_EX timeout
const MAX_TRACE_ROWS = 300;

// ---------------------------------------------------------------------------
// Session state
// ---------------------------------------------------------------------------

function newSession() {
  return {
    trace: [],
    commands: [],
    errors: [],
    availableDevices: [],
    startedAt: performance.now(),
    seq: 0,
    device: null,
    link: null,
    descriptor: null,
    mode: 'UNKNOWN',
    lastExport: null,
  };
}

let state = newSession();
let selectedDevice = null;
let busy = false;

// ---------------------------------------------------------------------------
// Trace
// ---------------------------------------------------------------------------

function dataSummary(bytes, limit = 32) {
  if (!bytes || bytes.byteLength === 0) return '(no data)';
  const head = hex(bytes, limit);
  return bytes.byteLength > limit ? `${head} … (${bytes.byteLength} bytes)` : `${head} (${bytes.byteLength} bytes)`;
}

function logTransfer(entry) {
  // Every control/bulk transfer is recorded, whether it succeeds or not:
  // direction, request fields, length, data summary and duration.
  state.seq += 1;
  state.trace.push({
    seq: state.seq,
    tMs: Number((performance.now() - state.startedAt).toFixed(3)),
    kind: entry.kind,
    endpoint: entry.endpoint ?? null,
    direction: entry.direction,
    requestType: entry.requestType ?? null,
    request: entry.request ?? null,
    value: entry.value ?? null,
    index: entry.index ?? null,
    requestedLength: entry.requestedLength ?? null,
    length: entry.length,
    status: entry.status ?? null,
    dataHex: entry.data ? hex(entry.data, 64) : null,
    dataSummary: entry.data ? dataSummary(entry.data) : null,
    durationMs: Number(entry.durationMs.toFixed(3)),
    op: entry.op ?? null,
    opName: entry.op != null ? (COMMANDS_BY_OP[entry.op]?.name ?? `0x${entry.op.toString(16)}`) : null,
    stage: entry.stage ?? null,
    note: entry.note ?? null,
  });
}

function logError(code, detail, context) {
  const rec = {
    atMs: Number((performance.now() - state.startedAt).toFixed(3)),
    code,
    name: ERROR_NAMES[code] ?? `ERR_UNKNOWN(${code})`,
    detail,
    context: context ?? null,
  };
  state.errors.push(rec);
  return rec;
}

// Framing, decoders and the read-only guard live in ./rockusb.js so they can be
// unit-tested without a browser (see rockusb.test.mjs).

// ---------------------------------------------------------------------------
// Device description
// ---------------------------------------------------------------------------

function hex4(n) {
  return `0x${n.toString(16).padStart(4, '0')}`;
}

function describeEndpoint(ep) {
  return {
    endpointNumber: ep.endpointNumber,
    direction: ep.direction,
    type: ep.type,
    packetSize: ep.packetSize,
    address: `${ep.direction === 'in' ? '0x8' : '0x0'}${ep.endpointNumber.toString(16)}`,
  };
}

function describeConfiguration(config) {
  return {
    configurationValue: config.configurationValue,
    configurationName: config.configurationName ?? null,
    interfaces: config.interfaces.map((iface) => ({
      interfaceNumber: iface.interfaceNumber,
      alternates: iface.alternates.map((alt) => ({
        alternateSetting: alt.alternateSetting,
        interfaceClass: alt.interfaceClass,
        interfaceSubclass: alt.interfaceSubclass,
        interfaceProtocol: alt.interfaceProtocol,
        interfaceName: alt.interfaceName ?? null,
        endpoints: alt.endpoints.map(describeEndpoint),
      })),
    })),
  };
}

function describeDevice(device) {
  const bcdUSB = packBCDUSB(device.usbVersionMajor, device.usbVersionMinor, device.usbVersionSubminor);
  return {
    vendorId: hex4(device.vendorId),
    productId: hex4(device.productId),
    vendorIdDecimal: device.vendorId,
    productIdDecimal: device.productId,
    manufacturerName: device.manufacturerName ?? null,
    productName: device.productName ?? null,
    serialNumber: device.serialNumber ?? null,
    bcdUSB: hex4(bcdUSB),
    usbVersion: `${device.usbVersionMajor}.${device.usbVersionMinor}.${device.usbVersionSubminor}`,
    bcdUSBLowBit: bcdUSB & 1,
    mode: modeFromDescriptor(device.vendorId, bcdUSB),
    deviceVersion: `${device.deviceVersionMajor}.${device.deviceVersionMinor}.${device.deviceVersionSubminor}`,
    deviceClass: device.deviceClass,
    deviceSubclass: device.deviceSubclass,
    deviceProtocol: device.deviceProtocol,
    configurations: device.configurations.map(describeConfiguration),
    activeConfiguration: device.configuration ? describeConfiguration(device.configuration) : null,
    opened: device.opened,
  };
}

function findTransferInterface(device) {
  const configs = device.configuration ? [device.configuration] : device.configurations;
  for (const config of configs) {
    const candidates = [];
    for (const iface of config.interfaces) {
      for (const alt of iface.alternates) {
        let epIn = null;
        let epOut = null;
        for (const ep of alt.endpoints) {
          if (ep.type !== 'bulk') continue;
          if (ep.direction === 'in' && (!epIn || ep.packetSize > epIn.packetSize)) epIn = ep;
          if (ep.direction === 'out' && (!epOut || ep.packetSize > epOut.packetSize)) epOut = ep;
        }
        if (epIn && epOut) {
          candidates.push({ config, iface, alt, epIn, epOut, vendorSpecific: alt.interfaceClass === 0xff });
        }
      }
    }
    const preferred = candidates.find((c) => c.vendorSpecific) ?? candidates[0];
    if (preferred) return preferred;
  }
  return null;
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

function withTimeout(promise, ms, what) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new ProbeError(-4, `${what} timed out after ${ms} ms`)), ms);
    promise.then(
      (v) => {
        clearTimeout(timer);
        resolve(v);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      },
    );
  });
}

async function transferOut(link, bytes, meta) {
  // Structural read-only guarantee: the only bulk-OUT payload this page can
  // ever produce is a CBW for a whitelisted read command.
  assertReadOnlyOp(meta.op, bytes.byteLength);

  const started = performance.now();
  let result;
  try {
    result = await withTimeout(link.device.transferOut(link.epOut, bytes), TRANSFER_TIMEOUT_MS, 'bulk OUT');
  } catch (err) {
    logTransfer({
      kind: 'bulk-out',
      endpoint: link.epOut,
      direction: 'out',
      requestedLength: bytes.byteLength,
      length: bytes.byteLength,
      data: bytes,
      durationMs: performance.now() - started,
      status: 'error',
      note: err.message,
      ...meta,
    });
    throw err instanceof ProbeError ? err : new ProbeError(-3, `${meta.opName} bulk OUT failed: ${err.message}`);
  }

  logTransfer({
    kind: 'bulk-out',
    endpoint: link.epOut,
    direction: 'out',
    requestedLength: bytes.byteLength,
    length: result.bytesWritten ?? bytes.byteLength,
    data: bytes,
    durationMs: performance.now() - started,
    status: result.status,
    ...meta,
  });

  if (result.status !== 'ok' || result.bytesWritten !== bytes.byteLength) {
    throw new ProbeError(-3, `bulk OUT status=${result.status} bytesWritten=${result.bytesWritten}`);
  }
}

async function transferIn(link, length, meta) {
  const chunks = [];
  let received = 0;
  let attempt = 0;

  while (received < length) {
    attempt += 1;
    if (attempt > 8) {
      throw new ProbeError(-4, `short read: ${received}/${length} bytes after ${attempt - 1} transfers`);
    }
    const want = length - received;
    const started = performance.now();
    let result;
    try {
      result = await withTimeout(link.device.transferIn(link.epIn, want), TRANSFER_TIMEOUT_MS, 'bulk IN');
    } catch (err) {
      logTransfer({
        kind: 'bulk-in',
        endpoint: link.epIn,
        direction: 'in',
        requestedLength: want,
        length: 0,
        data: null,
        durationMs: performance.now() - started,
        status: 'error',
        note: err.message,
        ...meta,
      });
      throw err instanceof ProbeError ? err : new ProbeError(-4, `${meta.opName} bulk IN failed: ${err.message}`);
    }

    const bytes = result.data
      ? new Uint8Array(result.data.buffer, result.data.byteOffset, result.data.byteLength)
      : new Uint8Array(0);

    logTransfer({
      kind: 'bulk-in',
      endpoint: link.epIn,
      direction: 'in',
      requestedLength: want,
      length: bytes.byteLength,
      data: bytes.byteLength > 0 ? bytes : null,
      durationMs: performance.now() - started,
      status: result.status,
      ...meta,
    });

    if (result.status !== 'ok') {
      throw new ProbeError(-4, `bulk IN status=${result.status}`);
    }
    if (bytes.byteLength === 0) {
      throw new ProbeError(-4, `device returned 0 bytes while expecting ${length}`);
    }

    chunks.push(bytes);
    received += bytes.byteLength;
  }

  const out = new Uint8Array(received);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

async function runCommand(link, command) {
  const record = {
    op: command.op,
    opName: command.name,
    description: command.description,
    ok: false,
    errorCode: 0,
    errorName: ERROR_NAMES[0],
    errorDetail: null,
    dataHex: null,
    decoded: null,
    csw: null,
    durationMs: 0,
  };
  const started = performance.now();
  const tag = makeTag();

  try {
    const cbw = encodeCBW(tag, command.op, command.transferLength);
    await transferOut(link, cbw, {
      op: command.op,
      opName: command.name,
      stage: 'cbw',
      note: `CBW tag=0x${tag.toString(16)}`,
    });

    let data = null;
    if (command.dataLength > 0) {
      data = await transferIn(link, command.dataLength, {
        op: command.op,
        opName: command.name,
        stage: 'data',
      });
    }

    const cswBytes = await transferIn(link, CSW_LENGTH, {
      op: command.op,
      opName: command.name,
      stage: 'csw',
    });
    const csw = decodeCSW(cswBytes);
    record.csw = csw;

    if (csw.tag !== tag) {
      throw new ProbeError(-5, `CSW tag 0x${csw.tag.toString(16)} does not match CBW tag 0x${tag.toString(16)}`);
    }
    if (csw.status === STATUS_FAILED) {
      throw new ProbeError(-8, 'device reported CSW status failed');
    }

    if (data) {
      record.dataHex = hex(data);
      record.decoded = decodeCommandData(command.op, data);
    }
    record.ok = true;
  } catch (err) {
    const code = err instanceof ProbeError ? err.code : -8;
    record.errorCode = code;
    record.errorName = ERROR_NAMES[code] ?? `ERR_UNKNOWN(${code})`;
    record.errorDetail = err.message;
    logError(code, err.message, command.name);
  }

  record.durationMs = Number((performance.now() - started).toFixed(3));
  state.commands.push(record);
  return record;
}

// ---------------------------------------------------------------------------
// Probe run
// ---------------------------------------------------------------------------

async function probeDevice(device) {
  const authorized = state.availableDevices ?? [];
  state = newSession();
  state.availableDevices = authorized;
  state.device = device;
  render();

  await device.open();
  if (!device.configuration) {
    const configs = device.configurations;
    if (configs.length === 0) throw new ProbeError(-1, 'device exposes no USB configuration');
    const value = configs[0].configurationValue;
    await device.selectConfiguration(value);
  }

  const target = findTransferInterface(device);
  if (!target) {
    await device.close().catch(() => {});
    throw new ProbeError(-10, 'no interface with a bulk IN + bulk OUT pair found');
  }

  await device.claimInterface(target.iface.interfaceNumber);
  if (target.alt.alternateSetting !== 0) {
    await device.selectAlternateInterface(target.iface.interfaceNumber, target.alt.alternateSetting);
  }

  const link = {
    device,
    interfaceNumber: target.iface.interfaceNumber,
    alternateSetting: target.alt.alternateSetting,
    interfaceClass: target.alt.interfaceClass,
    epIn: target.epIn.endpointNumber,
    epOut: target.epOut.endpointNumber,
  };
  state.link = link;
  state.descriptor = describeDevice(device);
  state.mode = state.descriptor.mode;
  render();

  try {
    for (const command of COMMANDS) {
      // eslint-disable-next-line no-await-in-loop
      await runCommand(link, command);
      render();
    }
  } finally {
    await device.releaseInterface(link.interfaceNumber).catch(() => {});
    await device.close().catch(() => {});
  }
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

function buildExport() {
  return {
    schema: 'fydetab-flash-web/probe-trace@1',
    readOnly: true,
    probe: {
      version: PROBE_VERSION,
      protocol: 'rockusb/M0-readonly',
      exportedAt: new Date().toISOString(),
      userAgent: navigator.userAgent,
      secureContext: window.isSecureContext,
      origin: window.location.origin,
      commandTable: COMMANDS,
    },
    device: state.descriptor,
    mode: state.mode,
    link: state.link
      ? {
          interfaceNumber: state.link.interfaceNumber,
          alternateSetting: state.link.alternateSetting,
          interfaceClass: state.link.interfaceClass,
          bulkIn: state.link.epIn,
          bulkOut: state.link.epOut,
        }
      : null,
    commands: state.commands,
    transfers: state.trace,
    errors: state.errors,
    counts: {
      transfers: state.trace.length,
      commands: state.commands.length,
      failedCommands: state.commands.filter((c) => !c.ok).length,
      errors: state.errors.length,
    },
  };
}

function exportJSON() {
  state.lastExport = buildExport();
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const vid = state.descriptor?.vendorId ?? 'unknown';
  const pid = state.descriptor?.productId ?? 'unknown';
  const name = `rk3588-probe-${vid}-${pid}-${stamp}.json`;
  const blob = new Blob([JSON.stringify(state.lastExport, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  setStatus(`已导出 ${name}（${state.trace.length} 条传输记录）`, 'ok');
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

const $ = (id) => document.getElementById(id);

function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = value;
    else if (key === 'dataset') Object.assign(node.dataset, value);
    else node.setAttribute(key, value);
  }
  for (const child of [].concat(children)) {
    if (child == null) continue;
    node.appendChild(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return node;
}

function setStatus(text, tone = 'info') {
  const node = $('status');
  node.textContent = text;
  node.dataset.tone = tone;
}

function kvTable(rows) {
  const tbody = el('tbody');
  for (const [key, value] of rows) {
    if (value == null) continue;
    tbody.appendChild(el('tr', {}, [el('th', { text: key }), el('td', { text: String(value) })]));
  }
  return el('table', { class: 'kv' }, [tbody]);
}

function renderDeviceList() {
  const container = $('device-list');
  container.replaceChildren();

  const devices = state.availableDevices ?? [];
  if (devices.length === 0) {
    container.appendChild(el('p', { class: 'muted', text: '尚无已授权设备。点击“授权设备”在浏览器弹窗里选择 Rockchip 设备（VID 0x2207）。' }));
    return;
  }

  for (const device of devices) {
    const button = el('button', {
      class: `device-card${device === selectedDevice ? ' selected' : ''}`,
      type: 'button',
    }, [
      el('span', { class: 'device-title', text: device.productName || 'Rockchip device' }),
      el('span', {
        class: 'device-sub',
        text: `${hex4(device.vendorId)}:${hex4(device.productId)}${device.serialNumber ? ` · SN ${device.serialNumber}` : ''}`,
      }),
    ]);
    button.addEventListener('click', () => {
      selectedDevice = device;
      renderDeviceList();
      updateButtons();
    });
    container.appendChild(button);
  }
}

function renderDescriptor() {
  const container = $('descriptor');
  container.replaceChildren();
  const d = state.descriptor;
  if (!d) {
    container.appendChild(el('p', { class: 'muted', text: '尚未读取设备描述符。' }));
    return;
  }

  container.appendChild(kvTable([
    ['VID:PID', `${d.vendorId}:${d.productId}`],
    ['厂商 / 产品', [d.manufacturerName, d.productName].filter(Boolean).join(' / ') || null],
    ['序列号', d.serialNumber],
    ['USB 版本 (bcdUSB)', `${d.bcdUSB}（${d.usbVersion}，bit0=${d.bcdUSBLowBit}）`],
    ['设备模式（RKScan 判定）', d.mode],
    ['设备版本 (bcdDevice)', d.deviceVersion],
    ['设备类 / 子类 / 协议', `${d.deviceClass} / ${d.deviceSubclass} / ${d.deviceProtocol}`],
    ['传输接口', state.link ? `interface ${state.link.interfaceNumber}，bulk IN 0x8${state.link.epIn.toString(16)}，bulk OUT 0x0${state.link.epOut.toString(16)}` : null],
  ]));

  const active = d.activeConfiguration ?? d.configurations[0];
  if (active) {
    const rows = [];
    for (const iface of active.interfaces) {
      for (const alt of iface.alternates) {
        rows.push(el('tr', {}, [
          el('td', { text: `interface ${iface.interfaceNumber}` }),
          el('td', { text: `alt ${alt.alternateSetting}` }),
          el('td', { text: `class 0x${alt.interfaceClass.toString(16).padStart(2, '0')}/0x${alt.interfaceSubclass.toString(16).padStart(2, '0')}/0x${alt.interfaceProtocol.toString(16).padStart(2, '0')}` }),
          el('td', { text: alt.endpoints.length === 0 ? '—' : alt.endpoints.map((ep) => `${ep.address} ${ep.direction} ${ep.type} ${ep.packetSize}B`).join('; ') }),
        ]));
      }
    }
    container.appendChild(el('h3', { text: `配置 ${active.configurationValue} 的接口与 endpoint` }));
    container.appendChild(el('table', { class: 'grid' }, [
      el('thead', {}, [el('tr', {}, [
        el('th', { text: '接口' }), el('th', { text: '备用设置' }), el('th', { text: 'class/subclass/protocol' }), el('th', { text: 'endpoint（number/direction/type/packetSize）' }),
      ])]),
      el('tbody', {}, rows),
    ]));
  }
}

function renderCommandResults() {
  const container = $('results');
  container.replaceChildren();

  if (state.commands.length === 0) {
    container.appendChild(el('p', { class: 'muted', text: '尚未执行读取命令。' }));
    return;
  }

  for (const record of state.commands) {
    const body = el('div', { class: 'card-body' });

    if (record.ok && record.decoded) {
      for (const [key, value] of Object.entries(record.decoded)) {
        if (key === 'rawHex' || value == null) continue;
        body.appendChild(el('div', { class: 'row' }, [
          el('span', { class: 'row-key', text: key }),
          el('span', { class: 'row-value', text: Array.isArray(value) ? value.join(', ') : String(value) }),
        ]));
      }
    }
    if (record.dataHex) {
      body.appendChild(el('div', { class: 'row' }, [
        el('span', { class: 'row-key', text: 'rawHex' }),
        el('code', { class: 'row-value', text: record.dataHex }),
      ]));
    }
    if (!record.ok) {
      body.appendChild(el('div', { class: 'row error' }, [
        el('span', { class: 'row-key', text: '失败' }),
        el('span', { class: 'row-value', text: `${record.errorName} (${record.errorCode}): ${record.errorDetail}` }),
      ]));
    }

    const status = record.ok ? 'ok' : 'error';
    container.appendChild(el('article', { class: `card ${status}` }, [
      el('header', {}, [
        el('h3', { text: record.opName }),
        el('span', { class: `badge ${status}`, text: record.ok ? 'OK' : record.errorName }),
        el('span', { class: 'muted', text: `${record.durationMs} ms` }),
      ]),
      el('p', { class: 'muted small', text: record.description }),
      body,
    ]));
  }
}

function renderTrace() {
  const tbody = $('trace-body');
  tbody.replaceChildren();
  const rows = state.trace.slice(-MAX_TRACE_ROWS);
  for (const t of rows) {
    tbody.appendChild(el('tr', { class: t.status && t.status !== 'ok' ? 'error' : '' }, [
      el('td', { text: String(t.seq) }),
      el('td', { text: `${t.kind}${t.endpoint != null ? `/${t.endpoint}` : ''}` }),
      el('td', { text: t.opName ?? '—' }),
      el('td', { text: t.stage ?? '—' }),
      el('td', { text: `${t.length} B` }),
      el('td', { text: String(t.durationMs) }),
      el('td', { class: 'mono', text: t.dataSummary ?? '—' }),
      el('td', { text: t.note ?? t.status ?? '' }),
    ]));
  }
  $('trace-count').textContent = `${state.trace.length} 条传输（表格显示最近 ${rows.length} 条，导出包含全部）`;
}

function renderErrors() {
  const container = $('error-list');
  container.replaceChildren();
  if (state.errors.length === 0) {
    container.appendChild(el('p', { class: 'muted', text: '没有错误。' }));
    return;
  }
  const list = el('ul');
  for (const err of state.errors) {
    list.appendChild(el('li', {}, [
      el('code', { text: `${err.name} (${err.code})` }),
      el('span', { text: ` · ${err.context ?? '—'} · ${err.detail}` }),
    ]));
  }
  container.appendChild(list);
}

function render() {
  renderDeviceList();
  renderDescriptor();
  renderCommandResults();
  renderTrace();
  renderErrors();
  updateButtons();
}

function updateButtons() {
  const supported = Boolean(navigator.usb);
  $('btn-request').disabled = !supported || busy;
  $('btn-refresh').disabled = !supported || busy;
  $('btn-read').disabled = !supported || busy || !selectedDevice;
  $('btn-export').disabled = busy || state.trace.length === 0;
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

async function refreshDevices(requestNew) {
  if (!navigator.usb) return;
  try {
    const devices = requestNew
      ? [...(await navigator.usb.getDevices()), await navigator.usb.requestDevice({ filters: [{ vendorId: ROCKCHIP_VENDOR_ID }] })]
      : await navigator.usb.getDevices();

    state.availableDevices = dedupeDevices(devices);
    if (!selectedDevice || !state.availableDevices.includes(selectedDevice)) {
      selectedDevice = state.availableDevices[0] ?? null;
    }
    renderDeviceList();
    updateButtons();
    setStatus(
      state.availableDevices.length === 0
        ? '没有已授权设备。请点击“授权设备”。'
        : `已授权 ${state.availableDevices.length} 台设备，选中 ${hex4(selectedDevice.vendorId)}:${hex4(selectedDevice.productId)}。`,
      'info',
    );
  } catch (err) {
    if (err.name === 'NotFoundError') {
      setStatus('未选择设备（授权弹窗被取消）。', 'warn');
      return;
    }
    setStatus(`授权失败：${err.message}`, 'error');
    logError(-1, err.message, 'requestDevice');
    renderErrors();
  }
}

function dedupeDevices(devices) {
  const seen = new Set();
  const out = [];
  for (const device of devices) {
    const key = `${device.vendorId}:${device.productId}:${device.serialNumber ?? ''}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(device);
  }
  return out;
}

async function readDevice() {
  if (!selectedDevice) return;
  busy = true;
  updateButtons();
  setStatus('正在打开设备并读取（只读命令，不会写入）…', 'busy');
  try {
    await probeDevice(selectedDevice);
    const failed = state.commands.filter((c) => !c.ok);
    if (failed.length === 0) {
      setStatus(`读取完成：${state.commands.length} 条命令全部成功，导出 JSON 即可。`, 'ok');
    } else {
      setStatus(`读取完成，但有 ${failed.length} 条命令失败：${failed.map((c) => `${c.opName}=${c.errorName}`).join('，')}。失败已记录，未做任何绕过或重试写入。`, 'warn');
    }
  } catch (err) {
    const code = err instanceof ProbeError ? err.code : -1;
    logError(code, err.message, 'probeDevice');
    state.descriptor = selectedDevice ? describeDevice(selectedDevice) : state.descriptor;
    if (!state.link && state.trace.length === 0) {
      setStatus(`打开/配置设备失败：${err.message}`, 'error');
    } else {
      setStatus(`读取中断：${err.message}`, 'error');
    }
  } finally {
    busy = false;
    render();
  }
}

function clearSession() {
  const authorized = state.availableDevices ?? [];
  state = newSession();
  state.availableDevices = authorized;
  render();
  setStatus('已清空本次记录。', 'info');
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

function checkSupport() {
  const banner = $('support-warning');
  if (!navigator.usb) {
    banner.hidden = false;
    banner.textContent = '当前浏览器没有 WebUSB。请用桌面版 Chrome / Edge（或 Android Chrome）打开，Safari 与 Firefox 不支持。';
    return false;
  }
  if (!window.isSecureContext) {
    banner.hidden = false;
    banner.textContent = 'WebUSB 需要安全上下文：请通过 https 或 http://localhost 打开本页（pixi run dev 提供 localhost）。';
    return false;
  }
  banner.hidden = true;
  return true;
}

async function main() {
  checkSupport();
  $('btn-request').addEventListener('click', () => refreshDevices(true));
  $('btn-refresh').addEventListener('click', () => refreshDevices(false));
  $('btn-read').addEventListener('click', readDevice);
  $('btn-export').addEventListener('click', exportJSON);
  $('btn-clear').addEventListener('click', clearSession);

  navigator.usb?.addEventListener('connect', () => refreshDevices(false));
  navigator.usb?.addEventListener('disconnect', () => refreshDevices(false));

  render();
  if (navigator.usb) await refreshDevices(false);
  setStatus('就绪。授权设备后点击“读取设备信息”。', 'info');
}

main();
