(function () {
  const main = document.querySelector('main[data-session-id]');
  if (!main) return;
  const sessionID = main.dataset.sessionId;
  const img = document.getElementById('latest-frame');
  const sCap = document.getElementById('status-capturing');
  const sRun = document.getElementById('status-frames-run');
  const sTot = document.getElementById('status-frames-total');
  const sAt = document.getElementById('status-last-at');
  const compileEl = document.getElementById('compile-status');

  let lastFrameSeen = parseInt(sTot ? sTot.textContent : '0', 10) || 0;

  function fmt(ts) {
    if (!ts) return '—';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return '—';
    return d.toLocaleString();
  }

  async function tick() {
    try {
      const r = await fetch('/sessions/' + encodeURIComponent(sessionID) + '/status.json', { cache: 'no-store' });
      if (!r.ok) return;
      const s = await r.json();
      if (sCap) sCap.textContent = s.capturing ? 'yes' : 'no';
      if (sRun) sRun.textContent = s.frames_this_run;
      if (sTot) sTot.textContent = s.last_frame_number;
      if (sAt) sAt.textContent = fmt(s.last_frame_at);
      if (compileEl) {
        if (s.compile && s.compile.running) {
          compileEl.textContent = 'Compile in progress (started ' + fmt(s.compile.started_at) + ')…';
          compileEl.className = 'muted';
        } else if (s.compile && s.compile.last_error) {
          compileEl.textContent = 'Compile error: ' + s.compile.last_error;
          compileEl.className = 'error';
        } else if (s.compile && s.compile.output) {
          const enc = s.compile.encoder ? ' (' + s.compile.encoder + ')' : '';
          compileEl.textContent = 'Last compile: ' + s.compile.output + enc;
          compileEl.className = 'muted';
        }
      }
      if (img && s.last_frame_number > lastFrameSeen) {
        lastFrameSeen = s.last_frame_number;
        img.src = '/sessions/' + encodeURIComponent(sessionID) + '/latest.jpg?ts=' + Date.now();
      }
    } catch (e) {
      // ignore network blips
    }
  }

  tick();
  setInterval(tick, 2000);
})();

(function () {
  const startBtn = document.getElementById('vf-start');
  if (!startBtn) return;
  const stopBtn = document.getElementById('vf-stop');
  const vfImg = document.getElementById('vf-img');
  const vfErr = document.getElementById('vf-err');

  let running = false;

  function showRunning() {
    running = true;
    startBtn.hidden = true;
    stopBtn.hidden = false;
    vfImg.hidden = false;
    vfErr.hidden = true;
    vfErr.textContent = '';
  }

  function showStopped() {
    running = false;
    startBtn.hidden = false;
    stopBtn.hidden = true;
    vfImg.hidden = true;
    vfImg.removeAttribute('src');
  }

  function showError(msg) {
    vfErr.textContent = msg;
    vfErr.hidden = false;
  }

  startBtn.addEventListener('click', () => {
    showRunning();
    vfImg.src = '/viewfinder.mjpeg?ts=' + Date.now();
  });

  stopBtn.addEventListener('click', async () => {
    showStopped();
    try {
      await fetch('/viewfinder/stop', { method: 'POST' });
    } catch (e) {
      // ignore — context cancel via disconnect already handled server-side
    }
  });

  vfImg.addEventListener('error', () => {
    if (running) {
      showError('Viewfinder failed — camera may be busy or already in use.');
      showStopped();
    }
  });

  async function pollStatus() {
    try {
      const r = await fetch('/viewfinder/status.json', { cache: 'no-store' });
      if (!r.ok) return;
      const s = await r.json();
      if (running && !s.running) {
        // Server killed the stream (likely capture started elsewhere).
        showStopped();
        if (s.capturing) {
          showError('Camera was taken over by a capture session.');
        }
      }
    } catch (e) {
      // ignore
    }
  }
  setInterval(pollStatus, 2000);

  window.addEventListener('beforeunload', () => {
    if (running) navigator.sendBeacon('/viewfinder/stop');
  });
})();
