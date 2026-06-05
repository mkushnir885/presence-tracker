// Browser-side glue for the challenger: captures mic audio, uploads short
// segments to /audio/segment, and surfaces auto-generated bank results.
(function () {
	const stateEl = document.getElementById("audio-state");
	if (!stateEl) return;

	const SEGMENT_S = Math.max(parseInt(stateEl.dataset.pollInterval, 10) || 300, 30);
	const DEVICE_KEY = "ptrack.audio.deviceId";

	const supportedMime =
		typeof MediaRecorder === "undefined" || !MediaRecorder.isTypeSupported
			? ""
			: [
				"audio/webm;codecs=opus",
				"audio/webm",
				"audio/ogg;codecs=opus",
				"audio/mp4",
			].find((m) => MediaRecorder.isTypeSupported(m)) || "";

	let stream = null;
	let recorder = null;
	let segmentTimer = null;
	let muted = false;
	let inFlight = false;
	let currentBlobs = [];

	async function openStream(deviceId) {
		stream = await navigator.mediaDevices.getUserMedia({
			audio: deviceId ? { deviceId: { exact: deviceId } } : true,
		});
	}

	function startRecorder() {
		if (!stream) return;
		recorder = new MediaRecorder(
			stream,
			supportedMime ? { mimeType: supportedMime } : undefined,
		);
		currentBlobs = [];
		recorder.ondataavailable = (e) => {
			if (e.data && e.data.size > 0) currentBlobs.push(e.data);
		};
		recorder.onstop = cycleRecorder;
		recorder.start();
		if (muted) recorder.pause();
	}

	function stopCapture() {
		if (recorder) {
			// Null the handler so teardown doesn't restart via cycleRecorder.
			recorder.onstop = null;
			if (recorder.state !== "inactive") recorder.stop();
		}
		if (stream) stream.getTracks().forEach((t) => t.stop());
		stream = null;
		recorder = null;
	}

	// Restart the same recorder so each segment is an independently decodable
	// WebM (the container only finalizes on stop, and each start() emits a
	// fresh header).
	function cycleRecorder() {
		const blob = new Blob(currentBlobs, { type: supportedMime || "audio/webm" });
		currentBlobs = [];
		recorder.start();
		if (muted) recorder.pause();
		if (blob.size > 0) postSegment(blob);
	}

	function startSegmentTimer() {
		clearInterval(segmentTimer);
		segmentTimer = setInterval(() => {
			// Fire even when paused (muted): the server skips silent segments
			// via the ASR silence floor, and ticking through mute keeps any
			// pre-mute audio from sitting in the buffer until unmute.
			if (recorder && recorder.state !== "inactive") recorder.stop();
		}, SEGMENT_S * 1000);
	}

	async function postSegment(blob) {
		if (inFlight) return; // segments never overlap
		inFlight = true;
		try {
			const resp = await fetch("/audio/segment", {
				method: "POST",
				headers: { "Content-Type": blob.type || "audio/webm" },
				body: blob,
			});
			handleResult(await resp.json().catch(() => ({})));
		} catch (_) {
			/* upload errors are logged server-side */
		} finally {
			inFlight = false;
		}
	}

	function handleResult(result) {
		if (!result) return;
		if (result.status === "generated" && result.auto_submit === false) {
			const questions = result.questions || 0;
			notifyDesktop(questions);
			document.body.dispatchEvent(
				new CustomEvent("ptrack:generated", { detail: { questions } }),
			);
		} else if (result.status === "failed") {
			notifyDesktopFail();
		}
	}

	function fireNotification(title, body, tag) {
		if (typeof Notification === "undefined") return;
		const fire = () => {
			try {
				new Notification(title, { body, tag, renotify: true });
			} catch (_) {
				/* some browsers throw outside a secure context */
			}
		};
		if (Notification.permission === "granted") fire();
		else if (Notification.permission !== "denied") {
			Notification.requestPermission().then((p) => p === "granted" && fire());
		}
	}

	function notifyDesktop(questions) {
		const title = stateEl.dataset.notifyTitle || "ptrack: questions ready";
		const body = (stateEl.dataset.notifyBody || "Generated %d question(s)")
			.replace("%d", questions);
		fireNotification(title, body, "ptrack-generated");
	}

	function notifyDesktopFail() {
		const title = stateEl.dataset.notifyFailTitle || "ptrack: generation failed";
		const body = stateEl.dataset.notifyFailBody || "Auto-generation failed";
		fireNotification(title, body, "ptrack-failed");
	}

	let micBtn = null;
	let deviceList = null;
	let deviceMenu = null;
	const labels = { grant: "Grant microphone access", mute: "Mute", unmute: "Unmute" };

	function setMicState(state) {
		micBtn.dataset.state = state;
		const label =
			state === "muted" ? labels.unmute :
			state === "live" ? labels.mute :
			labels.grant;
		micBtn.setAttribute("aria-label", label);
		micBtn.setAttribute("data-tooltip", label);
	}

	async function listDevices() {
		if (!deviceList) return;
		let inputs = [];
		try {
			const all = await navigator.mediaDevices.enumerateDevices();
			inputs = all.filter((d) => d.kind === "audioinput");
		} catch (_) { /* empty list */ }

		deviceList.replaceChildren();
		if (inputs.length === 0) {
			const li = document.createElement("li");
			li.className = "menu-empty text-muted";
			li.textContent = deviceList.dataset.emptyLabel || "";
			deviceList.appendChild(li);
			return;
		}
		const saved = localStorage.getItem(DEVICE_KEY) || "";
		inputs.forEach((d, i) => {
			const btn = document.createElement("button");
			btn.type = "button";
			if (d.deviceId === saved || (!saved && i === 0)) btn.classList.add("active");
			btn.textContent = d.label || "Microphone " + (i + 1);
			btn.addEventListener("click", () => switchDevice(d.deviceId));
			const li = document.createElement("li");
			li.appendChild(btn);
			deviceList.appendChild(li);
		});
	}

	async function grant() {
		try {
			await openStream(localStorage.getItem(DEVICE_KEY) || "");
			await listDevices();
			startRecorder();
			startSegmentTimer();
			setMicState(muted ? "muted" : "live");
		} catch (_) {
			setMicState("idle");
		}
	}

	async function switchDevice(deviceId) {
		localStorage.setItem(DEVICE_KEY, deviceId);
		stopCapture();
		await grant();
		if (deviceMenu) deviceMenu.open = false;
	}

	function setMuted(next) {
		muted = next;
		if (!recorder) return;
		setMicState(muted ? "muted" : "live");
		if (muted && recorder.state === "recording") recorder.pause();
		else if (!muted && recorder.state === "paused") recorder.resume();
	}

	async function tryAutoGrant() {
		if (!navigator.permissions || !navigator.permissions.query) return;
		try {
			const perm = await navigator.permissions.query({ name: "microphone" });
			if (perm.state === "granted") await grant();
		} catch (_) {
			/* permission API not supported */
		}
	}

	// Idempotent: the audio card may arrive via an htmx swap.
	let initialised = false;
	function init() {
		const btn = document.getElementById("audio-mic-btn");
		if (!btn || initialised) return;
		initialised = true;
		micBtn = btn;
		deviceList = document.getElementById("audio-device-list");
		deviceMenu = deviceList && deviceList.closest("details.menu");
		labels.grant = micBtn.dataset.labelGrant || labels.grant;
		labels.mute = micBtn.dataset.labelMute || labels.mute;
		labels.unmute = micBtn.dataset.labelUnmute || labels.unmute;
		if (!supportedMime) {
			micBtn.disabled = true;
			return;
		}
		setMicState("idle");
		micBtn.addEventListener("click", () => {
			const state = micBtn.dataset.state;
			if (state === "idle") grant();
			else setMuted(state === "live");
		});
		if (deviceMenu) {
			deviceMenu.addEventListener("toggle", () => {
				if (deviceMenu.open) listDevices();
			});
		}
		listDevices();
		if (navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
			navigator.mediaDevices.addEventListener("devicechange", listDevices);
		}
		tryAutoGrant();
	}

	init();
	if (!initialised) {
		document.body.addEventListener("htmx:afterSettle", init);
	}
})();
