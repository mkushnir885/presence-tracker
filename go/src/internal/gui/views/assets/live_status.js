// Live status page interactions, written so it does not have to be
// re-bound after every htmx full-body swap (the status page reloads
// itself every 5 s). Strategy:
//   * Use event delegation off document for inputs and clicks.
//   * On every DOMContentLoaded and htmx:afterSettle, re-apply the
//     roster filter (saved in localStorage) and rebuild the "chosen
//     file" chip if the picker still has a file selected.
//
// Surface:
//   * Tracked / untracked roster filter inputs — case-insensitive
//     substring of the display name.
//   * "Upload file" → "filename × clear" chip swap inside the trigger-
//     poll card.
//   * "Start a poll" multipart submission to /poll/file.

(function () {
	const FILTER_KEY = "ptrack.status.filter.";

	function applyFilter(card) {
		const input = card.querySelector(".roster-filter");
		if (!input) return;
		const q = input.value.trim().toLowerCase();
		const items = card.querySelectorAll(".roster-item");
		let visible = 0;
		items.forEach((li) => {
			const name = (li.dataset.name || li.textContent || "").toLowerCase();
			const show = q === "" || name.includes(q);
			li.hidden = !show;
			if (show) visible++;
		});
		const noMatches = card.querySelector(".roster-no-matches");
		const list = card.querySelector(".roster-list");
		if (noMatches && list) {
			const hasItems = items.length > 0;
			noMatches.hidden = !hasItems || visible !== 0;
			list.hidden = hasItems && visible === 0;
		}
	}

	function restoreFilters() {
		document.querySelectorAll(".roster-card").forEach((card) => {
			const kind = card.dataset.roster;
			const input = card.querySelector(".roster-filter");
			if (!input) return;
			try {
				const saved = localStorage.getItem(FILTER_KEY + kind) || "";
				if (saved && !input.value) input.value = saved;
			} catch (_) {
				/* private mode → ignore */
			}
			applyFilter(card);
		});
	}

	document.addEventListener("input", (ev) => {
		const input = ev.target.closest && ev.target.closest(".roster-filter");
		if (!input) return;
		const card = input.closest(".roster-card");
		if (!card) return;
		try {
			localStorage.setItem(FILTER_KEY + card.dataset.roster, input.value);
		} catch (_) {
			/* ignore */
		}
		applyFilter(card);
	});

	function uploadLabel(slot) {
		return slot.dataset.labelUpload || "Upload file";
	}

	function clearLabel(slot) {
		return slot.dataset.labelClear || "Remove file";
	}

	function renderUploadButton(slot, startBtn) {
		slot.innerHTML = "";
		const btn = document.createElement("button");
		btn.type = "button";
		btn.id = "poll-upload-btn";
		btn.className = "btn poll-upload-btn";
		btn.textContent = uploadLabel(slot);
		slot.appendChild(btn);
		if (startBtn) startBtn.disabled = true;
	}

	function renderFileChip(slot, startBtn, name) {
		slot.innerHTML = "";
		const chip = document.createElement("div");
		chip.className = "poll-file-chip";
		const nameEl = document.createElement("span");
		nameEl.className = "poll-file-name";
		nameEl.textContent = name;
		nameEl.title = name;
		const clear = document.createElement("button");
		clear.type = "button";
		clear.className = "icon-btn poll-file-clear";
		clear.setAttribute("aria-label", clearLabel(slot));
		clear.setAttribute("data-tooltip", clearLabel(slot));
		clear.dataset.action = "poll-clear";
		clear.innerHTML =
			'<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" ' +
			'fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
			'<line x1="18" y1="6" x2="6" y2="18"></line>' +
			'<line x1="6" y1="6" x2="18" y2="18"></line></svg>';
		chip.appendChild(nameEl);
		chip.appendChild(clear);
		slot.appendChild(chip);
		if (startBtn) startBtn.disabled = false;
	}

	function refreshPollCard() {
		const slot = document.getElementById("poll-file-slot");
		const picker = document.getElementById("poll-bank-picker");
		const startBtn = document.getElementById("poll-start-btn");
		if (!slot || !picker) return;
		// Capture the initial server-rendered label exactly once so the
		// JS-rebuilt button keeps the localized text.
		const initBtn = slot.querySelector("#poll-upload-btn");
		if (initBtn && !slot.dataset.labelUpload) {
			slot.dataset.labelUpload = initBtn.textContent.trim();
		}
		const hasFile = picker.files && picker.files[0];
		if (hasFile) {
			renderFileChip(slot, startBtn, picker.files[0].name);
		} else if (!slot.querySelector("#poll-upload-btn") && !slot.querySelector(".poll-file-chip")) {
			renderUploadButton(slot, startBtn);
		} else if (startBtn) {
			startBtn.disabled = !hasFile;
		}
	}

	document.addEventListener("click", (ev) => {
		const target = ev.target;
		if (!target || !target.closest) return;
		const uploadBtn = target.closest("#poll-upload-btn");
		if (uploadBtn) {
			const picker = document.getElementById("poll-bank-picker");
			if (picker) picker.click();
			return;
		}
		const clearBtn = target.closest('[data-action="poll-clear"]');
		if (clearBtn) {
			const picker = document.getElementById("poll-bank-picker");
			const startBtn = document.getElementById("poll-start-btn");
			const slot = document.getElementById("poll-file-slot");
			const fb = document.getElementById("poll-feedback");
			if (picker) picker.value = "";
			if (slot) renderUploadButton(slot, startBtn);
			if (fb) fb.textContent = "";
			return;
		}
		const startBtn = target.closest("#poll-start-btn");
		if (startBtn && !startBtn.disabled) {
			ev.preventDefault();
			submitPoll();
		}
	});

	document.addEventListener("change", (ev) => {
		if (ev.target && ev.target.id === "poll-bank-picker") refreshPollCard();
	});

	async function submitPoll() {
		const picker = document.getElementById("poll-bank-picker");
		const startBtn = document.getElementById("poll-start-btn");
		const slot = document.getElementById("poll-file-slot");
		const file = picker && picker.files && picker.files[0];
		if (!file) return;
		if (startBtn) startBtn.disabled = true;
		// Reset the picker immediately so the upload button swaps back
		// in; the captured `file` ref keeps the in-flight upload alive.
		if (picker) picker.value = "";
		if (slot) renderUploadButton(slot, startBtn);
		try {
			const form = new FormData();
			form.append("bank", file, file.name);
			await fetch("/poll/file", { method: "POST", body: form });
		} catch (_) {
			/* upload errors are surfaced in the system log */
		}
	}

	function onReady() {
		restoreFilters();
		refreshPollCard();
	}

	// connectStatusStream subscribes to the per-region SSE feed served
	// by ptrack. Each named event carries the fragment HTML for one
	// stable wrapper (matched via data-sse=<name>), so unchanged
	// regions are never touched and the audio + poll cards survive
	// across updates. The body event is reserved for the waiting↔live
	// phase swap; sub-region events handle everything else.
	function connectStatusStream() {
		if (typeof EventSource === "undefined") return;
		const es = new EventSource("/status/stream");
		const swap = (name) => (ev) => {
			document
				.querySelectorAll('[data-sse="' + name + '"]')
				.forEach((el) => {
					el.innerHTML = ev.data;
				});
			onReady();
		};
		es.addEventListener("started", swap("started"));
		es.addEventListener("roster", swap("roster"));
		es.addEventListener("log", swap("log"));
		es.addEventListener("pending", swap("pending"));
		es.addEventListener("body", (ev) => {
			const body = document.getElementById("status-body");
			if (body) body.innerHTML = ev.data;
			onReady();
			// Audio + roster filter init normally piggyback on htmx
			// settle; dispatch the same event so they re-bind to the
			// fresh nodes the phase swap just introduced.
			document.body.dispatchEvent(new Event("htmx:afterSettle"));
		});
		es.addEventListener("session-ended", () => {
			es.close();
			window.location.href = "/";
		});
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", onReady);
		document.addEventListener("DOMContentLoaded", connectStatusStream);
	} else {
		onReady();
		connectStatusStream();
	}
	document.body.addEventListener("htmx:afterSettle", onReady);
})();
