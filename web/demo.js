// Replays a ferry session in the two terminals on the landing page, and
// sails the boat across the strait in step with the progress bar.
// The lines are ferry's real output; only the timing is compressed.
(function () {
	"use strict";

	var L = document.getElementById("left");
	var R = document.getElementById("right");
	if (!L || !R) return;

	var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
	var run = 0;
	var SVG = "http://www.w3.org/2000/svg";

	function esc(s) {
		return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
			// A number and its unit stay together when a line wraps.
			.replace(/(\d) (GB|MB\/s)/g, "$1\u00a0$2");
	}

	// A line is [cls, text] pairs; cls is a span class or "".
	function html(parts) {
		return parts.map(function (p) {
			return p[0] ? '<span class="' + p[0] + '">' + esc(p[1]) + "</span>" : esc(p[1]);
		}).join("");
	}

	function Term(el) {
		this.el = el;
		this.lines = [];
		this.live = "";
	}
	Term.prototype.draw = function (cursor) {
		var s = this.lines.join("\n");
		if (this.live) s += (s ? "\n" : "") + this.live;
		if (cursor) s += '<span class="cur"></span>';
		this.el.innerHTML = s;
	};
	Term.prototype.print = function (parts) { this.lines.push(html(parts)); this.live = ""; };
	Term.prototype.clear = function () { this.lines = []; this.live = ""; };

	// ---- the strait ----

	// Two drawings of the same crossing, one wide and one for phones; each
	// carries its own geometry in data attributes and both move together.
	var strait = document.getElementById("strait");
	var status = document.getElementById("status");
	var flagClass = ["f-yellow", "f-red", "f-blue"];
	var scenes = Array.prototype.map.call(document.querySelectorAll(".scene"), function (svg) {
		var d = svg.dataset;
		return {
			boat: svg.querySelector(".boat"), wake: svg.querySelector(".wake"), crate: svg.querySelector(".crate"),
			hoists: { l: svg.querySelector(".hoist-l"), r: svg.querySelector(".hoist-r") },
			from: +d.from, to: +d.to, dy: +d.dy, wakeY: +d.wake,
			mast: { l: +d.mastL, r: +d.mastR }, top: +d.top, slope: +d.slope, gap: +d.gap, joined: !!d.joined
		};
	});

	function sail(pct) {
		scenes.forEach(function (s) {
			var x = s.from + (s.to - s.from) * pct / 100;
			s.boat.setAttribute("transform", "translate(" + x.toFixed(1) + " " + s.dy + ")");
			s.wake.setAttribute("d", "M" + s.from + " " + s.wakeY + " L" + x.toFixed(1) + " " + s.wakeY);
		});
		if (strait) strait.classList.toggle("moving", pct > 0 && pct < 100);
	}

	function say(text) { if (status) status.textContent = text; }

	// hoist strings words as pennants on the line from a shore's mast. On the
	// narrow drawing the words go in one line under the flags.
	function hoist(side, words) {
		scenes.forEach(function (s) {
			var h = s.hoists[side];
			var g = h.querySelector(".flags");
			while (g.firstChild) g.removeChild(g.firstChild);
			var x0 = s.mast[side], dir = side === "l" ? 1 : -1, lowest = 0;
			words.forEach(function (w, i) {
				var x = x0 + dir * (s.gap * 0.65 + i * s.gap), y = s.top + (x - x0) * dir * s.slope;
				var f = document.createElementNS(SVG, "path");
				f.setAttribute("class", flagClass[i % 3]);
				f.setAttribute("d", "M" + (x - 9) + " " + y + " L" + (x + 9) + " " + (y + 6.5) + " L" + x + " " + (y + 22) + " Z");
				g.appendChild(f);
				lowest = Math.max(lowest, y + 22);
				if (!s.joined) g.appendChild(label(x, y + 36, w, "middle"));
			});
			if (s.joined && words.length) {
				g.appendChild(label(x0 + dir * 4, lowest + 20, words.join(" · "), dir > 0 ? "start" : "end"));
			}
			h.classList.toggle("up", words.length > 0);
		});
	}

	function label(x, y, text, anchor) {
		var t = document.createElementNS(SVG, "text");
		t.setAttribute("x", x);
		t.setAttribute("y", y);
		t.setAttribute("text-anchor", anchor);
		t.textContent = text;
		return t;
	}

	function land(on) {
		scenes.forEach(function (s) { s.crate.classList.toggle("landed", on); });
	}

	function reset() {
		sail(0);
		hoist("l", []);
		hoist("r", []);
		land(false);
		say("at the pier");
	}

	// ---- the script ----

	var left = new Term(L), right = new Term(R);
	var code = "7-acorn-tulip-otter";
	var words = code.split("-").slice(1);
	var total = 2.1;

	// cols is how many characters fit on one line of a terminal.
	function cols(el) {
		var s = document.createElement("span");
		s.textContent = "0000000000";
		s.style.position = "absolute";
		s.style.visibility = "hidden";
		el.appendChild(s);
		var cw = s.getBoundingClientRect().width / 10;
		el.removeChild(s);
		var cs = getComputedStyle(el);
		var inner = el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
		return cw > 0 ? Math.floor(inner / cw) : 80;
	}

	// progress draws ferry's progress line the way ferry does on a terminal
	// this wide: the full line if it fits, else shorter ones (see ui.go).
	function progress(arrow, pct, width, full) {
		var done = (total * pct / 100).toFixed(1);
		var rate = (108 + Math.round(Math.sin(pct / 9) * 6)).toFixed(1);
		var left = Math.max(0, Math.round((100 - pct) / 100 * 19));
		var p = arrow + " " + ("    " + pct.toFixed(1)).slice(-5) + "%";
		var speed = pct < 100 ? "  " + rate + " MB/s" : "";
		var line = full ? p + "  " + done + " GB of " + total + " GB" + speed + (pct < 100 ? " · " + left + "s left" : "") : p;
		var w = 14;
		[p + "  " + done + " GB" + speed, p].forEach(function (short) {
			if (w + 1 + line.length >= width) { w = 8; line = short; }
		});
		var n = Math.round(pct / 100 * w);
		return [["g", "━".repeat(n)], ["d", "━".repeat(w - n)], ["", " " + line]];
	}

	// Steps: [delay ms, function].
	function script() {
		var s = [];
		function type(t, text, after) {
			s.push([500, function () { t.live = '<span class="p">$ </span>'; }]);
			for (var i = 1; i <= text.length; i++) {
				(function (k) { s.push([38, function () { t.live = '<span class="p">$ </span>' + esc(text.slice(0, k)); }]); })(i);
			}
			s.push([350, function () { t.print([["p", "$ "], ["", text]]); }]);
			if (after) s.push([0, after]);
		}
		function out(t, delay, parts) { s.push([delay, function () { t.print(parts); }]); }
		function then(delay, f) { s.push([delay, f]); }

		type(left, "ferry send ./holiday.mp4");
		out(left, 250, [["", "Sending holiday.mp4 (2.1 GB)."]]);
		out(left, 120, [["", "On the other machine, run:"]]);
		out(left, 60, [["", ""]]);
		out(left, 60, [["", "    ferry get "], ["a", code]]);
		then(0, function () { hoist("l", words); say("code hoisted"); });
		out(left, 60, [["", ""]]);
		out(left, 60, [["d", "Waiting for them… (Ctrl-C to cancel)"]]);

		type(right, "ferry get " + code, function () { hoist("r", words); say("meeting"); });
		out(right, 600, [["", "Connected. "], ["d", "(both sides see: motto sway)"]]);
		out(left, 0, [["", "Connected. "], ["d", "(both sides see: motto sway)"]]);
		then(0, function () { hoist("l", ["motto", "sway"]); hoist("r", ["motto", "sway"]); say("both sides see: motto sway"); });
		out(right, 200, [["", "Receiving holiday.mp4 (2.1 GB)."]]);
		for (var p = 0; p <= 100; p += 2.5) {
			(function (pct) {
				s.push([90, function () {
					right.live = html(progress("↓", pct, cols(R), true));
					left.live = html(progress("↑", pct, cols(L), true));
					sail(pct);
					say("crossing · " + Math.round(pct) + "%");
				}]);
			})(p);
		}
		out(right, 200, [["g", "✓"], ["", " Received 2.1 GB in 19s (110.5 MB/s). Checked: every byte matches."]]);
		then(0, function () { land(true); say("arrived · every byte checked"); });
		out(right, 60, [["", "holiday.mp4"]]);
		out(left, 0, [["g", "✓"], ["", " Sent 2.1 GB in 19s (110.5 MB/s, direct)."]]);

		type(left, 'ferry msg sam "it is the director\'s cut, enjoy"');
		out(left, 300, [["g", "✓"], ["", " Sent to sam, delivered."]]);
		then(0, function () { say("message delivered"); });
		type(right, "ferry inbox");
		out(right, 300, [["d", "#12 ← "], ["", "ana        "], ["d", "21:04"], ["", "  it is the director's cut, enjoy"]]);
		out(right, 60, [["d", "Next time: ferry inbox --since 12"]]);
		return s;
	}

	function play() {
		var me = ++run;
		left.clear(); right.clear();
		reset();
		var steps = script();
		if (reduce) {
			steps.forEach(function (st) { st[1](); });
			left.draw(false); right.draw(false);
			return;
		}
		var i = 0;
		(function next() {
			if (me !== run) return;
			if (i >= steps.length) { left.draw(true); right.draw(false); return; }
			var st = steps[i++];
			setTimeout(function () {
				if (me !== run) return;
				st[1]();
				left.draw(false); right.draw(false);
				next();
			}, st[0]);
		})();
	}

	document.getElementById("replay").addEventListener("click", play);

	// Start when the demo scrolls into view, once; with reduced motion, now.
	if (reduce || !("IntersectionObserver" in window)) {
		play(); // nothing moves, so show the finished picture straight away
	} else {
		var seen = false;
		new IntersectionObserver(function (es, obs) {
			if (!seen && es[0].isIntersecting) { seen = true; obs.disconnect(); play(); }
		}, { threshold: 0.3 }).observe(L);
	}

	// Copy buttons.
	Array.prototype.forEach.call(document.querySelectorAll("[data-copy]"), function (b) {
		b.addEventListener("click", function () {
			var t = document.getElementById(b.getAttribute("data-copy")).textContent;
			if (navigator.clipboard) {
				navigator.clipboard.writeText(t).then(function () {
					b.textContent = "copied";
					setTimeout(function () { b.textContent = "copy"; }, 1500);
				});
			}
		});
	});
})();
