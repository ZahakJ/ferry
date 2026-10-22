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
		return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
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

	var strait = document.getElementById("strait");
	var boat = document.getElementById("boat");
	var wake = document.getElementById("wake");
	var crate = document.getElementById("crate");
	var status = document.getElementById("status");
	var hoists = { l: document.getElementById("hoist-l"), r: document.getElementById("hoist-r") };
	var flagClass = ["f-yellow", "f-red", "f-blue"];
	var dock = { from: 214, to: 708 }; // boat x beside each pier

	function sail(pct) {
		if (!boat) return;
		var x = dock.from + (dock.to - dock.from) * pct / 100;
		boat.setAttribute("transform", "translate(" + x.toFixed(1) + " 0)");
		wake.setAttribute("d", "M214 140 L" + x.toFixed(1) + " 140");
		strait.classList.toggle("moving", pct > 0 && pct < 100);
	}

	function say(text) { if (status) status.textContent = text; }

	// hoist strings words as pennants on the line from a shore's mast.
	function hoist(side, words) {
		var h = hoists[side];
		if (!h) return;
		var g = h.querySelector(".flags");
		while (g.firstChild) g.removeChild(g.firstChild);
		var x0 = side === "l" ? 40 : 960, dir = side === "l" ? 1 : -1;
		words.forEach(function (w, i) {
			var x = x0 + dir * (22 + i * 34), y = 16 + (x - x0) * dir * 0.36;
			var f = document.createElementNS(SVG, "path");
			f.setAttribute("class", flagClass[i % 3]);
			f.setAttribute("d", "M" + (x - 9) + " " + y + " L" + (x + 9) + " " + (y + 6.5) + " L" + x + " " + (y + 22) + " Z");
			g.appendChild(f);
			var t = document.createElementNS(SVG, "text");
			t.setAttribute("x", x);
			t.setAttribute("y", y + 36);
			t.setAttribute("text-anchor", "middle");
			t.textContent = w;
			g.appendChild(t);
		});
		h.classList.toggle("up", words.length > 0);
	}

	function reset() {
		sail(0);
		hoist("l", []);
		hoist("r", []);
		if (crate) crate.classList.remove("landed");
		say("at the pier");
	}

	// ---- the script ----

	var left = new Term(L), right = new Term(R);
	var code = "7-acorn-tulip-otter";
	var words = code.split("-").slice(1);
	var total = 2.1;

	function bar(pct) {
		var w = 24, n = Math.round(pct / 100 * w);
		return [["g", "━".repeat(n)], ["d", "━".repeat(w - n)]];
	}

	function progress(pct) {
		var done = (total * pct / 100).toFixed(1);
		var rate = (108 + Math.round(Math.sin(pct / 9) * 6)).toFixed(1);
		var left = Math.max(0, Math.round((100 - pct) / 100 * 19));
		var tail = pct < 100 ? "  " + rate + " MB/s · " + left + "s left" : "";
		var p = ("    " + pct.toFixed(1)).slice(-5);
		return bar(pct).concat([["", " ↓ " + p + "%  " + done + " GB of " + total + " GB" + tail]]);
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
					right.live = html(progress(pct));
					left.live = html(bar(pct).concat([["", " ↑ " + ("    " + pct.toFixed(1)).slice(-5) + "%"]]));
					sail(pct);
					say("crossing · " + Math.round(pct) + "%");
				}]);
			})(p);
		}
		out(right, 200, [["g", "✓"], ["", " Received 2.1 GB in 19s (110.5 MB/s). Checked: every byte matches."]]);
		then(0, function () { if (crate) crate.classList.add("landed"); say("arrived · every byte checked"); });
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

	// Start when the demo scrolls into view, once.
	if ("IntersectionObserver" in window) {
		var seen = false;
		new IntersectionObserver(function (es, obs) {
			if (!seen && es[0].isIntersecting) { seen = true; obs.disconnect(); play(); }
		}, { threshold: 0.3 }).observe(L);
	} else {
		play();
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
