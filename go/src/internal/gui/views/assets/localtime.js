// Reformat <time datetime="<RFC3339>" data-fmt="..."> elements using the
// browser's locale (inherited from the OS).
// Re-runs on htmx swaps and DOM mutations so SSE-injected and
// script-built nodes also get localized.
(function () {
	const styleMap = {
		date: { dateStyle: "short" },
		time: { timeStyle: "medium" },
		datetime: { dateStyle: "short", timeStyle: "short" },
		"datetime-seconds": { dateStyle: "short", timeStyle: "medium" },
	};
	const fmtCache = {};
	function formatter(key) {
		const k = styleMap[key] ? key : "datetime";
		if (!fmtCache[k]) {
			fmtCache[k] = new Intl.DateTimeFormat(undefined, styleMap[k]);
		}
		return fmtCache[k];
	}

	function localize(root) {
		if (!root || root.nodeType !== 1) return;
		const list =
			root.matches && root.matches("time[datetime]:not([data-ts-done])")
				? [root]
				: Array.from(
					root.querySelectorAll("time[datetime]:not([data-ts-done])"),
				);
		for (const el of list) {
			const d = new Date(el.dateTime);
			if (Number.isNaN(d.getTime())) continue;
			try {
				el.textContent = formatter(el.dataset.fmt).format(d);
				el.setAttribute("data-ts-done", "");
			} catch (_) {
				/* leave server fallback in place */
			}
		}
	}

	window.ptrackLocalizeTimes = localize;

	function runAll() {
		localize(document.body);
	}
	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", runAll);
	} else {
		runAll();
	}
	document.addEventListener("htmx:afterSwap", (e) => localize(e.target));
	new MutationObserver((muts) => {
		for (const m of muts) {
			for (const n of m.addedNodes) localize(n);
		}
	}).observe(document.body, { childList: true, subtree: true });
})();
