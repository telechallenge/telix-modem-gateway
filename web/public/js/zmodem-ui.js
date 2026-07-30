// UI layer for ZMODEM transfers. Pure DOM binding — protocol logic lives in
// zmodem-sentry.js. Exposes window.ZmodemUI with these methods:
//   startXfer({ direction, filename, size, batchIndex, batchTotal })
//   updateXfer({ bytes, cps })
//   endXfer({ status })                // status: 'done' | 'aborted' | 'timeout'
//   surfaceDownload({ filename, blob })
//   surfaceError(message)
//   promptUpload(maxBytes) -> Promise<File[]>

(function () {
  const $ = id => document.getElementById(id);
  const strip = $('xferStrip');
  const dir = $('xferDir');
  const name = $('xferName');
  const pct = $('xferPct');
  const fill = $('xferFill');
  const cpsEl = $('xferCps');
  const etaEl = $('xferEta');
  const batchEl = $('xferBatch');
  const notifications = $('xferNotifications');
  const uploadInput = $('xferUploadInput');

  let currentSize = 0;
  let startedAt = 0;
  let hideTimer = 0;

  function scheduleHide(ms) {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => { strip.hidden = true; }, ms);
  }

  function startXfer({ direction, filename, size, batchIndex, batchTotal }) {
    clearTimeout(hideTimer);
    strip.hidden = false;
    strip.classList.remove('state-aborted', 'state-timeout');
    dir.textContent = direction === 'send' ? '▲' : '▶';
    name.textContent = window.XferUtil.sanitizeFilename(filename);
    currentSize = size || 0;
    startedAt = performance.now();
    pct.textContent = '0%';
    fill.style.width = '0%';
    cpsEl.textContent = '0 CPS';
    etaEl.textContent = '--:--';
    if (batchTotal && batchTotal > 1) {
      batchEl.hidden = false;
      batchEl.textContent = `${batchIndex}/${batchTotal}`;
    } else {
      batchEl.hidden = true;
    }
  }

  function updateXfer({ bytes, cps }) {
    const percent = currentSize > 0 ? Math.floor((bytes / currentSize) * 100) : 0;
    pct.textContent = percent + '%';
    fill.style.width = Math.min(100, percent) + '%';
    cpsEl.textContent = (cps || 0).toFixed(0) + ' CPS';
    if (cps > 0 && currentSize > bytes) {
      const remaining = (currentSize - bytes) / cps;
      etaEl.textContent = window.XferUtil.formatDuration(remaining);
    }
  }

  function endXfer({ status }) {
    if (status === 'done') {
      pct.textContent = '100%';
      fill.style.width = '100%';
      etaEl.textContent = '00:00';
      scheduleHide(800);
    } else if (status === 'aborted') {
      strip.classList.add('state-aborted');
      name.textContent = '✕ ABORTED';
      scheduleHide(2000);
    } else if (status === 'timeout') {
      strip.classList.add('state-timeout');
      name.textContent = '✕ TIMEOUT';
      scheduleHide(2000);
    }
  }

  function surfaceDownload({ filename, blob }) {
    const safeName = window.XferUtil.sanitizeFilename(filename);
    const size = window.XferUtil.formatBytes(blob.size);
    const bubble = document.createElement('div');
    bubble.className = 'xfer-notification';

    const label = document.createElement('span');
    label.textContent = `⬇ ${safeName}  (${size})`;
    label.style.flex = '1';

    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Save';

    let objectUrl = URL.createObjectURL(blob);
    let removed = false;
    function dismiss() {
      if (removed) return;
      removed = true;
      URL.revokeObjectURL(objectUrl);
      bubble.remove();
    }

    button.addEventListener('click', () => {
      const a = document.createElement('a');
      a.href = objectUrl;
      a.download = safeName;
      document.body.appendChild(a);
      a.click();
      a.remove();
      dismiss();
    });

    bubble.appendChild(label);
    bubble.appendChild(button);
    notifications.appendChild(bubble);

    // Auto-dismiss after 60 s.
    setTimeout(dismiss, 60_000);
  }

  function surfaceError(message) {
    const bubble = document.createElement('div');
    bubble.className = 'xfer-notification error';
    bubble.textContent = '✕ ' + message;
    notifications.appendChild(bubble);
    setTimeout(() => bubble.remove(), 5000);
  }

  function promptUpload(maxBytes) {
    // A file <input> only opens its picker during transient user activation.
    // This runs from a WebSocket frame (the BBS's ZRINIT), which has none, so
    // calling uploadInput.click() directly is silently blocked and no dialog
    // appears. Surface a button instead and open the picker from its click —
    // the same gesture-carrying path the download "Save" button uses.
    return new Promise(resolve => {
      let settled = false;
      function finish(files) {
        if (settled) return;
        settled = true;
        uploadInput.removeEventListener('change', onChange);
        bubble.remove();
        resolve(files);
      }

      const onChange = () => {
        const files = Array.from(uploadInput.files || []);
        uploadInput.value = ''; // reset so the same file can be re-picked
        if (files.length === 0) return; // native cancel — leave the prompt up
        const oversize = files.find(f => f.size > maxBytes);
        if (oversize) {
          surfaceError(`${oversize.name}: exceeds ${window.XferUtil.formatBytes(maxBytes)}`);
          finish([]);
        } else {
          finish(files);
        }
      };
      uploadInput.addEventListener('change', onChange);

      const bubble = document.createElement('div');
      bubble.className = 'xfer-notification';

      const label = document.createElement('span');
      label.textContent = '⬆ BBS ready — choose file(s) to upload';
      label.style.flex = '1';

      const chooseBtn = document.createElement('button');
      chooseBtn.type = 'button';
      chooseBtn.textContent = 'Choose files';
      chooseBtn.addEventListener('click', () => uploadInput.click()); // user gesture

      const cancelBtn = document.createElement('button');
      cancelBtn.type = 'button';
      cancelBtn.textContent = 'Cancel';
      cancelBtn.addEventListener('click', () => finish([]));

      bubble.appendChild(label);
      bubble.appendChild(chooseBtn);
      bubble.appendChild(cancelBtn);
      notifications.appendChild(bubble);
    });
  }

  window.ZmodemUI = {
    startXfer, updateXfer, endXfer,
    surfaceDownload, surfaceError, promptUpload,
  };
})();
