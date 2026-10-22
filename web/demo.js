// Replays a ferry session in the two terminals on the landing page.
// The lines are ferry's real output; only the timing is compressed.
(function () {
	"use strict";

	var L = document.getElementById("left");
	var R = document.getElementById("right");
	if (!L || !R) return;

	var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
	var run = 0;

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

	var left = new Term(L), right = new Term(R);
	var code = "7-acorn-tulip-otter";
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

	// Steps: [delay ms, function]. Typing is its own step kind.
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
		function say(t, delay, parts) { s.push([delay, function () { t.print(parts); }]); }

		type(left, "ferry send ./holiday.mp4");
		say(left, 250, [["", "Sending holiday.mp4 (2.1 GB)."]]);
		say(left, 120, [["", "On the other machine, run:"]]);
		say(left, 60, [["", ""]]);
		say(left, 60, [["", "    ferry get "], ["a", code]]);
		say(left, 60, [["", ""]]);
		say(left, 60, [["d", "Waiting for them… (Ctrl-C to cancel)"]]);

		type(right, "ferry get " + code);
		say(right, 600, [["", "Connected. "], ["d", "(both sides see: motto sway)"]]);
		say(left, 0, [["", "Connected. "], ["d", "(both sides see: motto sway)"]]);
		say(right, 200, [["", "Receiving holiday.mp4 (2.1 GB)."]]);
		for (var p = 0; p <= 100; p += 2.5) {
			(function (pct) {
				s.push([90, function () {
					right.live = html(progress(pct));
					left.live = html(bar(pct).concat([["", " ↑ " + ("    " + pct.toFixed(1)).slice(-5) + "%"]]));
				}]);
			})(p);
		}
		say(right, 200, [["g", "✓"], ["", " Received 2.1 GB in 19s (110.5 MB/s). Checked: every byte matches."]]);
		say(right, 60, [["", "holiday.mp4"]]);
		say(left, 0, [["g", "✓"], ["", " Sent 2.1 GB in 19s (110.5 MB/s, direct)."]]);

		type(left, 'ferry msg sam "it is the director\'s cut, enjoy"');
		say(left, 300, [["g", "✓"], ["", " Sent to sam, delivered."]]);
		type(right, "ferry inbox");
		say(right, 300, [["d", "#12 ← "], ["", "ana        "], ["d", "21:04"], ["", "  it is the director's cut, enjoy"]]);
		say(right, 60, [["d", "Next time: ferry inbox --since 12"]]);
		return s;
	}

	function play() {
		var me = ++run;
		left.clear(); right.clear();
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
