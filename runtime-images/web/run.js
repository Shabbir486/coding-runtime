// Web (HTML/CSS/JS) runner — Option A (jsdom, no real browser).
//
// Loads a combined HTML document (inline <style>/<script>) into a simulated DOM
// and forwards console output to stdout/stderr. No visual rendering and no
// network (external <script src>/CSS won't load).
//
// Assessment model:
//   - source_code (index.html): the candidate's page/functions.
//   - stdin: the grader's test code (e.g. QUnit tests referencing the page's
//     functions). Executed in the SAME window after the page loads.
//
// QUnit is bundled and auto-injected as a global whenever the page OR the stdin
// tests reference `QUnit` — no CDN needed. The runner starts QUnit only after
// injecting the stdin tests, then waits for QUnit.done.
'use strict';

const fs = require('fs');
const path = require('path');
const { JSDOM, VirtualConsole } = require('jsdom');

const file = process.argv[2] || 'index.html';
const GRACE_MS = 300;           // async settle for plain pages
const QUNIT_TIMEOUT_MS = 15000; // hard cap waiting for QUnit.done
const SETTLE_MS = 60;           // let the page's own scripts/load run first

function readFileSafe(f) {
  try { return fs.readFileSync(f, 'utf8'); } catch (_) { return null; }
}

const html0 = readFileSafe(file);
if (html0 === null) {
  console.error(`web-runner: cannot read ${file}`);
  process.exit(1);
}

// Grader's test code arrives on STDIN (empty for self-contained pages).
let stdinCode = '';
try { stdinCode = fs.readFileSync(0, 'utf8'); } catch (_) { stdinCode = ''; }

const vc = new VirtualConsole();
["log", "info", "debug", "dir"].forEach((m) => vc.on(m, (...a) => console.log(...a)));
["warn", "error"].forEach((m) => vc.on(m, (...a) => console.error(...a)));
vc.on("jsdomError", (e) => console.error(e && e.message ? e.message : String(e)));

const usesQUnit = /QUnit/.test(html0) || /QUnit/.test(stdinCode);

let html = html0;
if (usesQUnit) {
  const qunitSrc = readFileSafe(path.join(__dirname, "node_modules/qunit/qunit/qunit.js")) || "";
  const reporter = `
    QUnit.config.autostart = false; // started manually after stdin tests are injected
    // Failure details go to STDERR only; STDOUT stays a single PASS/FAIL verdict.
    QUnit.log(function (d) {
      if (!d.result) {
        console.error((d.module ? d.module + ': ' : '') + (d.name ? d.name + ': ' : '') +
          (d.message || 'assertion failed') +
          ' | expected=' + JSON.stringify(d.expected) +
          ' actual=' + JSON.stringify(d.actual) +
          (d.source ? '\\n' + d.source : ''));
      }
    });
    QUnit.done(function (d) {
      console.log(d.failed === 0 ? 'PASS' : 'FAIL'); // STDOUT: verdict only
      window.__QUNIT_DONE__ = d;
    });
  `;
  const boot = `<script>${qunitSrc}</script><script>${reporter}</script>`;
  if (/<head[^>]*>/i.test(html)) html = html.replace(/<head[^>]*>/i, (m) => m + boot);
  else if (/<html[^>]*>/i.test(html)) html = html.replace(/<html[^>]*>/i, (m) => m + "<head>" + boot + "</head>");
  else html = boot + html;
}

let dom;
try {
  dom = new JSDOM(html, {
    runScripts: "dangerously",
    pretendToBeVisual: true,
    virtualConsole: vc,
    url: "http://localhost/",
  });
} catch (e) {
  console.error(`web-runner: failed to evaluate document: ${e.message}`);
  process.exit(1);
}

function finish() {
  try { dom.window.close(); } catch (_) { /* ignore */ }
  process.exit(0);
}

// After the page's own scripts + load have run, inject the grader's test code
// (STDIN) into the same window (so it can call the page's functions), then start
// QUnit if used.
setTimeout(() => {
  if (stdinCode.trim()) {
    try {
      dom.window.eval(stdinCode);
    } catch (e) {
      console.error("web-runner: error running test code from stdin: " + e.message);
    }
  }

  if (usesQUnit) {
    try { dom.window.QUnit.start(); } catch (_) { /* may already have started */ }
    const deadline = Date.now() + QUNIT_TIMEOUT_MS;
    const poll = setInterval(() => {
      if (dom.window.__QUNIT_DONE__) {
        clearInterval(poll);
        finish();
      } else if (Date.now() > deadline) {
        clearInterval(poll);
        console.error("web-runner: QUnit did not finish within time limit");
        finish();
      }
    }, 50);
  } else {
    setTimeout(finish, GRACE_MS);
  }
}, SETTLE_MS);
