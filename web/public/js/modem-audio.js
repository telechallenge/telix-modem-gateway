// ModemAudio — synthesized telephony tones + pre-recorded handshake
// States: IDLE → DIALING → RINGING → HANDSHAKE → IDLE
const ModemAudio = (() => {
  const DTMF_FREQS = {
    '1': [697, 1209], '2': [697, 1336], '3': [697, 1477],
    '4': [770, 1209], '5': [770, 1336], '6': [770, 1477],
    '7': [852, 1209], '8': [852, 1336], '9': [852, 1477],
    '*': [941, 1209], '0': [941, 1336], '#': [941, 1477],
  };

  let ctx = null;
  let gainNode = null;
  let activeOscs = [];
  let activeTimers = [];
  let handshakeAudio = null;
  let handshakeLoaded = false;
  let state = 'IDLE';
  let hasHeardRing = false;
  let muted = localStorage.getItem('modemAudioMuted') === 'true';

  function ensureContext() {
    if (!ctx) {
      ctx = new (window.AudioContext || window.webkitAudioContext)();
      gainNode = ctx.createGain();
      gainNode.gain.value = muted ? 0 : 0.3;
      gainNode.connect(ctx.destination);
      loadHandshake();
    }
    if (ctx.state === 'suspended') ctx.resume();
    return ctx;
  }

  function loadHandshake() {
    if (handshakeLoaded) return;
    handshakeLoaded = true;
    fetch('audio/modem-handshake.mp3')
      .then(r => { if (r.ok) return r.arrayBuffer(); throw new Error('not found'); })
      .then(buf => ctx.decodeAudioData(buf))
      .then(decoded => { handshakeAudio = decoded; })
      .catch(() => { /* no handshake audio available — synthesized fallback below */ });
  }

  function stopOscs() {
    activeOscs.forEach(o => { try { o.stop(); } catch (e) {} });
    activeOscs = [];
  }

  function clearTimers() {
    activeTimers.forEach(id => clearTimeout(id));
    activeTimers = [];
  }

  function playTone(freqs, duration) {
    const c = ensureContext();
    const oscs = freqs.map(f => {
      const osc = c.createOscillator();
      osc.type = 'sine';
      osc.frequency.value = f;
      osc.connect(gainNode);
      osc.start();
      activeOscs.push(osc);
      return osc;
    });
    if (duration > 0) {
      const t = setTimeout(() => {
        oscs.forEach(o => { try { o.stop(); } catch (e) {} });
        activeOscs = activeOscs.filter(o => !oscs.includes(o));
      }, duration);
      activeTimers.push(t);
    }
    return oscs;
  }

  function playClick() {
    const c = ensureContext();
    const bufSize = c.sampleRate * 0.05;
    const buf = c.createBuffer(1, bufSize, c.sampleRate);
    const data = buf.getChannelData(0);
    for (let i = 0; i < bufSize; i++) {
      data[i] = (Math.random() * 2 - 1) * (1 - i / bufSize);
    }
    const src = c.createBufferSource();
    src.buffer = buf;
    const clickGain = c.createGain();
    clickGain.gain.value = 0.15;
    clickGain.connect(gainNode);
    src.connect(clickGain);
    src.start();
  }

  function playDialTone() {
    return playTone([350, 440], 0); // continuous until stopped
  }

  function playDTMFSequence(digits, callback) {
    let i = 0;
    function next() {
      if (i >= digits.length) {
        if (callback) callback();
        return;
      }
      const freqs = DTMF_FREQS[digits[i]];
      if (freqs) {
        playTone(freqs, 120);
        i++;
        const t = setTimeout(next, 180);
        activeTimers.push(t);
      } else {
        i++;
        next();
      }
    }
    next();
  }

  let ringOscs = [];
  let ringTimer = null;

  function startRingCadence() {
    stopRingCadence();
    function ringOn() {
      ringOscs = [440, 480].map(f => {
        const c = ensureContext();
        const osc = c.createOscillator();
        osc.type = 'sine';
        osc.frequency.value = f;
        osc.connect(gainNode);
        osc.start();
        return osc;
      });
      ringTimer = setTimeout(() => {
        ringOscs.forEach(o => { try { o.stop(); } catch (e) {} });
        ringOscs = [];
        ringTimer = setTimeout(ringOn, 4000);
      }, 2000);
    }
    ringOn();
  }

  function stopRingCadence() {
    if (ringTimer) { clearTimeout(ringTimer); ringTimer = null; }
    ringOscs.forEach(o => { try { o.stop(); } catch (e) {} });
    ringOscs = [];
  }

  let busyTimer = null;
  let busyOscs = [];

  function playBusyTone(durationMs) {
    stopBusyTone();
    const dur = durationMs || 3000;
    let elapsed = 0;

    function busyOn() {
      if (elapsed >= dur) { stopBusyTone(); return; }
      busyOscs = [480, 620].map(f => {
        const c = ensureContext();
        const osc = c.createOscillator();
        osc.type = 'sine';
        osc.frequency.value = f;
        osc.connect(gainNode);
        osc.start();
        return osc;
      });
      busyTimer = setTimeout(() => {
        busyOscs.forEach(o => { try { o.stop(); } catch (e) {} });
        busyOscs = [];
        elapsed += 500;
        busyTimer = setTimeout(() => { elapsed += 500; busyOn(); }, 500);
      }, 500);
    }
    busyOn();
  }

  function stopBusyTone() {
    if (busyTimer) { clearTimeout(busyTimer); busyTimer = null; }
    busyOscs.forEach(o => { try { o.stop(); } catch (e) {} });
    busyOscs = [];
  }

  function playSITTones(callback) {
    // Special Information Tones: three ascending tones
    const tones = [
      { freq: 913.8, dur: 274 },
      { freq: 1370.6, dur: 274 },
      { freq: 1776.7, dur: 380 },
    ];
    let i = 0;
    function next() {
      if (i >= tones.length) {
        if (callback) callback();
        return;
      }
      const { freq, dur } = tones[i];
      playTone([freq], dur);
      i++;
      const t = setTimeout(next, dur + 30);
      activeTimers.push(t);
    }
    next();
  }

  function playHandshake() {
    if (handshakeAudio && ctx) {
      const src = ctx.createBufferSource();
      src.buffer = handshakeAudio;
      src.connect(gainNode);
      src.start();
      activeOscs.push(src);
    } else {
      // Synthesized fallback: noisy screech
      synthesizeHandshake();
    }
  }

  function synthesizeHandshake() {
    const c = ensureContext();
    const duration = 3;
    const sampleRate = c.sampleRate;
    const bufSize = sampleRate * duration;
    const buf = c.createBuffer(1, bufSize, sampleRate);
    const data = buf.getChannelData(0);

    // Mix of carrier tones and noise to approximate handshake
    for (let i = 0; i < bufSize; i++) {
      const t = i / sampleRate;
      const carrier1 = Math.sin(2 * Math.PI * 1200 * t) * 0.3;
      const carrier2 = Math.sin(2 * Math.PI * 2400 * t) * 0.2;
      const sweep = Math.sin(2 * Math.PI * (1000 + 1500 * t / duration) * t) * 0.15;
      const noise = (Math.random() * 2 - 1) * 0.1;
      data[i] = (carrier1 + carrier2 + sweep + noise) * (1 - t / (duration + 1));
    }

    const src = c.createBufferSource();
    src.buffer = buf;
    src.connect(gainNode);
    src.start();
    activeOscs.push(src);
  }

  // Public API
  return {
    dialNumber(number) {
      this.stopAll();
      state = 'DIALING';
      hasHeardRing = false;
      ensureContext();

      const digits = number.replace(/[^0-9*#]/g, '');

      playClick();

      // Dial tone for ~500ms, then DTMF
      const dialToneOscs = playDialTone();
      const t = setTimeout(() => {
        dialToneOscs.forEach(o => { try { o.stop(); } catch (e) {} });
        activeOscs = activeOscs.filter(o => !dialToneOscs.includes(o));
        playDTMFSequence(digits, () => {
          // Done dialing — silence while waiting for server response
        });
      }, 500);
      activeTimers.push(t);
    },

    onRing() {
      hasHeardRing = true;
      state = 'RINGING';
      stopOscs();
      clearTimers();
      startRingCadence();
    },

    onConnect() {
      stopRingCadence();
      stopOscs();
      clearTimers();
      state = 'HANDSHAKE';
      playHandshake();
      // Transition to IDLE after handshake plays
      const t = setTimeout(() => { state = 'IDLE'; }, handshakeAudio ? handshakeAudio.duration * 1000 : 3000);
      activeTimers.push(t);
    },

    onBusy() {
      stopRingCadence();
      stopOscs();
      clearTimers();
      playBusyTone(3000);
      const t = setTimeout(() => { stopBusyTone(); state = 'IDLE'; }, 3500);
      activeTimers.push(t);
    },

    onFailure() {
      stopRingCadence();
      stopOscs();
      clearTimers();
      if (!hasHeardRing) {
        // Invalid number — play SIT intercept tones
        playSITTones(() => { state = 'IDLE'; });
      } else {
        state = 'IDLE';
      }
    },

    stopAll() {
      stopRingCadence();
      stopBusyTone();
      stopOscs();
      clearTimers();
      state = 'IDLE';
      hasHeardRing = false;
    },

    setMuted(val) {
      muted = !!val;
      localStorage.setItem('modemAudioMuted', muted);
      if (gainNode) gainNode.gain.value = muted ? 0 : 0.3;
    },

    isMuted() {
      return muted;
    },

    // Call on first user interaction to init AudioContext
    initOnGesture() {
      if (!ctx) ensureContext();
    },

    getState() { return state; },
  };
})();
