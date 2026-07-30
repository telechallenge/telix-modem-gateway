// Extracts Hayes result codes from the inbound byte stream. Kept separate from
// app.js so it can be unit-tested without a DOM.
//
// The gateway emits each result code as its own CR/LF-delimited line, but it
// does not get a chunk to itself: the kernel coalesces a result code with
// whatever payload follows it, so CONNECT arrives glued to the BBS banner and
// NO CARRIER arrives glued to the goodbye screen. Matching whole lines finds
// the code anywhere in such a chunk while still refusing BBS prose that merely
// contains the words ("STRING", "CONNECTED TO NODE 3", "NO CARRIER means...").
//
// CONNECT takes an optional " <speed>/<suffix>" argument (modem.go X0 vs X1+);
// every other code stands alone on its line.

const RESULT_LINE =
  /(?:^|[\r\n])(?:(CONNECT)(?: [^\r\n]*)?|(RING|BUSY|NO CARRIER|NO ANSWER|NO DIALTONE))(?=[\r\n]|$)/g;

function parseResultCodes(text) {
  const codes = [];
  for (const m of text.matchAll(RESULT_LINE)) {
    codes.push(m[1] || m[2]);
  }
  return codes;
}

if (typeof window !== 'undefined') {
  window.ModemState = { parseResultCodes };
}
