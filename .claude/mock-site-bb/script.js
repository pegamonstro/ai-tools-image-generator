// BIKINI CLUB — all cropping, typing and cycling driven by generated atlas sidecars.
(function () {
  "use strict";

  // ---------- nine-slice tooltip ----------
  const tipper = document.getElementById("tipper");
  let tip = null;
  if (tipper) {
    tipper.addEventListener("mouseenter", function () {
      tip = document.createElement("div");
      tip.className = "nslice nslice-tip";
      tip.style.cssText = "position:fixed;z-index:130;padding:8px 16px;font-size:13px;color:#cfffe2;max-width:340px";
      tip.textContent = "a 192×48 nine-slice frame — the punched middle grows to any text";
      document.body.appendChild(tip);
      const r = tipper.getBoundingClientRect();
      tip.style.left = r.left + "px";
      tip.style.top = r.top - 72 + "px";
    });
    tipper.addEventListener("mouseleave", function () { if (tip) tip.remove(); tip = null; });
  }

  // ---------- typewriter terminal ----------
  const termText = document.getElementById("term-text");
  const LINES = [
    "$ ssh guest@bikini.club",
    "password: ………………  accepted",
    "welcome back, smith of signals.",
    "$ ls /signals",
    "handshake.png   neon.wav   manifesto.txt",
    "$ cat manifesto.txt",
    "\"the wire never sleeps. neither do the smiths of signal.\"",
    "$ node --grid relay",
    "tracing copper … beacon stands cold …",
    "$ render --seed 5003",
    "▉▉▉▉▉▉▉▉▉▉  the club hums."
  ];
  let li = 0, ci = 0, out = [];
  function tick() {
    if (li >= LINES.length) {
      setTimeout(function () { li = 0; ci = 0; out = []; termText.textContent = ""; tick(); }, 6000);
      return;
    }
    const line = LINES[li];
    if (ci <= line.length) {
      termText.textContent = out.join("\n") + (out.length ? "\n" : "") + line.slice(0, ci);
      ci++;
      setTimeout(tick, line.startsWith("$") ? 34 : 18);
    } else {
      out.push(line); li++; ci = 0;
      setTimeout(tick, 420);
    }
  }
  if (termText) tick();

  // ---------- meters: five fill states ----------
  document.querySelectorAll(".meter .mood-picker").forEach(function (group) {
    const meter = group.closest(".meter");
    const stack = meter.querySelector(".bar-stack");
    const prefix = stack.id.replace("-stack", "-fill-");
    group.querySelectorAll("button").forEach(function (btn) {
      btn.addEventListener("click", function () {
        group.querySelectorAll("button").forEach(function (b) { b.classList.remove("active"); });
        btn.classList.add("active");
        stack.querySelector(".fill").src = "assets/widgets/" + prefix + btn.dataset.state.padStart(3, "0") + ".png";
      });
    });
  });

  // ---------- atlas helpers ----------
  function loadAtlas(url, cb) { return fetch(url).then(r => r.json()).then(cb); }
  function frameOf(f) { return f.frame || f; }
  function wireStrips(stripId, sheetUrl, atlasUrl, capFn) {
    const strip = document.getElementById(stripId);
    const sheet = new Image();
    sheet.src = sheetUrl;
    Promise.all([loadAtlas(atlasUrl, function (atlas) { strip.dataset.frames = JSON.stringify(atlas); }),
      new Promise(r => { sheet.onload = r; })]).then(function () {
      const atlas = JSON.parse(strip.dataset.frames);
      const keys = Object.keys(atlas.frames);
      keys.forEach(function (key, i) {
        const f = frameOf(atlas.frames[key]);
        const fig = document.createElement("figure");
        fig.className = "badge";
        const img = document.createElement("img");
        img.src = sheet.src;
        img.style.cssText = "width:" + sheet.naturalWidth + "px;height:" + sheet.naturalHeight +
          "px;left:" + (-f.x) + "px;top:" + (-f.y) + "px";
        fig.appendChild(img);
        const cap = document.createElement("figcaption");
        cap.className = "cap";
        cap.textContent = capFn ? capFn(key, i) : "chip " + (i + 1);
        fig.appendChild(cap);
        strip.appendChild(fig);
      });
    });
  }

  // ---------- gadget shelf: chips strip + live LED cycling ----------
  wireStrips("chips-strip", "assets/widgets/chips-sheet.png", "assets/widgets/chips-atlas.json", function (k, i) {
    return ["IC", "cap", "res", "xtal"][i] || "chip";
  });
  wireStrips("honors-strip", "assets/widgets/chips-sheet.png", "assets/widgets/chips-atlas.json", function (k) {
    return "honor · " + k;
  });

  const ledHolder = document.getElementById("led-live");
  if (ledHolder) {
    const ledImg = document.getElementById("led-img");
    new Image().src = "assets/widgets/leds-sheet.png";
    fetch("assets/widgets/leds-atlas.json").then(r => r.json()).then(function (atlas) {
      const flat = Object.values(atlas.frames).map(frameOf).sort((a, b) => a.x - b.x);
      const sheet = new Image();
      sheet.src = "assets/widgets/leds-sheet.png";
      let idx = 0;
      function show() {
        const f = flat[idx % flat.length];
        if (sheet.complete && f) {
          ledHolder.style.width = f.w + "px";
          ledHolder.style.height = f.h + "px";
          ledImg.style.cssText = "width:" + sheet.naturalWidth + "px;height:" + sheet.naturalHeight +
            "px;left:" + (-f.x) + "px;top:" + (-f.y) + "px;position:relative;image-rendering:pixelated";
        }
        idx++;
      }
      sheet.onload = function () {
        show();
        setInterval(show, 700);
      };
    });
  }

  // ---------- variants sheets: crop mood cells ----------
  function wireVariants(card) {
    const img = card.querySelector('.cell img[data-atlas]');
    const picker = card.querySelector(".mood-picker");
    loadAtlas(img.dataset.atlas, function (atlas) {
      const flat = Object.values(atlas.frames).map(frameOf).sort((a, b) => a.x - b.x);
      const sheet = new Image();
      sheet.src = img.src;
      function show(i) {
        const f = flat[i]; if (!f) return;
        function paint() {
          const sc = 140 / Math.max(f.w, f.h);
          img.style.display = "block";
          img.style.width = sheet.naturalWidth * sc + "px";
          img.style.height = sheet.naturalHeight * sc + "px";
          img.style.left = (-f.x * sc) + "px";
          img.style.top = (-f.y * sc) + "px";
        }
        if (sheet.complete) paint(); else sheet.onload = paint;
      }
      const initial = card.querySelector('[data-frame]') || picker.querySelector('[data-frame]');
      if (initial) show(parseInt(initial.dataset.frame || "0", 10));
      picker.querySelectorAll("button").forEach(function (btn) {
        btn.addEventListener("click", function () {
          picker.querySelectorAll("button").forEach(b => b.classList.remove("active"));
          btn.classList.add("active");
          show(parseInt(btn.dataset.frame, 10));
        });
      });
    });
  }
  document.querySelectorAll(".crew-card").forEach(function (card) {
    if (card.querySelector('.cell img[data-atlas]') && card.querySelector('[data-frame]')) wireVariants(card);
  });

  // ---------- runner walk4 ----------
  const runnerImg = document.querySelector('[data-npc="runner"] .cell img');
  const rPicker = document.querySelector('[data-npc="runner"] .mood-picker');
  const DIRS = ["down", "left", "right", "up"];
  const runCells = {};
  DIRS.forEach(function (d) {
    const im = new Image();
    im.src = 'assets/crew/runner-' + d + '.png';
    runCells[d] = im;
  });
  function paintRunner(im) {
    const sc = 140 / im.naturalHeight;
    runnerImg.src = im.src;
    runnerImg.style.display = "block";
    runnerImg.style.width = im.naturalWidth * sc + "px";
    runnerImg.style.height = im.naturalHeight * sc + "px";
    runnerImg.style.left = "0";
    runnerImg.style.top = "0";
  }
  const down = runCells.down;
  down.onload = function () { paintRunner(down); };
  if (down.complete) down.onload();
  const rStrip = document.getElementById("runner-walk");
  DIRS.forEach(function (d) {
    const im = new Image();
    im.src = 'assets/crew/runner-' + d + '.png';
    im.title = "runner " + d;
    rStrip.appendChild(im);
  });
  rPicker.querySelectorAll("button").forEach(function (btn) {
    btn.addEventListener("click", function () {
      rPicker.querySelectorAll("button").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      paintRunner(runCells[DIRS[parseInt(btn.dataset.dir, 10)] || "down"]);
    });
  });

  // ---------- node grid: 20-class circuit wang map ----------
  const canvas = document.getElementById("tilemap-canvas");
  const ctx = canvas.getContext("2d");
  ctx.imageSmoothingEnabled = false;
  const T = 64;
  const map = [
    ["corner-out-nw", "line-h", "line-h", "corner-out-ne", "single", "line-h", "line-h", "single"],
    ["line-v", "single", "single", "line-v", "single", "single", "line-v", "single"],
    ["line-v", "single", "t-e", "line-h", "t-w", "line-v", "single", "cap-w"],
    ["line-v", "single", "corner-in-sw", "line-h", "corner-in-se", "line-v", "single", "single"],
    ["corner-out-sw", "line-h", "line-h", "line-h", "corner-out-se", "line-v", "single", "single"],
    ["single", "single", "single", "single", "cap-s", "line-v", "single", "single"]
  ];
  const tileCache = {};
  function tile(cls) {
    return new Promise(function (res) {
      if (tileCache[cls]) return res(tileCache[cls]);
      const im = new Image();
      im.onload = function () { tileCache[cls] = im; res(im); };
      im.onerror = function () { res(null); };
      im.src = "assets/board/circuit-" + cls + ".png";
    });
  }
  const flat = [];
  map.forEach(function (row, y) {
    row.forEach(function (cls, x) {
      flat.push(tile(cls).then(function (im) {
        if (!im) return tile("single");
        return im;
      }).then(function (im) {
        if (im) ctx.drawImage(im, 0, 0, im.naturalWidth, im.naturalHeight, x * T, y * T, T, T);
      }));
    });
  });
  Promise.all(flat);
})();