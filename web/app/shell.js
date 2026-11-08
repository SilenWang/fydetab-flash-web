// Page shell: the parts of the interface that are not device logic.
//
//   · the log / diagnostics drawer (and its two panes)
//   · the persistent action bar, which mirrors what app.js has already
//     rendered in the panels so the primary action keeps its context
//   · drag & drop onto the image dropzone
//
// Everything here is presentation. It never talks to the worker or to a USB
// device; app.js remains the only owner of that state.

const $ = (id) => document.getElementById(id);

const el = {
  drawer: $('drawer'),
  scrim: $('scrim'),
  openDrawer: $('btn-drawer'),
  closeDrawer: $('btn-drawer-close'),
  tabLog: $('tab-log'),
  tabDiag: $('tab-diag'),
  paneLog: $('pane-log'),
  paneDiag: $('pane-diag'),
  actionImage: $('action-image'),
  actionDevice: $('action-device'),
  imageLabel: $('image-label'),
  imageSummary: $('image-summary'),
  deviceSelected: $('device-selected'),
  devices: $('devices'),
  modeBadge: $('mode-badge'),
  dropzone: $('dropzone'),
  dropzoneSub: document.querySelector('.dropzone-sub'),
  fileImage: $('file-image'),
  logBody: $('log-body'),
  toggleLog: $('btn-toggle-log'),
};

// --- drawer ---------------------------------------------------------------

function setDrawer(open, { focus = true } = {}) {
  el.drawer.hidden = !open;
  el.scrim.hidden = !open;
  el.openDrawer.setAttribute('aria-expanded', String(open));
  document.body.style.overflow = open ? 'hidden' : '';
  if (!open) return;
  if (focus) $('tab-log').focus();
  if (!el.logBody.hidden) el.logBody.scrollTop = el.logBody.scrollHeight;
}

function setTab(name) {
  const log = name === 'log';
  el.tabLog.setAttribute('aria-selected', String(log));
  el.tabDiag.setAttribute('aria-selected', String(!log));
  el.paneLog.hidden = !log;
  el.paneDiag.hidden = log;
}

el.openDrawer.addEventListener('click', () => setDrawer(true));
el.closeDrawer.addEventListener('click', () => {
  setDrawer(false);
  el.openDrawer.focus();
});
el.scrim.addEventListener('click', () => setDrawer(false));
el.tabLog.addEventListener('click', () => setTab('log'));
el.tabDiag.addEventListener('click', () => setTab('diag'));

document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && !el.drawer.hidden) {
    setDrawer(false);
    el.openDrawer.focus();
  }
});

// Keep the "collapse" control in step with the pane it hides.
el.toggleLog.addEventListener('click', () => {
  const collapsed = el.logBody.hidden;
  el.toggleLog.textContent = collapsed ? '收起' : '展开';
});

// --- action bar -----------------------------------------------------------
//
// app.js owns the values; this only mirrors them into the bar so the button
// never sits next to a stale file name or a device that has gone away.

function shortName(name) {
  if (!name) return '';
  const trimmed = name.trim();
  return trimmed.length > 48 ? `${trimmed.slice(0, 26)}…${trimmed.slice(-16)}` : trimmed;
}

function syncActionBar() {
  const image = shortName(el.imageLabel?.textContent ?? '');
  const hasImage = Boolean(image) && !image.startsWith('拖入镜像');
  el.actionImage.textContent = hasImage ? image : '未选择镜像';
  el.dropzone?.classList.toggle('has-file', hasImage);
  if (el.dropzoneSub) {
    el.dropzoneSub.textContent = hasImage ? '点击可更换镜像' : '支持 update.img 与官方 .zip';
  }

  const card = document.querySelector('.device-card.selected');
  const title = card?.querySelector('.device-title')?.textContent?.trim() ?? '';
  const tag = card?.querySelector('.device-tag')?.textContent?.trim() ?? '';
  const mode = el.modeBadge?.textContent?.trim() ?? '';
  if (title) {
    el.actionDevice.textContent = tag ? `${tag} · ${title}` : title;
  } else {
    el.actionDevice.textContent = mode && mode !== '未连接' ? `设备：${mode}` : '未连接设备';
  }
}

const observer = new MutationObserver(syncActionBar);
for (const node of [el.imageLabel, el.devices, el.modeBadge]) {
  if (node) observer.observe(node, { childList: true, characterData: true, subtree: true });
}
syncActionBar();

// --- dropzone -------------------------------------------------------------

function acceptDrop(file) {
  if (!file) return;
  const transfer = new DataTransfer();
  transfer.items.add(file);
  el.fileImage.files = transfer.files;
  el.fileImage.dispatchEvent(new Event('change', { bubbles: true }));
}

if (el.dropzone) {
  const zone = el.dropzone;
  for (const type of ['dragenter', 'dragover']) {
    zone.addEventListener(type, (event) => {
      event.preventDefault();
      zone.dataset.drag = 'true';
    });
  }
  for (const type of ['dragleave', 'dragend']) {
    zone.addEventListener(type, () => delete zone.dataset.drag);
  }
  zone.addEventListener('drop', (event) => {
    event.preventDefault();
    delete zone.dataset.drag;
    acceptDrop(event.dataTransfer?.files?.[0] ?? null);
  });
  // A <label for> already opens the picker on click; the keyboard path needs
  // its own handler because a label is not a button.
  zone.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      el.fileImage.click();
    }
  });
}

export { setDrawer, syncActionBar };

// --- write progress tone --------------------------------------------------
//
// A finished write keeps the bar at 100%; leaving it brand-red reads like an
// error. Turn it green the moment it completes, back to brand on a retry.

const writeBar = $('write-bar');
const writeProgress = $('write-progress');
if (writeBar && writeProgress) {
  const syncTone = () => {
    const width = parseFloat(writeBar.style.width || '0');
    writeProgress.classList.toggle('complete', width >= 100);
  };
  new MutationObserver(syncTone).observe(writeBar, { attributes: true, attributeFilter: ['style'] });
  syncTone();
}
