// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0
//
// The few browser-side behaviours htmx does not cover. Everything else is
// rendered by the server. No inline scripts or eval anywhere: the page runs
// under a strict Content-Security-Policy (script-src 'self').
(function () {
	"use strict";

	var root = document.documentElement;

	// Sidebar collapse is a per-browser preference.
	try {
		if (localStorage.getItem("cystemd.nav") === "collapsed") root.classList.add("nav-collapsed");
	} catch (e) { /* storage unavailable */ }

	function closest(el, sel) {
		return el && el.closest ? el.closest(sel) : null;
	}

	document.addEventListener("click", function (ev) {
		var t = ev.target;

		if (closest(t, "[data-nav-toggle]")) {
			ev.preventDefault();
			var collapsed = root.classList.toggle("nav-collapsed");
			try { localStorage.setItem("cystemd.nav", collapsed ? "collapsed" : "expanded"); } catch (e) { /* ignore */ }
			return;
		}

		var dismiss = closest(t, "[data-dismiss]");
		if (dismiss) {
			var target = closest(dismiss, dismiss.getAttribute("data-dismiss"));
			if (target) target.remove();
			return;
		}

		// Clicking a menu item closes its menu; clicking elsewhere closes all.
		var openMenus = document.querySelectorAll("details.menu[open]");
		for (var i = 0; i < openMenus.length; i++) {
			var m = openMenus[i];
			if (!m.contains(t) || closest(t, ".menu__item")) m.removeAttribute("open");
		}

		// Buttons can copy a value to the clipboard.
		var copy = closest(t, "[data-copy]");
		if (copy && navigator.clipboard) {
			navigator.clipboard.writeText(copy.getAttribute("data-copy"));
			toast("Copied to clipboard", "success");
		}
	});

	// Opening a menu closes the others.
	document.addEventListener("toggle", function (ev) {
		var el = ev.target;
		if (!el.matches || !el.matches("details.menu") || !el.open) return;
		var openMenus = document.querySelectorAll("details.menu[open]");
		for (var i = 0; i < openMenus.length; i++) {
			if (openMenus[i] !== el) openMenus[i].removeAttribute("open");
		}
	}, true);

	document.addEventListener("keydown", function (ev) {
		if (ev.key === "Escape") {
			var close = document.querySelector(".sliding-panel .sliding-panel__close");
			if (close) close.click();
			var openMenus = document.querySelectorAll("details.menu[open]");
			for (var i = 0; i < openMenus.length; i++) openMenus[i].removeAttribute("open");
		}
		// "/" focuses the search box, as in Argo CD.
		if (ev.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
			var search = document.querySelector("[data-search]");
			if (search) { ev.preventDefault(); search.focus(); }
		}
	});

	function toast(message, kind) {
		var box = document.getElementById("toasts");
		if (!box) return;
		var el = document.createElement("div");
		el.className = "toast toast--" + (kind || "success");
		var text = document.createElement("div");
		text.className = "toast__text";
		text.textContent = message;
		el.appendChild(text);
		box.appendChild(el);
		setTimeout(function () { el.remove(); }, kind === "error" ? 15000 : 7000);
	}

	// Connection problems never reach the server, so report them here.
	document.addEventListener("htmx:sendError", function () {
		toast("Cannot reach the CYSTEMD server. Check your connection.", "error");
	});

	// Log viewer: keep following the newest line unless the user scrolled up.
	document.addEventListener("htmx:sseBeforeMessage", function (ev) {
		var el = ev.target;
		if (el && el.classList && el.classList.contains("logs__content")) {
			el.dataset.stick = (el.scrollHeight - el.scrollTop - el.clientHeight < 40) ? "1" : "";
		}
	});
	document.addEventListener("htmx:sseMessage", function (ev) {
		var el = ev.target;
		if (el && el.classList && el.classList.contains("logs__content") && el.dataset.stick) {
			el.scrollTop = el.scrollHeight;
		}
	});

	// Toasts expire; remove them so the container stays small.
	document.addEventListener("htmx:oobAfterSwap", function (ev) {
		var el = ev.detail && ev.detail.target;
		if (!el || el.id !== "toasts") return;
		var items = el.querySelectorAll(".toast:not([data-timed])");
		for (var i = 0; i < items.length; i++) {
			(function (t) {
				t.setAttribute("data-timed", "1");
				setTimeout(function () { t.remove(); }, t.classList.contains("toast--error") ? 15000 : 7000);
			})(items[i]);
		}
	});
})();
