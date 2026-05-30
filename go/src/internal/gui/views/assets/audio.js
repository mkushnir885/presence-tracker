// Browser-side mic capture for auto-generated challenges: records short
// segments and POSTs each to /audio/segment, where the daemon runs ASR/LLM.
(function () {
	const stateEl = document.getElementById("audio-state");
	if (!stateEl) return;

	let micBtn = null;
	let statusEl = null;
	let deviceList = null;
	let deviceMenu = null;
	let labelGrant = "Grant microphone access";
	let labelMute = "Mute";
	let labelUnmute = "Unmute";
	const pollInterval = Math.max(parseInt(stateEl.dataset.pollInterval, 10) || 300, 30);
	const deviceKey = "ptrack.audio.deviceId";

	let stream = null;
	let recorder = null;
	let intervalHandle = null;
	let muted = false;
	let inFlight = false;
	let currentBlobs = [];

	const supportedMime = pickSupportedMime();

	// First container/codec this browser's MediaRecorder can actually produce.
	function pickSupportedMime() {
		const candidates = [
			"audio/webm;codecs=opus",
			"audio/webm",
			"audio/ogg;codecs=opus",
			"audio/mp4",
		];
		for (const m of candidates) {
			if (
				typeof MediaRecorder !== "undefined" &&
				MediaRecorder.isTypeSupported &&
				MediaRecorder.isTypeSupported(m)
			) {
				return m;
			}
		}
		return "";
	}

	function setStatus(text) {
		if (statusEl) statusEl.textContent = text;
	}

	function setMicState(state) {
		micBtn.dataset.state = state;
		const label =
			state === "muted" ? labelUnmute : state === "live" ? labelMute : labelGrant;
		micBtn.setAttribute("aria-label", label);
		micBtn.setAttribute("data-tooltip", label);
	}

	async function listDevices() {
		if (!deviceList) return;
		try {
			const devices = await navigator.mediaDevices.enumerateDevices();
			const inputs = devices.filter((d) => d.kind === "audioinput");
			deviceList.innerHTML = "";
			const saved = localStorage.getItem(deviceKey) || "";
			if (inputs.length === 0) {
				const li = document.createElement("li");
				li.className = "menu-empty text-muted";
				li.textContent = deviceList.dataset.emptyLabel || "No microphones found";
				deviceList.appendChild(li);
				return;
			}
			inputs.forEach((d) => {
				const li = document.createElement("li");
				const btn = document.createElement("button");
				btn.type = "button";
				const isActive = d.deviceId === saved;
				if (isActive) btn.classList.add("active");
				btn.textContent = d.label || "input (" + d.deviceId.slice(0, 6) + ")";
				btn.addEventListener("click", () => switchDevice(d.deviceId));
				li.appendChild(btn);
				deviceList.appendChild(li);
			});
		} catch (e) {
			console.warn("enumerateDevices failed", e);
		}
	}

	async function startStream(deviceId) {
		const constraints = {
			audio: deviceId ? { deviceId: { exact: deviceId } } : true,
		};
		stream = await navigator.mediaDevices.getUserMedia(constraints);
	}

	function startRecorder() {
		if (!stream) return;
		const opts = supportedMime ? { mimeType: supportedMime } : undefined;
		recorder = new MediaRecorder(stream, opts);
		currentBlobs = [];
		recorder.ondataavailable = (e) => {
			if (e.data && e.data.size > 0) currentBlobs.push(e.data);
		};
		recorder.onstop = onSegmentStop;
		recorder.start();
	}

	// Fires when the interval timer stops the recorder. Assemble the closed
	// segment, restart recording right away so no audio is lost, then upload.
	function onSegmentStop() {
		const blob = new Blob(currentBlobs, { type: supportedMime || "audio/webm" });
		startRecorder();
		if (muted) recorder.pause();
		if (blob.size === 0) return;
		postSegment(blob);
	}

	async function postSegment(blob) {
		// Skip if a previous upload is still running; segments never overlap.
		if (inFlight) return;
		inFlight = true;
		setStatus("uploading " + Math.round(blob.size / 1024) + " kB…");
		try {
			const resp = await fetch("/audio/segment", {
				method: "POST",
				headers: { "Content-Type": blob.type || "audio/webm" },
				body: blob,
			});
			const body = await resp.json().catch(() => ({}));
			renderResult(body);
		} catch (err) {
			setStatus("error: " + err);
		} finally {
			inFlight = false;
		}
	}

	function renderResult(body) {
		const now = new Date().toLocaleTimeString();
		switch (body.status) {
			case "generated":
				setStatus(
					"[" + now + "] generated " + (body.questions || 0) + " question(s)",
				);
				break;
			case "skipped":
				if (body.reason === "silence_or_too_short") {
					setStatus("[" + now + "] silent interval");
				} else {
					setStatus(
						"[" + now + "] holding (" + (body.words || 0) + "/" + (body.needed || 0) + " words)",
					);
				}
				break;
			case "failed":
				setStatus("[" + now + "] failed: " + (body.reason || "unknown"));
				break;
			default:
				setStatus("[" + now + "] " + JSON.stringify(body));
		}
	}

	// Stop the recorder every pollInterval seconds: MediaRecorder only emits a
	// complete, decodable file on stop, so this is what cuts each segment.
	function startInterval() {
		clearInterval(intervalHandle);
		intervalHandle = setInterval(() => {
			if (recorder && recorder.state === "recording") {
				recorder.stop();
			}
		}, pollInterval * 1000);
	}

	function setMuted(next) {
		muted = next;
		if (!recorder) return;
		if (muted && recorder.state === "recording") {
			recorder.pause();
			setMicState("muted");
			setStatus(labelUnmute);
		} else if (!muted && recorder.state === "paused") {
			recorder.resume();
			setMicState("live");
			setStatus("recording…");
		}
	}

	async function grant() {
		try {
			const saved = localStorage.getItem(deviceKey) || "";
			await startStream(saved);
			await listDevices();
			startRecorder();
			startInterval();
			setMicState("live");
			setStatus("recording…");
		} catch (e) {
			setMicState("idle");
			setStatus("permission denied: " + e);
		}
	}

	async function switchDevice(deviceId) {
		localStorage.setItem(deviceKey, deviceId);
		try {
			if (recorder && recorder.state !== "inactive") recorder.stop();
			if (stream) stream.getTracks().forEach((t) => t.stop());
			await startStream(deviceId);
			startRecorder();
			if (muted) recorder.pause();
			await listDevices();
			if (deviceMenu) deviceMenu.open = false;
		} catch (e) {
			setStatus("device change failed: " + e);
		}
	}

	// Resume capturing without a click if the user already granted the mic.
	async function tryAutoGrant() {
		if (!navigator.permissions || !navigator.permissions.query) return;
		try {
			const perm = await navigator.permissions.query({ name: "microphone" });
			if (perm.state === "granted") await grant();
		} catch (_) {
			/* ignore: not supported on this browser */
		}
	}

	let initialised = false;
	function init() {
		const btn = document.getElementById("audio-mic-btn");
		if (!btn || initialised) return;
		initialised = true;
		micBtn = btn;
		statusEl = document.getElementById("audio-status");
		deviceList = document.getElementById("audio-device-list");
		deviceMenu = deviceList && deviceList.closest("details.menu");
		labelGrant = micBtn.dataset.labelGrant || labelGrant;
		labelMute = micBtn.dataset.labelMute || labelMute;
		labelUnmute = micBtn.dataset.labelUnmute || labelUnmute;
		if (!supportedMime) {
			setStatus("MediaRecorder not supported in this browser");
			micBtn.disabled = true;
			return;
		}
		setMicState("idle");
		micBtn.addEventListener("click", () => {
			const state = micBtn.dataset.state;
			if (state === "idle") {
				grant();
			} else if (state === "live") {
				setMuted(true);
			} else {
				setMuted(false);
			}
		});
		if (deviceMenu) {
			deviceMenu.addEventListener("toggle", () => {
				if (deviceMenu.open) listDevices();
			});
		}
		tryAutoGrant();
	}

	// The Audio card may arrive via an htmx swap, so retry init after settle
	// if its button wasn't in the DOM on first load.
	init();
	if (!initialised) {
		document.body.addEventListener("htmx:afterSettle", init);
	}
})();
